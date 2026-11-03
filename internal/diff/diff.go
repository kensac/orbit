// Package diff computes line diffs and parses unified patches into one
// shape, a diff file (docs/api.md, Workspace, A reply's changes): files,
// hunks, and lines with their numbers on each side, so a diff view and the
// comments on it anchor to exact lines. Code computes every number here.
package diff

import (
	"bytes"
	"strings"
	"unicode/utf8"
)

// Caps on one diff response (docs/workspace.md, Auth, scope and limits).
const (
	// MaxFiles is the most files a response lists.
	MaxFiles = 300
	// MaxBytes bounds the text of every line of a response together.
	MaxBytes = 10 << 20
	// MaxFileLines is the most lines one file's hunks carry.
	MaxFileLines = 5000
	// Context is the unchanged lines shown around a change, as git's
	// default.
	Context = 3
)

// The statuses of a diff file.
const (
	Added    = "added"
	Modified = "modified"
	Deleted  = "deleted"
	Renamed  = "renamed"
	Mode     = "mode"
)

// File is one file's diff.
type File struct {
	Path      string  `json:"path"`
	OldPath   *string `json:"old_path"`
	Status    string  `json:"status"`
	Binary    bool    `json:"binary"`
	Additions int     `json:"additions"`
	Deletions int     `json:"deletions"`
	Truncated bool    `json:"truncated"`
	Hunks     []Hunk  `json:"hunks"`
}

// Hunk is a run of changed lines with their context.
type Hunk struct {
	OldStart int    `json:"old_start"`
	OldLines int    `json:"old_lines"`
	NewStart int    `json:"new_start"`
	NewLines int    `json:"new_lines"`
	Header   string `json:"header"`
	Lines    []Line `json:"lines"`
}

// Line is one line of a hunk: Old and New are its 1-based numbers on each
// side, null on the side it is not on.
type Line struct {
	Kind string `json:"kind"`
	Old  *int   `json:"old"`
	New  *int   `json:"new"`
	Text string `json:"text"`
}

// The kinds of a line.
const (
	KindContext = "context"
	KindAdd     = "add"
	KindDelete  = "delete"
)

// Bytes is the text a file's hunks carry, what MaxBytes counts.
func (f *File) Bytes() int {
	n := 0
	for _, h := range f.Hunks {
		for _, l := range h.Lines {
			n += len(l.Text)
		}
	}
	return n
}

// binarySniff is how much of a file git reads to call it binary.
const binarySniff = 8000

// IsBinary reports whether git would call b binary: a NUL in its first
// 8000 bytes.
func IsBinary(b []byte) bool {
	return bytes.IndexByte(b[:min(len(b), binarySniff)], 0) >= 0
}

// Compare is the diff file of one path from old to new content. A binary
// side gives a file without hunks; maxLines caps the lines its hunks carry
// (Truncated set), while Additions and Deletions always count the whole
// change.
func Compare(old, new []byte, maxLines int) (hunks []Hunk, additions, deletions int, binary, truncated bool) {
	if IsBinary(old) || IsBinary(new) {
		return nil, 0, 0, true, false
	}
	a, b := SplitLines(old), SplitLines(new)
	ops := script(a, b)
	for _, o := range ops {
		switch o.kind {
		case opDel:
			deletions++
		case opIns:
			additions++
		}
	}
	hunks, truncated = group(ops, a, b, Context, maxLines)
	return hunks, additions, deletions, false, truncated
}

// Count is Compare's line counts alone, without building hunks.
func Count(old, new []byte) (additions, deletions int, binary bool) {
	if IsBinary(old) || IsBinary(new) {
		return 0, 0, true
	}
	for _, o := range script(SplitLines(old), SplitLines(new)) {
		switch o.kind {
		case opDel:
			deletions++
		case opIns:
			additions++
		}
	}
	return additions, deletions, false
}

// SplitLines splits text into lines without their newlines; a final line
// without one is a line, an empty text has none.
func SplitLines(b []byte) []string {
	if len(b) == 0 {
		return nil
	}
	s := strings.Split(string(b), "\n")
	if s[len(s)-1] == "" {
		s = s[:len(s)-1]
	}
	return s
}

const (
	opEq = iota
	opDel
	opIns
)

// op is one step of an edit script: a line of a (eq, del) or of b (ins),
// by index.
type op struct {
	kind int
	a, b int
}

// script is a shortest edit script from a to b: the common prefix and
// suffix, and Myers' algorithm between them. Past a bound on the edit
// distance (scaled so one diff stays a bounded amount of work) the middle
// is all deleted and all added, which is a correct diff, only not minimal.
func script(a, b []string) []op {
	pre := 0
	for pre < len(a) && pre < len(b) && a[pre] == b[pre] {
		pre++
	}
	suf := 0
	for suf < len(a)-pre && suf < len(b)-pre && a[len(a)-1-suf] == b[len(b)-1-suf] {
		suf++
	}
	ids := map[string]int{}
	intern := func(lines []string) []int {
		out := make([]int, len(lines))
		for i, l := range lines {
			id, ok := ids[l]
			if !ok {
				id = len(ids)
				ids[l] = id
			}
			out[i] = id
		}
		return out
	}
	ma, mb := intern(a[pre:len(a)-suf]), intern(b[pre:len(b)-suf])
	ops := make([]op, 0, len(a)+len(b)-pre-suf)
	for i := 0; i < pre; i++ {
		ops = append(ops, op{opEq, i, i})
	}
	mid, ok := myers(ma, mb, maxEdits(len(ma)+len(mb)))
	if !ok {
		mid = mid[:0]
		for i := range ma {
			mid = append(mid, op{opDel, i, 0})
		}
		for j := range mb {
			mid = append(mid, op{opIns, len(ma), j})
		}
	}
	for _, o := range mid {
		o.a += pre
		o.b += pre
		ops = append(ops, o)
	}
	for i := 0; i < suf; i++ {
		ops = append(ops, op{opEq, len(a) - suf + i, len(b) - suf + i})
	}
	return ops
}

// maxEdits bounds Myers' search to about 20 million steps.
func maxEdits(n int) int {
	return max(20, min(2000, 20_000_000/(n+1)))
}

// myers finds a shortest edit script with Myers' O(ND) algorithm, keeping
// each round's furthest reaching paths to walk back from the end. ok is
// false when the distance exceeds maxD.
func myers(a, b []int, maxD int) ([]op, bool) {
	n, m := len(a), len(b)
	if n == 0 && m == 0 {
		return nil, true
	}
	off := maxD + 1
	v := make([]int, 2*maxD+3)
	var trace [][]int
	for d := 0; d <= maxD; d++ {
		// v before this round, for k in [-d-1, d+1], indexed k+d+1.
		trace = append(trace, append([]int(nil), v[off-d-1:off+d+2]...))
		for k := -d; k <= d; k += 2 {
			var x int
			if k == -d || (k != d && v[off+k-1] < v[off+k+1]) {
				x = v[off+k+1]
			} else {
				x = v[off+k-1] + 1
			}
			y := x - k
			for x < n && y < m && a[x] == b[y] {
				x++
				y++
			}
			v[off+k] = x
			if x >= n && y >= m {
				return backtrack(trace, n, m), true
			}
		}
	}
	return nil, false
}

func backtrack(trace [][]int, x, y int) []op {
	var rev []op
	for d := len(trace) - 1; d >= 0; d-- {
		v := trace[d]
		at := func(k int) int { return v[k+d+1] }
		k := x - y
		var pk int
		if k == -d || (k != d && at(k-1) < at(k+1)) {
			pk = k + 1
		} else {
			pk = k - 1
		}
		px := at(pk)
		py := px - pk
		for x > px && y > py {
			x--
			y--
			rev = append(rev, op{opEq, x, y})
		}
		if d > 0 {
			if x == px {
				rev = append(rev, op{opIns, px, py})
			} else {
				rev = append(rev, op{opDel, px, py})
			}
		}
		x, y = px, py
	}
	for i, j := 0, len(rev)-1; i < j; i, j = i+1, j-1 {
		rev[i], rev[j] = rev[j], rev[i]
	}
	return rev
}

// group makes hunks of an edit script: each change with ctx lines around
// it, changes closer than 2*ctx in one hunk. At most maxLines lines are
// carried (truncated reports more).
func group(ops []op, a, b []string, ctx, maxLines int) (hunks []Hunk, truncated bool) {
	lines := 0
	for i := 0; i < len(ops); {
		if ops[i].kind == opEq {
			i++
			continue
		}
		start := max(0, i-ctx)
		j := i
		var end int
		for {
			for j < len(ops) && ops[j].kind != opEq {
				j++
			}
			k := j
			for k < len(ops) && ops[k].kind == opEq {
				k++
			}
			if k < len(ops) && k-j <= 2*ctx {
				j = k
				continue
			}
			end = min(j+ctx, len(ops))
			break
		}
		h, n := hunk(ops[start:end], a, b, maxLines-lines)
		lines += n
		if len(h.Lines) > 0 {
			hunks = append(hunks, h)
		}
		if n < end-start {
			return hunks, true
		}
		i = end
	}
	return hunks, false
}

// hunk is one hunk of ops, carrying at most limit lines; n is how many it
// carries.
func hunk(ops []op, a, b []string, limit int) (Hunk, int) {
	// The lines of each side before the hunk.
	oldBefore, newBefore := ops[0].a, ops[0].b
	var h Hunk
	for _, o := range ops {
		if len(h.Lines) >= limit {
			break
		}
		switch o.kind {
		case opEq:
			ol, nl := o.a+1, o.b+1
			h.Lines = append(h.Lines, Line{Kind: KindContext, Old: &ol, New: &nl, Text: a[o.a]})
			h.OldLines++
			h.NewLines++
		case opDel:
			ol := o.a + 1
			h.Lines = append(h.Lines, Line{Kind: KindDelete, Old: &ol, Text: a[o.a]})
			h.OldLines++
		case opIns:
			nl := o.b + 1
			h.Lines = append(h.Lines, Line{Kind: KindAdd, New: &nl, Text: b[o.b]})
			h.NewLines++
		}
	}
	h.OldStart, h.NewStart = oldBefore, newBefore
	if h.OldLines > 0 {
		h.OldStart++
	}
	if h.NewLines > 0 {
		h.NewStart++
	}
	h.Header = funcname(a, oldBefore)
	return h, len(h.Lines)
}

// funcname is git's default hunk header: the nearest line before the
// hunk's first old line that starts with a letter, '_' or '$', trailing
// space trimmed, at most 80 bytes.
func funcname(a []string, before int) string {
	for i := min(before, len(a)) - 1; i >= 0; i-- {
		l := a[i]
		if l == "" {
			continue
		}
		c := l[0]
		if c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c == '_' || c == '$' {
			l = strings.TrimRight(l, " \t\r")
			if len(l) > 80 {
				l = l[:80]
				for !utf8.ValidString(l) {
					l = l[:len(l)-1]
				}
			}
			return l
		}
	}
	return ""
}
