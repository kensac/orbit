package orbitskills_test

import (
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/orbitskills"
	"github.com/kensac/orbit/internal/store"
)

// TestTheBuiltInSkillsAreWellFormed: each is a skill the library would take but for its reserved name: frontmatter naming it with a description, references each with a heading.
func TestTheBuiltInSkillsAreWellFormed(t *testing.T) {
	want := []string{orbitskills.Actions, orbitskills.Calc, orbitskills.Continuations, orbitskills.Entries, orbitskills.Query}
	if got := orbitskills.Names(); strings.Join(got, ",") != strings.Join(want, ",") {
		t.Fatalf("names = %v", got)
	}
	for _, n := range want {
		k, ok := orbitskills.Get(n)
		if !ok || !orbitskills.Has(n) {
			t.Fatalf("%s is missing", n)
		}
		if d, err := store.SkillDescription(n, k.Content); err != nil || len(d) < 40 {
			t.Errorf("%s: description %q, %v", n, d, err)
		}
		for _, f := range k.Files {
			if !strings.HasPrefix(f.Path, "references/") || !strings.HasPrefix(string(f.Content), "# ") {
				t.Errorf("%s: %s is not a reference with a heading", n, f.Path)
			}
		}
	}
	if orbitskills.Has("close") {
		t.Error("an unknown name is built in")
	}
}
