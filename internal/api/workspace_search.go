package api

import (
	"bytes"
	"net/http"
	"regexp"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// Search across a workspace's files (docs/api.md, Workspace, Search): code
// over the newest snapshot, line by line, with Go's regexp (RE2: linear in
// the text, so no pattern can make a search run away). Results are bounded
// in count, preview length and time.

const (
	// searchQueryMax bounds q.
	searchQueryMax = 256
	// searchLimit and searchLimitMax are a search's default and most
	// matches.
	searchLimit, searchLimitMax = 200, 1000
	// searchPreview is the most bytes of a line a match shows.
	searchPreview = 200
	// searchBudget bounds one search's time; past it the matches so far
	// are returned, truncated.
	searchBudget = 5 * time.Second
)

// searchMatch is one match: its line and its columns, 1-based in UTF-16
// code units (as a JavaScript string counts, which is how an editor
// places a cursor), and a short preview of the line.
type searchMatch struct {
	Path      string `json:"path"`
	Line      int    `json:"line"`
	Column    int    `json:"column"`
	EndColumn int    `json:"end_column"`
	Preview   string `json:"preview"`
	// PreviewColumn is where the preview starts in the line, 1-based as
	// Column.
	PreviewColumn int `json:"preview_column"`
}

// utf16Len is how many UTF-16 code units b is.
func utf16Len(b []byte) int {
	n := 0
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		n += len(utf16.Encode([]rune{r}))
		b = b[size:]
	}
	return n
}

// preview is up to searchPreview bytes of line around [lo, hi), cut on
// rune boundaries, and the byte where it starts.
func preview(line []byte, lo, hi int) ([]byte, int) {
	if len(line) <= searchPreview {
		return line, 0
	}
	start := max(0, lo-(searchPreview-(hi-lo))/2)
	if hi-lo >= searchPreview {
		start = lo
	}
	end := min(len(line), start+searchPreview)
	start = max(0, min(start, end-searchPreview))
	for start > 0 && !utf8.RuneStart(line[start]) {
		start--
	}
	for end < len(line) && !utf8.RuneStart(line[end]) {
		end--
	}
	return line[start:end], start
}

// workspaceSearch is GET /workspaces/{id}/search?q=&path=&regex=&case=&limit=.
func (s *Server) workspaceSearch(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	q := r.URL.Query()
	query := q.Get("q")
	switch {
	case query == "":
		wsErr(w, http.StatusBadRequest, "request", "q: a search needs text")
		return
	case len(query) > searchQueryMax:
		wsErr(w, http.StatusBadRequest, "request", "q: at most 256 bytes")
		return
	}
	root, ok := pathParam(w, r, "path")
	if !ok {
		return
	}
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	if limit <= 0 {
		limit = searchLimit
	}
	limit = min(limit, searchLimitMax)
	pattern := regexp.QuoteMeta(query)
	if q.Get("regex") == "1" {
		pattern = query
	}
	if q.Get("case") != "1" {
		pattern = "(?i)" + pattern
	}
	re, err := regexp.Compile(pattern)
	if err != nil {
		wsErr(w, http.StatusBadRequest, "request", "q: "+err.Error())
		return
	}
	snap, err := s.snapshotFor(r, t)
	if err != nil {
		wsFail(w, err)
		return
	}
	top, err := snap.Lookup(root)
	if err != nil {
		wsFail(w, err)
		return
	}
	deadline := time.Now().Add(searchBudget)
	matches := []searchMatch{}
	files, truncated := 0, false
	// search scans one file; false stops the search.
	search := func(e *workspace.Entry) bool {
		data := e.Data()
		if e.Bytes > EditorReadMax || !workspace.IsText(data) {
			return true
		}
		files++
		for n := 1; len(data) > 0; n++ {
			line := data
			if i := bytes.IndexByte(data, '\n'); i >= 0 {
				line, data = data[:i], data[i+1:]
			} else {
				data = nil
			}
			line = bytes.TrimSuffix(line, []byte("\r"))
			for _, m := range re.FindAllIndex(line, -1) {
				if m[0] == m[1] {
					continue
				}
				if len(matches) == limit {
					truncated = true
					return false
				}
				pv, at := preview(line, m[0], m[1])
				col := utf16Len(line[:m[0]]) + 1
				matches = append(matches, searchMatch{Path: e.Path, Line: n, Column: col,
					EndColumn: col + utf16Len(line[m[0]:m[1]]), Preview: string(pv), PreviewColumn: utf16Len(line[:at]) + 1})
			}
		}
		if time.Now().After(deadline) || r.Context().Err() != nil {
			truncated = true
			return false
		}
		return true
	}
	// walk visits files in tree order, skipping what .gitignore ignores.
	var walk func(e *workspace.Entry) bool
	walk = func(e *workspace.Entry) bool {
		if e.Path != "" && e.Path != root && snap.Ignored(e) {
			return true
		}
		switch e.Kind {
		case workspace.KindFile:
			return search(e)
		case workspace.KindDir:
			for _, c := range snap.Children(e.Path) {
				if !walk(c) {
					return false
				}
			}
		}
		return true
	}
	walk(top)
	respond(w, map[string]any{"matches": matches, "truncated": truncated, "files_searched": files}, nil)
}
