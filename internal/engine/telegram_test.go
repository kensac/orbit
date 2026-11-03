package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

type telegramRecorder struct{ messages []notify.Message }

func (r *telegramRecorder) Send(_ context.Context, m notify.Message) error {
	r.messages = append(r.messages, m)
	return nil
}

func TestTelegramMessageAlwaysGetsDirectReply(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint("failed=", fail), func(t *testing.T) {
			s := testdb.New(t)
			dir := t.TempDir()
			_ = os.MkdirAll(filepath.Join(dir, "agents"), 0o755)
			orbit := "defaults: {runner: fake}\nrunners: {fake: {kind: claude-code}}\nchannels: {tg: {kind: telegram, token_env: BOT}}\ntelegram: {channel: tg, agent: assistant, allow_users: [9]}\n"
			if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte(orbit), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "agents", "assistant.yaml"), []byte("name: assistant\ncharter: x\nnotify: {when: never}\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := spec.Load(dir)
			if err != nil {
				t.Fatal(err)
			}
			if err = s.SyncAgents(context.Background(), map[string]store.AgentSpec{"assistant": {SHA: cfg.Agents["assistant"].SHA, JSON: mustJSON(t, cfg.Agents["assistant"]), Enabled: true}}); err != nil {
				t.Fatal(err)
			}
			recorder := &telegramRecorder{}
			fr := &fakeRunner{output: `{"summary":"Hi","report":"This is the reply","notify":{"send":false,"priority":"default","title":""},"memory":{"update":false,"content":""},"questions":[],"actions":[]}`}
			if fail {
				fr.err = errors.New("temporary backend failure")
			}
			e := &engine.Engine{Store: s, DataDir: t.TempDir(), ConfigDir: dir, Secret: []byte("secret"), Env: func(string) (string, bool) { return "bot-token", true }, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), NewRunner: func(string, spec.RunnerSpec, func(string) (string, bool)) (runner.Runner, error) { return fr, nil }, NewChannel: func(string, spec.Channel, notify.Env, *http.Client) (notify.Channel, error) { return recorder, nil }}
			if err = e.SetConfig(cfg); err != nil {
				t.Fatal(err)
			}
			in, _, err := s.IngestTelegram(context.Background(), store.TelegramIn{UpdateID: 1, ChatID: 77, UserID: 9, Agent: "assistant", SpecSHA: cfg.Agents["assistant"].SHA, Text: "hello", Title: "Telegram"})
			if err != nil {
				t.Fatal(err)
			}
			claimed, err := s.ClaimRun(context.Background(), "test")
			if err != nil || claimed == nil {
				t.Fatalf("claim=%+v err=%v", claimed, err)
			}
			if err = e.Execute(context.Background(), claimed); err != nil {
				t.Fatal(err)
			}
			if len(recorder.messages) != 1 || recorder.messages[0].TelegramChatID != 77 {
				t.Fatalf("messages=%+v", recorder.messages)
			}
			run, _ := s.GetRun(context.Background(), in.RunID)
			want := "succeeded"
			if fail {
				want = "failed"
			}
			if run.Status != want {
				t.Fatalf("status=%s want=%s", run.Status, want)
			}
		})
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
