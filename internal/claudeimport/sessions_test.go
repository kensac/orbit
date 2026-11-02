package claudeimport

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/store"
)

// Synthetic session lines in Claude Code's shape. Nothing here is from a real session.
const (
	sessA = "11111111-1111-4111-8111-111111111111"
	sessB = "22222222-2222-4222-8222-222222222222"
)

func userLine(session, uuid, parent, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "user", "sessionId": session, "uuid": uuid, "parentUuid": parent,
		"cwd": "/home/p/work/widget", "gitBranch": "main", "timestamp": "2026-10-01T10:00:00.000Z",
		"message": map[string]any{"role": "user", "content": text}})
	return string(b)
}

func assistantLine(session, uuid, parent, text string) string {
	b, _ := json.Marshal(map[string]any{"type": "assistant", "sessionId": session, "uuid": uuid, "parentUuid": parent,
		"timestamp": "2026-10-01T10:00:05.000Z",
		"message":   map[string]any{"role": "assistant", "content": []any{map[string]any{"type": "text", "text": text}}}})
	return string(b)
}

func TestNormalizeLineFoldsForksAndScrubs(t *testing.T) {
	a, err := NormalizeLine([]byte(userLine(sessA, "u1", "", "set GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB please")))
	if err != nil {
		t.Fatal(err)
	}
	b, err := NormalizeLine([]byte(userLine(sessB, "u1", "", "set GITHUB_TOKEN=ghp_0123456789abcdefghijklmnopqrstuvwxyzAB please")))
	if err != nil {
		t.Fatal(err)
	}
	if string(a) != string(b) {
		t.Fatalf("a fork's copy of a line differs from its parent's:\n%s\n%s", a, b)
	}
	if strings.Contains(string(a), "ghp_") || strings.Contains(string(a), "sessionId") {
		t.Fatalf("stored line keeps a credential or its session id: %s", a)
	}
	if again, _ := NormalizeLine(a); string(again) != string(a) {
		t.Fatalf("NormalizeLine is not idempotent:\n%s\n%s", a, again)
	}
	if !strings.Contains(string(a), `"cwd":"/home/p/work/widget"`) || strings.Contains(string(a), `\u003c`) {
		t.Fatalf("stored line = %s", a)
	}
	r, err := RestoreLine(a, sessB)
	if err != nil || !strings.Contains(string(r), `"sessionId":"`+sessB+`"`) {
		t.Fatalf("restore = %s %v", r, err)
	}
	if _, err := NormalizeLine([]byte(`[1,2]`)); err == nil {
		t.Fatal("an array line was accepted")
	}
}

func TestAnOversizedLineKeepsItsSkeleton(t *testing.T) {
	big := strings.Repeat("x", MaxSessionLine+10)
	b, _ := json.Marshal(map[string]any{"type": "user", "uuid": "u2", "parentUuid": "a1", "toolUseResult": big,
		"message": map[string]any{"role": "user", "content": []any{
			map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": big}}}})
	out, err := NormalizeLine(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(out) > 4096 {
		t.Fatalf("cut line is %d bytes", len(out))
	}
	var l struct {
		UUID    string `json:"uuid"`
		Parent  string `json:"parentUuid"`
		Omitted int    `json:"orbitOmittedBytes"`
		Message struct {
			Content []map[string]any `json:"content"`
		} `json:"message"`
	}
	if err := json.Unmarshal(out, &l); err != nil {
		t.Fatal(err)
	}
	if l.UUID != "u2" || l.Parent != "a1" || l.Omitted < MaxSessionLine || len(l.Message.Content) != 1 ||
		l.Message.Content[0]["tool_use_id"] != "toolu_1" || l.Message.Content[0]["type"] != "tool_result" {
		t.Fatalf("skeleton = %s", out)
	}
}

func TestParseLineMeta(t *testing.T) {
	m := ParseLineMeta([]byte(userLine(sessA, "u1", "", "Fix the flaky test")))
	if m.Type != "user" || m.Cwd != "/home/p/work/widget" || m.Branch != "main" || m.Prompt != "Fix the flaky test" || m.At == nil {
		t.Fatalf("meta = %+v", m)
	}
	if m := ParseLineMeta([]byte(userLine(sessA, "u1", "", "<command-name>/clear</command-name>"))); m.Prompt != "" {
		t.Fatalf("a wrapped command is a prompt: %+v", m)
	}
	if m := ParseLineMeta([]byte(`{"type":"summary","summary":"Flaky test fixed","leafUuid":"a1"}`)); m.Title != "Flaky test fixed" {
		t.Fatalf("summary meta = %+v", m)
	}
	tool := `{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"ok"}]}}`
	if m := ParseLineMeta([]byte(tool)); m.Prompt != "" {
		t.Fatalf("a tool result is a prompt: %+v", m)
	}
}

func TestPlanSession(t *testing.T) {
	f := SessionFile{Path: "x.jsonl", SessionID: sessA}
	l1, l2 := userLine(sessA, "u1", "", "one"), assistantLine(sessA, "a1", "u1", "two")
	file := []byte(l1 + "\n" + l2 + "\n" + `{"type":"user","partial`)
	p := PlanSession(f, file, nil)
	if p.Outcome != store.SessionCreated || len(p.Lines) != 2 || p.Ends[1] != int64(len(l1)+len(l2)+2) || p.Cwd != "/home/p/work/widget" {
		t.Fatalf("new = %+v", p)
	}
	first := int64(len(l1) + 1)
	st := &ImportState{Offset: first, PrefixSHA256: PrefixSHA256(file, first)}
	if p := PlanSession(f, file, st); p.Outcome != store.SessionAppended || len(p.Lines) != 1 || p.From != first {
		t.Fatalf("append = %+v", p)
	}
	st = &ImportState{Offset: p.Ends[1], PrefixSHA256: PrefixSHA256(file, p.Ends[1])}
	if p := PlanSession(f, file, st); p.Outcome != store.SessionUnchanged || len(p.Lines) != 0 {
		t.Fatalf("unchanged = %+v", p)
	}
	st = &ImportState{Offset: first, PrefixSHA256: strings.Repeat("0", 64)}
	if p := PlanSession(f, file, st); p.Outcome != store.SessionRewritten || !p.Rewrite || len(p.Lines) != 2 || p.From != 0 {
		t.Fatalf("rewrite = %+v", p)
	}
	if p := PlanSession(f, []byte(`{"partial`), nil); p.Outcome != PlanEmpty {
		t.Fatalf("empty = %+v", p)
	}
}

func TestFindSessionsAndRepoOf(t *testing.T) {
	dir := t.TempDir()
	proj := filepath.Join(dir, "-home-p-work-widget")
	for _, p := range []string{proj, filepath.Join(proj, sessA, "subagents")} {
		if err := os.MkdirAll(p, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	for _, p := range []string{filepath.Join(proj, sessA+".jsonl"), filepath.Join(dir, sessB+".jsonl"),
		filepath.Join(proj, sessA, "subagents", "agent-1.jsonl"), filepath.Join(proj, "notes.jsonl")} {
		if err := os.WriteFile(p, []byte("{}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	got, err := FindSessions(dir)
	if err != nil || len(got) != 2 {
		t.Fatalf("sessions = %v %v", got, err)
	}

	repo := filepath.Join(dir, "repo")
	_ = os.MkdirAll(filepath.Join(repo, ".git", "worktrees", "wt"), 0o755)
	_ = os.WriteFile(filepath.Join(repo, ".git", "config"), []byte("[core]\n\tbare = false\n[remote \"origin\"]\n\turl = git@github.com:someone/widget.git\n"), 0o644)
	_ = os.WriteFile(filepath.Join(repo, ".git", "worktrees", "wt", "commondir"), []byte("../..\n"), 0o644)
	wt := filepath.Join(dir, "wt")
	_ = os.MkdirAll(wt, 0o755)
	_ = os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: "+filepath.Join(repo, ".git", "worktrees", "wt")+"\n"), 0o644)
	if r := RepoOf(repo); r != "someone/widget" {
		t.Fatalf("repo = %q", r)
	}
	if r := RepoOf(wt); r != "someone/widget" {
		t.Fatalf("worktree repo = %q", r)
	}
	if r := RepoOf(filepath.Join(dir, "gone")); r != "" {
		t.Fatalf("missing repo = %q", r)
	}
	if r := repoName("https://user:secret@git.example.com/x/y.git"); r != "https://git.example.com/x/y.git" {
		t.Fatalf("repo name = %q", r)
	}
}
