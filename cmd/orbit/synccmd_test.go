package main

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// Synthetic skills and entries only.

func skillMD(name, step string) string {
	return "---\nname: " + name + "\ndescription: Use when cutting a release.\n---\n" + step + "\n"
}

var planRE = regexp.MustCompile(`plan ([0-9a-f]{64})`)

func runSync(t *testing.T, url string, args ...string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := syncCmd(context.Background(), append(append([]string{"claude"}, args...), "--api", url, "--token", "op-token"), &out)
	return out.String(), err
}

func planOf(t *testing.T, out string) string {
	t.Helper()
	m := planRE.FindStringSubmatch(out)
	if m == nil {
		t.Fatalf("no plan digest in:\n%s", out)
	}
	return m[1]
}

// testAPI is orbit's internal API over s with a minimal configuration, taking the operator token "op-token".
func testAPI(t *testing.T, s *store.Store) *httptest.Server {
	t.Helper()
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "agents"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "orbit.yaml"), []byte("defaults: {runner: r, channels: [c]}\nrunners: {r: {kind: claude-code}}\nchannels: {c: {kind: log}}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := spec.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	e := &engine.Engine{Store: s, ConfigDir: dir, Log: log, Env: func(string) (string, bool) { return "", false }}
	if err := e.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer((&api.Server{Store: s, Engine: e, Token: "op-token", Log: log}).InternalHandler())
	t.Cleanup(srv.Close)
	return srv
}

func TestSyncClaudeWritesOnlyOverOrbitsOwnVersions(t *testing.T) {
	ctx := context.Background()
	s := testdb.New(t)
	srv := testAPI(t, s)
	person := store.SkillProvenance{Source: store.SkillByPerson}
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "cut-release", Content: skillMD("cut-release", "1. make release"),
		Files: []store.SkillFile{{Path: "references/notes.md", Content: []byte("notes")}}}, person); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "mine", Content: skillMD("mine", "orbit's")}, person); err != nil {
		t.Fatal(err)
	}
	entry, err := s.CreateEntry(ctx, store.NewEntry{EntryRef: store.EntryRef{Scope: store.ScopeProfile}, Key: "squash-merges",
		EntryFields: store.EntryFields{Type: "feedback", Title: "Squash merges", Description: "Squash every PR", Body: "Squash-merge PRs."}},
		store.Provenance{Source: store.SourcePerson})
	if err != nil {
		t.Fatal(err)
	}

	skills, memory := t.TempDir(), t.TempDir()
	// A skill of the person's own under orbit's name: never overwritten.
	if err := os.MkdirAll(filepath.Join(skills, "mine"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "mine", "SKILL.md"), []byte(skillMD("mine", "my own edit")), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(memory, "MEMORY.md"), []byte("- [Other](other.md) - not orbit's\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := []string{"--skills-dir", skills, "--memory-dir", memory, "--scope", "profile"}

	out, err := runSync(t, srv.URL, args...)
	if err != nil || !strings.Contains(out, "2 to create, 0 to update, 0 to remove, 0 unchanged, 1 changed locally") {
		t.Fatalf("dry run = %v\n%s", err, out)
	}
	digest := planOf(t, out)
	if _, err := os.Stat(filepath.Join(skills, "cut-release")); err == nil {
		t.Fatal("a dry run wrote a skill")
	}
	if _, err := runSync(t, srv.URL, append(args, "--apply", strings.Repeat("0", 64))...); err == nil || !strings.Contains(err.Error(), "no longer") {
		t.Fatalf("apply with another digest = %v", err)
	}
	if out, err := runSync(t, srv.URL, append(args, "--apply", digest)...); err != nil {
		t.Fatalf("apply = %v\n%s", err, out)
	}
	if b, _ := os.ReadFile(filepath.Join(skills, "cut-release", "references", "notes.md")); string(b) != "notes" {
		t.Fatalf("skill file = %q", b)
	}
	if b, _ := os.ReadFile(filepath.Join(skills, "mine", "SKILL.md")); !strings.Contains(string(b), "my own edit") {
		t.Fatalf("the person's own skill was overwritten: %s", b)
	}
	index, _ := os.ReadFile(filepath.Join(memory, "MEMORY.md"))
	if string(index) != "- [Other](other.md) - not orbit's\n- [Squash merges](squash-merges.md) - Squash every PR\n" {
		t.Fatalf("MEMORY.md =\n%s", index)
	}

	// orbit moves on: the local copies are its older versions, so they update; an entry edited locally is left alone.
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "cut-release", Content: skillMD("cut-release", "1. make release\n2. tag"),
		Files: []store.SkillFile{{Path: "references/notes.md", Content: []byte("notes")}}}, person); err != nil {
		t.Fatal(err)
	}
	body := "Squash-merge PRs, always."
	if _, err := s.UpdateEntry(ctx, entry.ID, 0, store.EntryEdit{Body: &body}, store.Provenance{Source: store.SourcePerson}); err != nil {
		t.Fatal(err)
	}
	out, err = runSync(t, srv.URL, args...)
	if err != nil || !strings.Contains(out, "0 to create, 2 to update") {
		t.Fatalf("second dry run = %v\n%s", err, out)
	}
	digest = planOf(t, out)
	// A local edit after the dry run: the plan no longer matches, so nothing is written.
	mem := filepath.Join(memory, "squash-merges.md")
	b, _ := os.ReadFile(mem)
	edited := strings.Replace(string(b), "Squash-merge PRs.", "Squash-merge PRs, mine.", 1)
	if err := os.WriteFile(mem, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := runSync(t, srv.URL, append(args, "--apply", digest)...); err == nil {
		t.Fatal("a plan that changed was applied")
	}
	if b, _ := os.ReadFile(filepath.Join(skills, "cut-release", "SKILL.md")); strings.Contains(string(b), "2. tag") {
		t.Fatal("a write from a stale plan went through")
	}
	out, _ = runSync(t, srv.URL, args...)
	if !strings.Contains(out, "0 to create, 1 to update, 0 to remove, 0 unchanged, 2 changed locally") || !strings.Contains(out, "squash-merges.md") {
		t.Fatalf("third dry run =\n%s", out)
	}
	if _, err := runSync(t, srv.URL, append(args, "--apply", planOf(t, out))...); err != nil {
		t.Fatal(err)
	}
	if b, _ := os.ReadFile(filepath.Join(skills, "cut-release", "SKILL.md")); !strings.Contains(string(b), "2. tag") {
		t.Fatalf("skill not updated: %s", b)
	}
	if b, _ := os.ReadFile(mem); !strings.Contains(string(b), "mine") {
		t.Fatalf("a locally edited memory was overwritten: %s", b)
	}

	// A retired skill's local copy goes, since orbit holds it.
	if _, err := s.RetireSkill(ctx, "cut-release", 0, person); err != nil {
		t.Fatal(err)
	}
	out, _ = runSync(t, srv.URL, args...)
	if _, err := runSync(t, srv.URL, append(args, "--apply", planOf(t, out))...); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(skills, "cut-release")); !os.IsNotExist(err) {
		t.Fatalf("retired skill still on disk: %v", err)
	}
	if ents, _ := os.ReadDir(skills); len(ents) != 1 {
		t.Fatalf("skills dir holds %d entries, want only the person's own", len(ents))
	}
}
