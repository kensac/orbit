package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/store"
)

// importChunkBytes is how many bytes of lines one POST /sessions/import carries: well inside the server's body limit (api.sessionImportBodyLimit, 24 MiB) with a line of claudeimport.MaxSessionLine past it.
const importChunkBytes = 8 << 20

// sessionChunk is one session's stretch of lines in an import request (docs/api.md, Imported sessions).
type sessionChunk struct {
	SessionID  string            `json:"session_id"`
	Repo       string            `json:"repo,omitempty"`
	FromOffset int64             `json:"from_offset"`
	FromPrefix string            `json:"from_prefix_sha256"`
	Rewrite    bool              `json:"rewrite,omitempty"`
	ToOffset   int64             `json:"to_offset"`
	ToPrefix   string            `json:"to_prefix_sha256"`
	Lines      []json.RawMessage `json:"lines"`
}

// importSessions imports a directory of Claude Code session transcripts. Without --apply it reads what orbit holds and prints what an import would send, sending no line; --print reads the files only.
func importSessions(ctx context.Context, args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("import claude-sessions", flag.ContinueOnError)
	var api apiFlags
	api.register(fs)
	apply := fs.Bool("apply", false, "send the new lines; without it orbit's state is read and nothing is sent")
	printOnly := fs.Bool("print", false, "list the sessions read from disk and stop, without calling the server")
	dir, rest := "", args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		dir, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if dir == "" && fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if dir == "" {
		return errors.New("import claude-sessions: name the projects directory (~/.claude/projects) or one project's directory")
	}
	files, err := claudeimport.FindSessions(dir)
	if err != nil {
		return err
	}
	state := map[string]*claudeimport.ImportState{}
	if !*printOnly {
		if state, err = sessionState(ctx, &api); err != nil {
			return err
		}
	}
	imp := &sessionImporter{api: &api, ctx: ctx, results: map[string]*store.SessionImportResult{}}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	counts := map[string]int{}
	repos := map[string]string{}
	var warnings []string
	for _, f := range files {
		b, err := os.ReadFile(f.Path)
		if err != nil {
			return err
		}
		p := claudeimport.PlanSession(f, b, state[f.SessionID])
		if *printOnly {
			_, _ = fmt.Fprintf(tw, "%s\t%d lines\t%d bytes\t%s\n", f.SessionID, len(p.Lines), p.Bytes, p.Cwd)
			continue
		}
		counts[p.Outcome]++
		if p.Outcome == claudeimport.PlanEmpty || p.Outcome == store.SessionUnchanged {
			continue
		}
		if !*apply {
			_, _ = fmt.Fprintf(tw, "%s\t%s\t%d lines\t%d bytes\t%s\n", p.Outcome, f.SessionID, len(p.Lines), p.Bytes, p.Cwd)
			continue
		}
		repo, ok := repos[p.Cwd]
		if !ok {
			repo = claudeimport.RepoOf(p.Cwd)
			repos[p.Cwd] = repo
		}
		w, err := imp.add(p, b, repo)
		warnings = append(warnings, w...)
		if err != nil {
			return err
		}
	}
	if *printOnly {
		return tw.Flush()
	}
	if err := imp.flush(); err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if !*apply {
		_ = tw.Flush()
		_, _ = fmt.Fprintf(stdout, "\n%d to create, %d to append to, %d to rewrite, %d unchanged, %d empty\n", counts[store.SessionCreated],
			counts[store.SessionAppended], counts[store.SessionRewritten], counts[store.SessionUnchanged], counts[claudeimport.PlanEmpty])
		_, _ = fmt.Fprintln(stdout, "dry run: nothing was sent; run again with --apply")
		return nil
	}
	got := map[string]int{}
	added, folded := 0, 0
	for _, f := range files {
		r, ok := imp.results[f.SessionID]
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%d lines added\t%d now\n", r.Outcome, r.SessionID, r.Added, r.Lines)
		got[r.Outcome]++
		added += r.Added
		folded += r.Added - r.New
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(stdout, "\n%d created, %d appended to, %d rewritten, %d conflicts, %d unchanged; %d lines added, %d of them already stored by another session\n",
		got[store.SessionCreated], got[store.SessionAppended], got[store.SessionRewritten], got[store.SessionConflict],
		counts[store.SessionUnchanged], added, folded)
	if got[store.SessionConflict] > 0 {
		return errors.New("some sessions changed in orbit while this import ran; run it again")
	}
	return nil
}

// sessionState reads every Claude Code session orbit holds, a page at a time.
func sessionState(ctx context.Context, api *apiFlags) (map[string]*claudeimport.ImportState, error) {
	state := map[string]*claudeimport.ImportState{}
	before := ""
	for {
		q := url.Values{"source": {claudeimport.SourceClaudeCode}, "limit": {fmt.Sprint(store.MaxSessionList)}}
		if before != "" {
			q.Set("before", before)
		}
		var page struct {
			Sessions   []store.ExternalSession `json:"sessions"`
			HasMore    bool                    `json:"has_more"`
			NextBefore *string                 `json:"next_before"`
		}
		if err := api.call(ctx, http.MethodGet, "/api/v1/sessions?"+q.Encode(), nil, &page); err != nil {
			return nil, err
		}
		for _, s := range page.Sessions {
			state[s.SessionID] = &claudeimport.ImportState{Offset: s.OffsetBytes, PrefixSHA256: s.PrefixSHA256}
		}
		if !page.HasMore || page.NextBefore == nil {
			return state, nil
		}
		before = *page.NextBefore
	}
}

// sessionImporter batches chunks into requests of at most importChunkBytes and MaxImport{Sessions,Lines}, keeping each session's first result (its outcome) and the sums of the rest.
type sessionImporter struct {
	api     *apiFlags
	ctx     context.Context
	pending []sessionChunk
	bytes   int
	lines   int
	results map[string]*store.SessionImportResult
}

// add splits one planned file into chunks, normalizing each line (so a credential is scrubbed before it leaves this machine), and sends full requests as they fill. A line that is not a JSON object is skipped with a warning; the offsets still cover it.
func (m *sessionImporter) add(p claudeimport.SessionPlan, b []byte, repo string) ([]string, error) {
	var warnings []string
	from := p.From
	fromPrefix := claudeimport.PrefixSHA256(b, from)
	c := sessionChunk{SessionID: p.File.SessionID, Repo: repo, FromOffset: from, FromPrefix: fromPrefix, Rewrite: p.Rewrite, Lines: []json.RawMessage{}}
	size := 0
	emit := func(to int64) error {
		c.ToOffset, c.ToPrefix = to, claudeimport.PrefixSHA256(b, to)
		m.pending = append(m.pending, c)
		m.bytes += size
		m.lines += len(c.Lines)
		next := sessionChunk{SessionID: c.SessionID, Repo: repo, FromOffset: to, FromPrefix: c.ToPrefix, Lines: []json.RawMessage{}}
		c, size = next, 0
		if m.bytes >= importChunkBytes || len(m.pending) >= store.MaxImportSessions || m.lines >= store.MaxImportLines-1000 {
			return m.flush()
		}
		return nil
	}
	for i, raw := range p.Lines {
		line, err := claudeimport.NormalizeLine(raw)
		if err != nil {
			warnings = append(warnings, fmt.Sprintf("%s: line ending at byte %d: skipped: %v", p.File.Path, p.Ends[i], err))
		} else {
			c.Lines = append(c.Lines, line)
			size += len(line)
		}
		if m.bytes+size >= importChunkBytes || len(c.Lines) >= 1000 {
			if err := emit(p.Ends[i]); err != nil {
				return warnings, err
			}
		}
	}
	if len(p.Ends) > 0 && c.FromOffset < p.Ends[len(p.Ends)-1] {
		return warnings, emit(p.Ends[len(p.Ends)-1])
	}
	return warnings, nil
}

// flush sends the pending chunks as one import.
func (m *sessionImporter) flush() error {
	if len(m.pending) == 0 {
		return nil
	}
	var out struct {
		Results []store.SessionImportResult `json:"results"`
	}
	if err := m.api.call(m.ctx, http.MethodPost, "/api/v1/sessions/import", map[string]any{
		"source": claudeimport.SourceClaudeCode, "sessions": m.pending,
	}, &out); err != nil {
		return err
	}
	for _, r := range out.Results {
		if prev, ok := m.results[r.SessionID]; ok {
			prev.Added += r.Added
			prev.New += r.New
			prev.Lines, prev.Offset = r.Lines, r.Offset
			if r.Outcome == store.SessionConflict {
				prev.Outcome, prev.Error = r.Outcome, r.Error
			}
			continue
		}
		m.results[r.SessionID] = &r
	}
	m.pending, m.bytes, m.lines = nil, 0, 0
	return nil
}
