package diff

import (
	"fmt"
	"strconv"
	"strings"
)

// ParsePatch reads a unified diff's hunks (GitHub's pull request file
// "patch": hunks without the file headers) into hunks with line numbers.
// At most maxLines lines are carried (truncated reports more); additions
// and deletions count the whole patch.
func ParsePatch(patch string, maxLines int) (hunks []Hunk, additions, deletions int, truncated bool, err error) {
	if patch == "" {
		return nil, 0, 0, false, nil
	}
	lines := strings.Split(strings.TrimSuffix(patch, "\n"), "\n")
	var cur *Hunk
	var oldN, newN, carried int
	flush := func() {
		if cur != nil && len(cur.Lines) > 0 {
			hunks = append(hunks, *cur)
		}
		cur = nil
	}
	for i, l := range lines {
		if strings.HasPrefix(l, "@@") {
			flush()
			h, err := parseHeader(l)
			if err != nil {
				return nil, 0, 0, false, fmt.Errorf("patch line %d: %w", i+1, err)
			}
			oldN, newN = h.OldStart, h.NewStart
			if h.OldLines == 0 {
				oldN++
			}
			if h.NewLines == 0 {
				newN++
			}
			// The counts are what the hunk carries, set as lines are kept.
			h.OldLines, h.NewLines = 0, 0
			cur = &h
			continue
		}
		if cur == nil {
			// Anything before the first hunk is a file header.
			continue
		}
		kind, text := KindContext, ""
		switch {
		case strings.HasPrefix(l, `\`):
			// "\ No newline at end of file" describes the line before.
			continue
		case l == "":
			// A context line whose single space was lost.
		case l[0] == ' ':
			text = l[1:]
		case l[0] == '+':
			kind, text = KindAdd, l[1:]
		case l[0] == '-':
			kind, text = KindDelete, l[1:]
		default:
			return nil, 0, 0, false, fmt.Errorf("patch line %d: %q is not a hunk line", i+1, l)
		}
		switch kind {
		case KindAdd:
			additions++
		case KindDelete:
			deletions++
		}
		if carried >= maxLines {
			truncated = true
			continue
		}
		carried++
		ln := Line{Kind: kind, Text: text}
		if kind != KindAdd {
			o := oldN
			ln.Old = &o
			oldN++
			cur.OldLines++
		}
		if kind != KindDelete {
			n := newN
			ln.New = &n
			newN++
			cur.NewLines++
		}
		cur.Lines = append(cur.Lines, ln)
	}
	flush()
	return hunks, additions, deletions, truncated, nil
}

// parseHeader reads "@@ -a,b +c,d @@ header" (a count of 1 may be left
// out).
func parseHeader(l string) (Hunk, error) {
	var h Hunk
	rest, ok := strings.CutPrefix(l, "@@ -")
	if !ok {
		return h, fmt.Errorf("%q is not a hunk header", l)
	}
	ranges, header, ok := strings.Cut(rest, " @@")
	if !ok {
		return h, fmt.Errorf("%q is not a hunk header", l)
	}
	oldR, newR, ok := strings.Cut(ranges, " +")
	if !ok {
		return h, fmt.Errorf("%q is not a hunk header", l)
	}
	var err error
	if h.OldStart, h.OldLines, err = parseRange(oldR); err != nil {
		return h, err
	}
	if h.NewStart, h.NewLines, err = parseRange(newR); err != nil {
		return h, err
	}
	h.Header = strings.TrimPrefix(header, " ")
	return h, nil
}

func parseRange(s string) (start, n int, err error) {
	a, b, has := strings.Cut(s, ",")
	if start, err = strconv.Atoi(a); err != nil || start < 0 {
		return 0, 0, fmt.Errorf("hunk range %q", s)
	}
	n = 1
	if has {
		if n, err = strconv.Atoi(b); err != nil || n < 0 {
			return 0, 0, fmt.Errorf("hunk range %q", s)
		}
	}
	return start, n, nil
}
