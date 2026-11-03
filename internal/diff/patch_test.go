package diff

import (
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"
)

// A patch written from Compare's hunks parses back to the same hunks, line
// numbers included.
func TestParsePatchReadsWhatCompareWrites(t *testing.T) {
	rng := rand.New(rand.NewSource(2))
	for n := 0; n < 300; n++ {
		var a, b []string
		for i := rng.Intn(40); i > 0; i-- {
			a = append(a, fmt.Sprint(rng.Intn(6)))
		}
		for i := rng.Intn(40); i > 0; i-- {
			b = append(b, fmt.Sprint(rng.Intn(6)))
		}
		old, new := strings.Join(a, "\n")+"\n", strings.Join(b, "\n")+"\n"
		if len(a) == 0 {
			old = ""
		}
		if len(b) == 0 {
			new = ""
		}
		want, add, del, _, _ := Compare([]byte(old), []byte(new), MaxFileLines)
		got, gadd, gdel, trunc, err := ParsePatch(render(want), MaxFileLines)
		if err != nil || trunc || gadd != add || gdel != del {
			t.Fatalf("%q -> %q: +%d -%d %v %v", old, new, gadd, gdel, trunc, err)
		}
		if len(want) == 0 && len(got) == 0 {
			continue
		}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("%q -> %q:\n%s\nparsed back as\n%s", old, new, render(want), render(got))
		}
	}
}

func TestParsePatchAsGitHubSendsIt(t *testing.T) {
	patch := "@@ -1 +1,2 @@ func x() {\n-a\n+b\n+c\n\\ No newline at end of file\n@@ -10,0 +12 @@\n+z"
	hunks, add, del, trunc, err := ParsePatch(patch, MaxFileLines)
	if err != nil || add != 3 || del != 1 || trunc || len(hunks) != 2 {
		t.Fatalf("+%d -%d %v %v %s", add, del, trunc, err, render(hunks))
	}
	h := hunks[0]
	if h.Header != "func x() {" || h.OldStart != 1 || h.OldLines != 1 || h.NewLines != 2 || *h.Lines[2].New != 2 {
		t.Fatalf("first = %+v", h)
	}
	if h := hunks[1]; h.OldStart != 10 || h.OldLines != 0 || h.NewStart != 12 || *h.Lines[0].New != 12 || h.Lines[0].Old != nil {
		t.Fatalf("second = %+v", h)
	}
	if _, add, _, trunc, _ := ParsePatch(patch, 2); !trunc || add != 3 {
		t.Fatal("the line cap truncates, the counts stay whole")
	}
	for _, bad := range []string{"@@ -x +1 @@\n+a", "@@ -1 +1\n+a", "@@ -1 +1 @@\n?a"} {
		if _, _, _, _, err := ParsePatch(bad, MaxFileLines); err == nil {
			t.Fatalf("%q parsed", bad)
		}
	}
}
