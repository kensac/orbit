package runner

import (
	"bufio"
	"bytes"
	"context"
	"io"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
)

type expiryChatBoxClient struct {
	expires time.Time
}

func (*expiryChatBoxClient) CreateProfile(context.Context, string, string, io.Reader) (sandbox.CreateResponse, error) {
	return sandbox.CreateResponse{}, io.ErrUnexpectedEOF
}

func (*expiryChatBoxClient) Attach(context.Context, string, sandbox.AttachRequest, io.Reader, io.Writer, io.Writer) (int, error) {
	return -1, io.ErrUnexpectedEOF
}

func (*expiryChatBoxClient) Archive(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func (*expiryChatBoxClient) Destroy(context.Context, string) error { return nil }

func (c *expiryChatBoxClient) CreateChatBox(_ context.Context, name, _ string, _ io.Reader) (sandbox.CreateResponse, error) {
	return sandbox.CreateResponse{ID: name, ExpiresAt: c.expires}, nil
}

func (c *expiryChatBoxClient) AttachChatBox(_ context.Context, _ string, _ sandbox.ChatBoxAttachRequest, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	if _, err := bufio.NewReader(stdin).ReadString('\n'); err != nil {
		return -1, err
	}
	for _, line := range []string{
		`{"type":"system","subtype":"init","session_id":"session-1"}`,
		`{"type":"assistant","message":{"content":[{"type":"text","text":"hello"}]}}`,
		`{"type":"result","subtype":"success","is_error":false,"result":"hello","session_id":"session-1","total_cost_usd":0.01}`,
	} {
		if _, err := io.WriteString(stdout, line+"\n"); err != nil {
			return -1, err
		}
	}
	return 0, nil
}

func (c *expiryChatBoxClient) ArchiveChatBox(context.Context, string) (io.ReadCloser, error) {
	return io.NopCloser(bytes.NewReader(nil)), nil
}

func TestPersistentChatBoxSetsLiveProcessHardExpiry(t *testing.T) {
	expires := time.Now().Add(30 * time.Minute)
	client := &expiryChatBoxClient{expires: expires}
	live := NewLiveWithPressure(1, nil, 0)
	defer live.Close()
	sess := &Session{Home: t.TempDir(), KeepDays: 1, Live: live, Key: "conversation", Open: true, LiveIdle: time.Hour}
	r, err := New("cc", spec.RunnerSpec{Kind: "claude-code", Binary: "claude", Model: "sonnet"}, lookup(map[string]string{
		"CLAUDE_CODE_OAUTH_TOKEN": "credential",
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = r.Run(context.Background(), Request{
		Workdir: t.TempDir(),
		Session: sess,
		Chat: &Chat{Level: spec.LevelLocked, Sandbox: &Sandbox{
			Client: client, Label: "orbit-chat-test", Profile: "workstation", Persistent: true,
			RunID: "123e4567-e89b-42d3-a456-426614174000", Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "credential"},
			Pack:   func() ([]byte, error) { return []byte("workspace"), nil },
			Unpack: func(io.Reader) error { return nil },
		}},
		Prompt: "hello",
	}, func(Event) {})
	if err != nil {
		t.Fatal(err)
	}
	if !sess.LiveUntil.Equal(expires) {
		t.Fatalf("LiveUntil = %s, want %s", sess.LiveUntil, expires)
	}
	live.mu.Lock()
	p := live.procs[sess.Key]
	live.mu.Unlock()
	if p == nil {
		t.Fatal("chat process was not kept live")
	}
	p.timerMu.Lock()
	hardTimer := p.hardTimer
	p.timerMu.Unlock()
	if hardTimer == nil {
		t.Fatal("persistent chat process has no hard credential expiry")
	}
}
