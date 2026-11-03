package diff

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

// render writes hunks as git's unified hunks, to compare with git's output.
func render(hunks []Hunk) string {
	var b strings.Builder
	for _, h := range hunks {
		fmt.Fprintf(&b, "@@ -%d,%d +%d,%d @@", h.OldStart, h.OldLines, h.NewStart, h.NewLines)
		if h.Header != "" {
			b.WriteString(" " + h.Header)
		}
		b.WriteString("\n")
		for _, l := range h.Lines {
			p := map[string]string{KindContext: " ", KindAdd: "+", KindDelete: "-"}[l.Kind]
			b.WriteString(p + l.Text + "\n")
		}
	}
	return b.String()
}

func TestCompareMatchesGit(t *testing.T) {
	old := "package main\n\nimport \"fmt\"\n\nfunc main() {\n\tfmt.Println(\"a\")\n\tfmt.Println(\"b\")\n\tfmt.Println(\"c\")\n}\n\nfunc other() {\n\treturn\n}\n"
	new := strings.Replace(old, "\tfmt.Println(\"b\")\n", "\tfmt.Println(\"B\")\n\tfmt.Println(\"b2\")\n", 1)
	hunks, add, del, bin, trunc := Compare([]byte(old), []byte(new), MaxFileLines)
	// git diff --no-index -U3 of the same two files.
	want := "@@ -4,7 +4,8 @@ import \"fmt\"\n \n func main() {\n \tfmt.Println(\"a\")\n-\tfmt.Println(\"b\")\n+\tfmt.Println(\"B\")\n+\tfmt.Println(\"b2\")\n \tfmt.Println(\"c\")\n }\n \n"
	if got := render(hunks); got != want || add != 2 || del != 1 || bin || trunc {
		t.Fatalf("got +%d -%d binary=%v truncated=%v\n%s\nwant\n%s", add, del, bin, trunc, got, want)
	}
	l := hunks[0].Lines[3]
	if l.Kind != KindDelete || *l.Old != 7 || l.New != nil {
		t.Fatalf("the deleted line = %+v", l)
	}
	l = hunks[0].Lines[5]
	if l.Kind != KindAdd || l.Old != nil || *l.New != 8 {
		t.Fatalf("the second added line = %+v", l)
	}
}

func TestCompareEdges(t *testing.T) {
	// A new file is one hunk from line 0 of an empty side.
	hunks, add, del, _, _ := Compare(nil, []byte("a\nb\n"), MaxFileLines)
	if got := render(hunks); got != "@@ -0,0 +1,2 @@\n+a\n+b\n" || add != 2 || del != 0 {
		t.Fatalf("added file:\n%s", got)
	}
	hunks, _, del, _, _ = Compare([]byte("a\nb"), nil, MaxFileLines)
	if got := render(hunks); got != "@@ -1,2 +0,0 @@\n-a\n-b\n" || del != 2 {
		t.Fatalf("deleted file:\n%s", got)
	}
	// Two changes far apart are two hunks; near, one.
	var lines []string
	for i := 1; i <= 30; i++ {
		lines = append(lines, fmt.Sprint(i))
	}
	a := strings.Join(lines, "\n") + "\n"
	b := strings.Replace(strings.Replace(a, "\n2\n", "\n2x\n", 1), "\n28\n", "\n28x\n", 1)
	if hunks, _, _, _, _ = Compare([]byte(a), []byte(b), MaxFileLines); len(hunks) != 2 || hunks[1].OldStart != 25 {
		t.Fatalf("far changes:\n%s", render(hunks))
	}
	b = strings.Replace(strings.Replace(a, "\n2\n", "\n2x\n", 1), "\n8\n", "\n8x\n", 1)
	if hunks, _, _, _, _ = Compare([]byte(a), []byte(b), MaxFileLines); len(hunks) != 1 {
		t.Fatalf("near changes:\n%s", render(hunks))
	}
	// Binary on either side has no hunks.
	if hunks, _, _, bin, _ := Compare([]byte("a\x00"), []byte("b"), MaxFileLines); !bin || hunks != nil {
		t.Fatal("binary")
	}
	// The line cap truncates what is carried, never the counts.
	hunks, add, _, _, trunc := Compare(nil, []byte(strings.Repeat("x\n", 20)), 5)
	if !trunc || add != 20 || len(hunks) != 1 || len(hunks[0].Lines) != 5 || hunks[0].NewLines != 5 {
		t.Fatalf("capped: +%d truncated=%v %s", add, trunc, render(hunks))
	}
}

// lcs is the longest common subsequence's length, by dynamic programming.
func lcs(a, b []string) int {
	dp := make([][]int, len(a)+1)
	for i := range dp {
		dp[i] = make([]int, len(b)+1)
	}
	for i := len(a) - 1; i >= 0; i-- {
		for j := len(b) - 1; j >= 0; j-- {
			if a[i] == b[j] {
				dp[i][j] = dp[i+1][j+1] + 1
			} else {
				dp[i][j] = max(dp[i+1][j], dp[i][j+1])
			}
		}
	}
	return dp[0][0]
}

// TestScriptIsMinimalAndApplies checks random edits: the script rebuilds
// b from a, and its edits are as few as the LCS allows.
func TestScriptIsMinimalAndApplies(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	word := func() string { return string(rune('a' + rng.Intn(4))) }
	for n := 0; n < 500; n++ {
		var a, b []string
		for i := rng.Intn(30); i > 0; i-- {
			a = append(a, word())
		}
		for i := rng.Intn(30); i > 0; i-- {
			b = append(b, word())
		}
		ops := script(a, b)
		var rebuilt []string
		edits, ai := 0, 0
		for _, o := range ops {
			switch o.kind {
			case opEq:
				if a[o.a] != b[o.b] || o.a != ai {
					t.Fatalf("%v -> %v: bad eq %+v", a, b, o)
				}
				ai++
				rebuilt = append(rebuilt, a[o.a])
			case opDel:
				if o.a != ai {
					t.Fatalf("%v -> %v: out of order %+v", a, b, o)
				}
				ai++
				edits++
			case opIns:
				rebuilt = append(rebuilt, b[o.b])
				edits++
			}
		}
		if strings.Join(rebuilt, ",") != strings.Join(b, ",") || ai != len(a) {
			t.Fatalf("%v -> %v rebuilt %v", a, b, rebuilt)
		}
		if want := len(a) + len(b) - 2*lcs(a, b); edits != want {
			t.Fatalf("%v -> %v: %d edits, minimal is %d", a, b, edits, want)
		}
		// The hunks number every line as its side has it.
		hunks, _ := group(ops, a, b, Context, MaxFileLines)
		for _, h := range hunks {
			for _, l := range h.Lines {
				if l.Old != nil && a[*l.Old-1] != l.Text || l.New != nil && b[*l.New-1] != l.Text {
					t.Fatalf("%v -> %v: line %+v", a, b, l)
				}
			}
		}
	}
}

func TestScriptPastTheBoundIsStillCorrect(t *testing.T) {
	ops, ok := myers([]int{0, 1, 2, 3}, []int{4, 5, 6, 7}, 2)
	if ok || ops != nil {
		t.Fatal("a distance of 8 is past 2")
	}
}
