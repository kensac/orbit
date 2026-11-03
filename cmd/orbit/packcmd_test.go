package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

func TestPackInspectionRoundTripAndTamperDetection(t *testing.T) {
	dir := t.TempDir()
	specBytes := []byte(`{"name":"ledger","description":"Review ledger","charter":"Read and summarize.","actions":{"notify":"allow"},"sources":[{"name":"bank","kind":"http","url":"https://example.test","headers":{"Authorization":"Bearer ${BANK_TOKEN}"}}]}`)
	files := []packFile{{".claude-plugin/plugin.json", []byte(pluginManifest("ledger", "Review ledger"))}, {"agents/ledger.md", []byte(pluginAgent("ledger", "Review ledger", "Read and summarize."))}, {"README.md", []byte(packReadme("ledger"))}, {"skills/reconcile/SKILL.md", []byte("---\nname: reconcile\ndescription: Reconcile ledger rows\n---\nUse source rows.\n")}, {"skills/reconcile/references/fields.md", []byte("Field reference\n")}}
	manifest := packManifest{Format: packFormat, Version: 1, Agent: specBytes, AgentSHA256: digest(specBytes), Enabled: true, Environment: []string{"BANK_TOKEN"}, Skills: []packSkill{{Name: "reconcile", Version: 2, SHA256: store.SkillDigest(string(files[3].data), []store.SkillFile{{Path: "references/fields.md", Content: files[4].data}}), Files: []packDigest{{Path: "skills/reconcile/SKILL.md", SHA256: digest(files[3].data)}, {Path: "skills/reconcile/references/fields.md", SHA256: digest(files[4].data)}}}}}
	for _, f := range files {
		manifest.Files = append(manifest.Files, packDigest{f.path, digest(f.data)})
	}
	b, _ := json.Marshal(manifest)
	files = append(files, packFile{"orbit.json", b})
	out := filepath.Join(dir, "plugin")
	if err := publishPack(out, files); err != nil {
		t.Fatal(err)
	}
	got, err := inspectPack(out)
	if err != nil {
		t.Fatal(err)
	}
	if got.Format != packFormat || len(got.Skills) != 1 || got.Skills[0].Name != "reconcile" {
		t.Fatalf("unexpected inspected manifest: %+v", got)
	}
	if err := os.WriteFile(filepath.Join(out, "README.md"), []byte("modified"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectPack(out); err == nil {
		t.Fatal("tampered file passed inspection")
	}
	if err := os.Symlink("README.md", filepath.Join(out, "skills", "reconcile", "linked.md")); err != nil {
		t.Fatal(err)
	}
	if _, err := inspectPack(out); err == nil {
		t.Fatal("symlink passed inspection")
	}
}

func TestPackInspectionRejectsUnsafeInventory(t *testing.T) {
	for _, p := range []string{"../escape", "a/../b", "/absolute", "C:/escape", "a\\b"} {
		if _, err := safeRelative(p); err == nil {
			t.Errorf("accepted unsafe path %q", p)
		}
	}
	var a spec.Agent
	if err := json.Unmarshal([]byte(`{"name":"x","sources":[{"name":"s","kind":"http","headers":{"Authorization":"Bearer clear-text"}}]}`), &a); err != nil {
		t.Fatal(err)
	}
	if err := rejectInlineCredentials(&a); err == nil {
		t.Fatal("inline credential accepted")
	}
	if allowedPackPath("hooks/hooks.json", json.RawMessage(`{"name":"ledger"}`)) {
		t.Fatal("active Claude Code hook accepted")
	}
	if allowedPackPath(".mcp.json", json.RawMessage(`{"name":"ledger"}`)) {
		t.Fatal("implicit MCP configuration accepted")
	}
}
