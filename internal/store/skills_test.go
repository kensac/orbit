package store_test

import (
	"errors"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

const skillMD = "---\nname: close\ndescription: Use to close a month.\n---\n\nSteps.\n"

func byPerson() store.SkillProvenance {
	return store.SkillProvenance{Source: store.SkillByPerson, Author: "kanishk"}
}

func TestSkillVersions(t *testing.T) {
	s := testdb.New(t)
	files := []store.SkillFile{{Path: "references/a.md", Content: []byte("A")}}
	k, changed, err := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: skillMD, Files: files, Create: true}, byPerson())
	if err != nil || !changed || k.Version != 1 || k.Description != "Use to close a month." || len(k.FilePaths) != 1 {
		t.Fatalf("create = %+v %v %v", k, changed, err)
	}
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: skillMD, Create: true}, byPerson()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("create twice = %v", err)
	}
	// The same content and files is no new version.
	if k, changed, _ := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: skillMD, Files: files}, byPerson()); changed || k.Version != 1 {
		t.Fatalf("unchanged put = %+v %v", k, changed)
	}
	k2, changed, err := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: skillMD + "More.\n", Expect: 1}, byPerson())
	if err != nil || !changed || k2.Version != 2 || len(k2.FilePaths) != 0 {
		t.Fatalf("v2 = %+v %v", k2, err)
	}
	if _, _, err := s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: skillMD, Expect: 1}, byPerson()); !errors.Is(err, store.ErrConflict) {
		t.Fatalf("stale put = %v", err)
	}
	r, err := s.RestoreSkill(ctx, "close", 1, 2, byPerson())
	if err != nil || r.Version != 3 || *r.RestoredFrom != 1 || len(r.FilePaths) != 1 {
		t.Fatalf("restore = %+v %v", r, err)
	}
	if full, _ := s.GetSkill(ctx, "close", 0, true); full.Content != skillMD || string(full.Files[0].Content) != "A" {
		t.Fatalf("restored content = %+v", full)
	}
	if _, err := s.RetireSkill(ctx, "close", 3, byPerson()); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.SkillsByName(ctx, []string{"close"}); len(got) != 0 {
		t.Fatal("a retired skill is still given to runs")
	}
	if vs, _ := s.SkillVersions(ctx, "close"); len(vs) != 4 {
		t.Fatalf("versions = %d", len(vs))
	}
}

func TestSkillValidation(t *testing.T) {
	s := testdb.New(t)
	bad := []store.SkillWrite{
		{Name: "Bad", Content: skillMD},
		{Name: "close", Content: "no frontmatter"},
		{Name: "close", Content: "---\nname: close\n---\nno description"},
		{Name: "other", Content: skillMD},
		{Name: "close", Content: skillMD, Files: []store.SkillFile{{Path: "../x"}}},
		{Name: "close", Content: skillMD, Files: []store.SkillFile{{Path: "/etc/x"}}},
		{Name: "close", Content: skillMD, Files: []store.SkillFile{{Path: "SKILL.md"}}},
		{Name: "close", Content: skillMD, Files: []store.SkillFile{{Path: "a/./b"}}},
	}
	for i, w := range bad {
		if _, _, err := s.PutSkill(ctx, w, byPerson()); !errors.Is(err, store.ErrInvalid) {
			t.Errorf("%d: %v, want ErrInvalid", i, err)
		}
	}
}

func TestSkillImport(t *testing.T) {
	s := testdb.New(t)
	p := store.SkillProvenance{Source: store.SkillByImport}
	in := []store.SkillWrite{{Name: "close", Content: skillMD}}
	if r, err := s.ImportSkills(ctx, in, p, true); err != nil || r[0].Outcome != "created" {
		t.Fatalf("dry = %+v %v", r, err)
	}
	if l, _ := s.ListSkills(ctx, "all"); len(l) != 0 {
		t.Fatal("a dry run wrote a skill")
	}
	s.ImportSkills(ctx, in, p, false)
	if r, _ := s.ImportSkills(ctx, in, p, false); r[0].Outcome != "unchanged" {
		t.Fatalf("again = %+v", r)
	}
	in[0].Content += "x\n"
	if r, _ := s.ImportSkills(ctx, in, p, false); r[0].Outcome != "updated" || r[0].Version != 2 {
		t.Fatalf("changed = %+v", r)
	}
}

func TestAgentSkillWrites(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "a")
	s.PutSkill(ctx, store.SkillWrite{Name: "close", Content: skillMD, Files: []store.SkillFile{{Path: "f.md", Content: []byte("F")}}}, byPerson())
	manual(t, s, "a")
	writes := []*store.AgentSkillWrite{
		{Name: "close", Content: skillMD + "Better.\n", Expect: 1, Note: "improved"},
		{Name: "fresh", Content: "---\nname: fresh\ndescription: New.\n---\nGo.\n"},
		{Name: "close", Content: skillMD, Expect: 1},
		{Name: "taken", Content: "---\nname: taken\ndescription: t\n---\n"},
	}
	s.PutSkill(ctx, store.SkillWrite{Name: "taken", Content: "---\nname: taken\ndescription: t\n---\nx"}, byPerson())
	runID := finishWith(t, s, store.Outcome{Skills: writes})
	want := []string{"written", "written", "refused", "refused"}
	for i, w := range writes {
		if w.Result.Outcome != want[i] {
			t.Fatalf("write %d = %+v, want %s", i, w.Result, want[i])
		}
	}
	k, _ := s.GetSkill(ctx, "close", 0, true)
	if k.Version != 2 || k.Source != store.SkillByAgent || *k.SourceRunID != runID || len(k.Files) != 1 {
		t.Fatalf("agent's write = %+v (the skill's files must stay)", k)
	}
	if names, _ := s.AttachedNames(ctx, "a", store.AttachSkill); len(names) != 2 {
		t.Fatalf("attached = %v", names)
	}
}
