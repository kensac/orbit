package claudeimport

import (
	"context"
	"slices"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

// A skills directory unpacked from a tar made on macOS holds an AppleDouble file beside every file and folder, and Finder's .DS_Store: none of them is a skill file.
func TestReadSkillsDirSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	skill := "---\nname: close\ndescription: Use when closing a month.\n---\n\nClose it.\n"
	write(t, dir, "close/SKILL.md", skill)
	write(t, dir, "close/references/format.md", "Format.\n")
	write(t, dir, "close/._SKILL.md", "\x00\x05\x16\x07Mac OS X")
	write(t, dir, "close/references/._format.md", "\x00\x05\x16\x07Mac OS X")
	write(t, dir, "close/.DS_Store", "\x00\x00\x00\x01Bud1")
	write(t, dir, "close/references/Thumbs.db", "x")
	write(t, dir, "close/._references", "\x00\x05\x16\x07")
	write(t, dir, "close/__MACOSX/references/._format.md", "x")
	write(t, dir, "._close", "\x00\x05\x16\x07")
	write(t, dir, ".DS_Store", "x")
	// A folder macOS made beside the skills is not a skill to warn about.
	write(t, dir, "__MACOSX/close/._SKILL.md", "x")
	write(t, dir, "._other/SKILL.md", skill)

	skills, warnings, err := ReadSkillsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(skills) != 1 || len(warnings) != 0 {
		t.Fatalf("skills %+v, warnings %v", skills, warnings)
	}
	k := skills[0]
	var paths []string
	for _, f := range k.Files {
		paths = append(paths, f.Path)
	}
	if k.Name != "close" || k.Content != skill || !slices.Equal(paths, []string{"references/format.md"}) {
		t.Fatalf("skill %s, files %v", k.Name, paths)
	}

	// A hidden file a person wrote is kept: only known droppings are skipped.
	write(t, dir, "close/.env.example", "KEY=\n")
	k, err = ReadSkill(dir + "/close")
	if err != nil {
		t.Fatal(err)
	}
	if len(k.Files) != 2 || k.Files[0].Path != ".env.example" {
		t.Fatalf("files %+v", k.Files)
	}
}

func TestReadMemoryDirSkipsJunk(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "tea.md", "---\nname: tea\ndescription: Prefers tea\ntype: feedback\n---\n\nOffer tea.\n")
	write(t, dir, "._tea.md", "\x00\x05\x16\x07Mac OS X")
	write(t, dir, "._MEMORY.md", "\x00\x05\x16\x07Mac OS X")
	write(t, dir, ".DS_Store", "x")
	mems, warnings, err := ReadMemoryDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(mems) != 1 || mems[0].Key != "tea" || len(warnings) != 0 {
		t.Fatalf("memories %+v, warnings %v", mems, warnings)
	}
}

func TestJunk(t *testing.T) {
	for name, want := range map[string]bool{
		"._SKILL.md": true, ".DS_Store": true, "__MACOSX": true, "Thumbs.db": true, "desktop.ini": true, ".Spotlight-V100": true,
		"SKILL.md": false, ".env.example": false, "_partial.md": false, ".gitignore": false, "notes._md": false,
	} {
		if Junk(name) != want {
			t.Errorf("Junk(%q) = %v", name, !want)
		}
	}
}

// A skill stored with a macOS tar's droppings is imported again from the same directory as a version without them.
func TestReimportDropsStoredJunk(t *testing.T) {
	s := testdb.New(t)
	ctx := context.Background()
	dir := t.TempDir()
	skill := "---\nname: close\ndescription: Use when closing a month.\n---\n\nClose it.\n"
	write(t, dir, "close/SKILL.md", skill)
	write(t, dir, "close/references/format.md", "Format.\n")
	write(t, dir, "close/._SKILL.md", "\x00\x05\x16\x07Mac OS X")
	write(t, dir, "close/.DS_Store", "x")
	p := store.SkillProvenance{Source: store.SkillByImport}
	// What an importer that kept every file stored.
	before := []store.SkillWrite{{Name: "close", Content: skill, Files: []store.SkillFile{
		{Path: "._SKILL.md", Content: []byte("\x00\x05\x16\x07Mac OS X")}, {Path: ".DS_Store", Content: []byte("x")},
		{Path: "references/format.md", Content: []byte("Format.\n")}}}}
	if _, err := s.ImportSkills(ctx, before, p, false); err != nil {
		t.Fatal(err)
	}
	skills, _, err := ReadSkillsDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	r, err := s.ImportSkills(ctx, []store.SkillWrite{{Name: skills[0].Name, Content: skills[0].Content, Files: skills[0].Files}}, p, false)
	if err != nil || r[0].Outcome != "updated" || r[0].Version != 2 {
		t.Fatalf("reimport = %+v %v", r, err)
	}
	k, err := s.GetSkill(ctx, "close", 0, false)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(k.FilePaths, []string{"references/format.md"}) {
		t.Fatalf("stored files %v", k.FilePaths)
	}
	// And again changes nothing.
	if r, _ := s.ImportSkills(ctx, []store.SkillWrite{{Name: "close", Content: skills[0].Content, Files: skills[0].Files}}, p, false); r[0].Outcome != "unchanged" {
		t.Fatalf("third import = %+v", r)
	}
}
