package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/diff"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// What changed in a workspace, on the snapshot backend (docs/api.md,
// Workspace: History, The change stream, A reply's changes). Every stored
// snapshot is indexed once against the one before it (store
// IndexSnapshot); the change stream and history read that index, and a
// run's diff compares the snapshots before and after it.

const (
	// changesPoll is how often the change stream looks for new snapshots.
	changesPoll = time.Second
	// changesBacklog is the most changes a stream replays on resuming; past
	// it the client is told to resync instead.
	changesBacklog = 1000
	// indexBatch is how many snapshots one index pass reads at a time.
	indexBatch = 50
)

// snapshotRef is a snapshot as a ref (history, before and after): opaque to
// a client, "s" and the snapshot's id here.
func snapshotRef(id int64) string { return "s" + strconv.FormatInt(id, 10) }

func parseSnapshotRef(ref string) (int64, bool) {
	n, err := strconv.ParseInt(strings.TrimPrefix(ref, "s"), 10, 64)
	return n, err == nil && strings.HasPrefix(ref, "s") && n > 0
}

// snapshotFor is the snapshot a read names: ref (a past version from
// history or a run's changes, of this workspace) or the newest.
func (s *Server) snapshotFor(r *http.Request, t *store.WorkspaceTree) (*workspace.Snapshot, error) {
	ref := r.URL.Query().Get("ref")
	if ref == "" {
		snap, _, err := s.snapshot(r.Context(), t)
		return snap, err
	}
	id, ok := parseSnapshotRef(ref)
	if !ok || t.ConversationID == nil {
		return nil, store.ErrNotFound
	}
	meta, err := s.Store.GetSnapshot(r.Context(), id)
	if err != nil {
		return nil, err
	}
	if meta.ConversationID != *t.ConversationID {
		return nil, store.ErrNotFound
	}
	return s.snapshotByID(r.Context(), id)
}

// indexLock is one conversation's index pass in this instance, so two
// streams on one workspace do not both read every snapshot.
func (s *Server) indexLock(conversationID string) *sync.Mutex {
	st := s.wsState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.indexing == nil {
		st.indexing = map[string]*sync.Mutex{}
	}
	m, ok := st.indexing[conversationID]
	if !ok {
		m = &sync.Mutex{}
		st.indexing[conversationID] = m
	}
	return m
}

// ensureIndexed indexes every snapshot of a conversation that is not yet,
// oldest first.
func (s *Server) ensureIndexed(ctx context.Context, conversationID string) error {
	mu := s.indexLock(conversationID)
	mu.Lock()
	defer mu.Unlock()
	for {
		list, err := s.Store.UnindexedSnapshots(ctx, conversationID, indexBatch)
		if err != nil || len(list) == 0 {
			return err
		}
		for i := range list {
			if err := s.indexSnapshot(ctx, &list[i]); err != nil {
				return err
			}
		}
	}
}

// indexSnapshot stores what one snapshot changed against the one before it.
// A person's restore is a restore, not a create.
func (s *Server) indexSnapshot(ctx context.Context, meta *store.Workspace) error {
	after, err := s.snapshotByID(ctx, meta.ID)
	if err != nil {
		return err
	}
	var before *workspace.Snapshot
	prev, err := s.Store.PreviousSnapshot(ctx, meta.ConversationID, meta.ID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return err
	default:
		if before, err = s.snapshotByID(ctx, prev.ID); err != nil {
			return err
		}
	}
	var restored []string
	if meta.Source == store.SnapshotPerson {
		if restored, err = s.Store.SnapshotRestores(ctx, meta.ID); err != nil {
			return err
		}
	}
	changes := workspace.Changes(before, after)
	in := make([]store.WorkspaceChangeIn, 0, len(changes))
	for _, c := range changes {
		if c.Op == workspace.OpCreate {
			for _, p := range restored {
				if c.Path == p || strings.HasPrefix(c.Path, p+"/") {
					c.Op = workspace.OpRestore
					break
				}
			}
		}
		in = append(in, store.WorkspaceChangeIn{Op: c.Op, Path: c.Path, From: c.From, Kind: c.Kind, Version: c.Version})
	}
	return s.Store.IndexSnapshot(ctx, meta, in)
}

// byDoc is who made a change.
type byDoc struct {
	Kind       string  `json:"kind"`
	RunID      *string `json:"run_id"`
	Person     *string `json:"person"`
	TerminalID *string `json:"terminal_id"`
}

// byOf is who stored a snapshot: a run, or a person (null for the operator
// token).
func byOf(source string, runID, author *string) byDoc {
	if source == store.SnapshotRun {
		return byDoc{Kind: "run", RunID: runID}
	}
	return byDoc{Kind: "person", Person: author}
}

// changeDoc is a change event's data.
type changeDoc struct {
	Path    string  `json:"path"`
	Op      string  `json:"op"`
	Kind    string  `json:"kind"`
	From    *string `json:"from"`
	Version *string `json:"version"`
	By      byDoc   `json:"by"`
}

// changesCursor is a position in a workspace's changes: every change of
// snapshots before Snapshot, and of Snapshot up to Seq, was sent.
type changesCursor struct {
	Snapshot int64
	Seq      int
}

// String is the cursor as an event id: "<snapshot>" for all of it,
// "<snapshot>.<seq>" for part.
func (c changesCursor) String() string {
	if c.Seq == math.MaxInt32 {
		return strconv.FormatInt(c.Snapshot, 10)
	}
	return fmt.Sprintf("%d.%d", c.Snapshot, c.Seq)
}

func parseChangesCursor(s string) (changesCursor, bool) {
	a, b, part := strings.Cut(s, ".")
	n, err := strconv.ParseInt(a, 10, 64)
	if err != nil || n < 0 {
		return changesCursor{}, false
	}
	c := changesCursor{Snapshot: n, Seq: math.MaxInt32}
	if part {
		seq, err := strconv.Atoi(b)
		if err != nil || seq < 0 {
			return changesCursor{}, false
		}
		c.Seq = seq
	}
	return c, true
}

// underPath reports whether p is in the subtree root ("" is everything).
func underPath(p, root string) bool {
	return root == "" || p == root || strings.HasPrefix(p, root+"/")
}

// workspaceChangeStream is GET /workspaces/{id}/changes: Server-Sent
// Events of every change under a path, resumable with Last-Event-ID.
func (s *Server) workspaceChangeStream(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	root, ok := pathParam(w, r, "path")
	if !ok {
		return
	}
	if t.ConversationID == nil {
		wsFail(w, store.ErrNotFound)
		return
	}
	conv := *t.ConversationID
	ctx := r.Context()
	if err := s.ensureIndexed(ctx, conv); err != nil {
		wsFail(w, err)
		return
	}
	head, err := s.Store.IndexedHead(ctx, conv)
	if err != nil {
		wsFail(w, err)
		return
	}
	cursor := changesCursor{Snapshot: head, Seq: math.MaxInt32}
	lost := false
	if last := r.Header.Get("Last-Event-ID"); last != "" {
		c, ok := parseChangesCursor(last)
		switch {
		case !ok || c.Snapshot > head:
			lost = true
		case c.Snapshot == 0:
			cursor = c
		default:
			meta, err := s.Store.GetSnapshot(ctx, c.Snapshot)
			if err != nil || meta.ConversationID != conv {
				lost = true
			} else {
				cursor = c
			}
		}
	}
	rc := http.NewResponseController(w)
	if err := rc.SetWriteDeadline(time.Time{}); err != nil {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported: "+err.Error())
		return
	}
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	var buf strings.Builder
	add := func(event, id string, data any) {
		b, _ := json.Marshal(data)
		if id != "" {
			fmt.Fprintf(&buf, "id: %s\n", id)
		}
		fmt.Fprintf(&buf, "event: %s\ndata: %s\n\n", event, b)
	}
	flush := func() error {
		if buf.Len() == 0 {
			return nil
		}
		_, err := w.Write([]byte(buf.String()))
		buf.Reset()
		if err != nil {
			return err
		}
		return rc.Flush()
	}
	add("ready", cursor.String(), map[string]string{"backend": backendSnapshot, "cursor": cursor.String()})
	if lost {
		add("resync", "", map[string]string{"reason": "cursor_lost"})
	}
	// send queues the changes after the cursor up to the indexed head, or
	// a resync past changesBacklog of them.
	send := func() error {
		if err := s.ensureIndexed(ctx, conv); err != nil {
			return err
		}
		head, err := s.Store.IndexedHead(ctx, conv)
		if err != nil {
			return err
		}
		if head <= cursor.Snapshot && cursor.Seq == math.MaxInt32 {
			return flush()
		}
		list, err := s.Store.WorkspaceChanges(ctx, conv, cursor.Snapshot, cursor.Seq, head, changesBacklog+1)
		if err != nil {
			return err
		}
		if len(list) > changesBacklog {
			cursor = changesCursor{Snapshot: head, Seq: math.MaxInt32}
			add("resync", "", map[string]string{"reason": "overflow"})
			return flush()
		}
		for _, c := range list {
			if !underPath(c.Path, root) && (c.From == nil || !underPath(*c.From, root)) {
				continue
			}
			id := changesCursor{Snapshot: c.SnapshotID, Seq: c.Seq}.String()
			add("change", id, changeDoc{Path: c.Path, Op: c.Op, Kind: c.Kind, From: c.From, Version: c.Version,
				By: byOf(c.Source, c.RunID, c.Author)})
		}
		cursor = changesCursor{Snapshot: head, Seq: math.MaxInt32}
		return flush()
	}
	if err := send(); err != nil {
		return
	}
	poll := time.NewTicker(orDefault(s.ChangesPoll, changesPoll))
	defer poll.Stop()
	ping := time.NewTicker(orDefault(s.StreamPing, streamPing))
	defer ping.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.Stop:
			return
		case <-ping.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		case <-poll.C:
			if err := send(); err != nil {
				if ctx.Err() == nil {
					s.Log.Warn("workspace change stream", "workspace", t.ID, "err", err)
				}
				return
			}
		}
	}
}

// versionDoc is one version of a file in its history.
type versionDoc struct {
	Ref     string    `json:"ref"`
	Version string    `json:"version"`
	At      time.Time `json:"at"`
	By      byDoc     `json:"by"`
	Source  string    `json:"source"`
}

// workspaceHistory is GET /workspaces/{id}/history: a file's versions,
// newest first.
func (s *Server) workspaceHistory(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	p, ok := pathParam(w, r, "path")
	if !ok {
		return
	}
	if p == "" {
		wsErr(w, http.StatusBadRequest, "path", "path: the root is a directory, which has no versions")
		return
	}
	var before int64
	if b := r.URL.Query().Get("before"); b != "" {
		n, err := strconv.ParseInt(b, 10, 64)
		if err != nil || n <= 0 {
			wsErr(w, http.StatusBadRequest, "request", "before: not a cursor from next_before")
			return
		}
		before = n
	}
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	if t.ConversationID == nil {
		wsFail(w, store.ErrNotFound)
		return
	}
	if err := s.ensureIndexed(r.Context(), *t.ConversationID); err != nil {
		wsFail(w, err)
		return
	}
	list, more, err := s.Store.PathHistory(r.Context(), *t.ConversationID, p, before, limit)
	if err != nil {
		wsFail(w, err)
		return
	}
	versions := make([]versionDoc, 0, len(list))
	for _, c := range list {
		versions = append(versions, versionDoc{Ref: snapshotRef(c.SnapshotID), Version: *c.Version, At: c.At,
			By: byOf(c.Source, c.RunID, c.Author), Source: "snapshot"})
	}
	resp := map[string]any{"versions": versions, "next_before": nil}
	if more {
		resp["next_before"] = strconv.FormatInt(list[len(list)-1].SnapshotID, 10)
	}
	respond(w, resp, nil)
}

// snapshotPoint is a run's before or after: the snapshot as a ref and when
// it was stored.
type snapshotPoint struct {
	Ref string    `json:"ref"`
	At  time.Time `json:"at"`
}

func pointOf(w *store.Workspace) *snapshotPoint {
	if w == nil {
		return nil
	}
	return &snapshotPoint{Ref: snapshotRef(w.ID), At: w.CreatedAt}
}

// runSnapshots opens a run's before and after snapshots (an empty tree for
// none).
func (s *Server) runSnapshots(ctx context.Context, rs *store.RunSnapshots) (before, after *workspace.Snapshot, err error) {
	open := func(w *store.Workspace) (*workspace.Snapshot, error) {
		if w == nil {
			return emptySnapshot, nil
		}
		return s.snapshotByID(ctx, w.ID)
	}
	if before, err = open(rs.Before); err != nil {
		return nil, nil, err
	}
	after, err = open(rs.After)
	return before, after, err
}

// fileDiffs are the diff files from before to after, by path: the regular
// files that differ (workspace.Diff). only narrows to one path (the file's
// path after, or before for a deleted one) and lifts the per-file line cap,
// so a truncated file is read whole. sum counts every file, whatever the
// caps left out of files.
func fileDiffs(before, after *workspace.Snapshot, only string) (files []diff.File, truncated bool, sum store.ChangeSummary) {
	files = []diff.File{}
	bytes := 0
	for _, c := range workspace.Diff(before, after) {
		if only != "" && c.Path != only {
			continue
		}
		data := func(s *workspace.Snapshot, p string) []byte {
			if e, err := s.Lookup(p); err == nil {
				return e.Data()
			}
			return nil
		}
		f := diff.File{Path: c.Path, Hunks: []diff.Hunk{}}
		var old, new []byte
		switch c.Op {
		case "added":
			f.Status, new = diff.Added, data(after, c.Path)
		case "removed":
			f.Status, old = diff.Deleted, data(before, c.Path)
		case "renamed":
			from := c.From
			f.Status, f.OldPath = diff.Renamed, &from
		default:
			f.Status, old, new = diff.Modified, data(before, c.Path), data(after, c.Path)
			if string(old) == string(new) {
				f.Status = diff.Mode
			}
		}
		sum.Files++
		full := len(files) < diff.MaxFiles && bytes < diff.MaxBytes
		if !full {
			a, d, _ := diff.Count(old, new)
			sum.Additions += a
			sum.Deletions += d
			truncated = true
			continue
		}
		maxLines := diff.MaxFileLines
		if only != "" {
			maxLines = math.MaxInt
		}
		f.Hunks, f.Additions, f.Deletions, f.Binary, f.Truncated = diff.Compare(old, new, maxLines)
		if f.Hunks == nil {
			f.Hunks = []diff.Hunk{}
		}
		sum.Additions += f.Additions
		sum.Deletions += f.Deletions
		if n := f.Bytes(); bytes+n > diff.MaxBytes {
			// Over the response's bytes: the file is listed without its
			// lines, to be read whole with path.
			f.Hunks, f.Truncated, truncated = []diff.Hunk{}, len(f.Hunks) > 0, true
		} else {
			bytes += n
		}
		files = append(files, f)
	}
	return files, truncated, sum
}

// runChangesRoute is GET /runs/{id}/changes: what a run changed in its
// workspace, as diff files.
func (s *Server) runChangesRoute(w http.ResponseWriter, r *http.Request) {
	only := ""
	if r.URL.Query().Has("path") {
		p, ok := pathParam(w, r, "path")
		if !ok {
			return
		}
		only = p
	}
	rs, err := s.Store.GetRunSnapshots(r.Context(), r.PathValue("id"))
	if err != nil {
		wsFail(w, err)
		return
	}
	if rs.WorkspaceID == nil {
		wsErr(w, http.StatusNotFound, "no_workspace", "the run's conversation has no workspace; its journal's diff items are what it changed")
		return
	}
	if !s.allow(w, r, *rs.WorkspaceID, "read", s.rate(s.WorkspaceReadRate, readRate)) {
		return
	}
	before, after, err := s.runSnapshots(r.Context(), rs)
	if err != nil {
		wsFail(w, err)
		return
	}
	files, truncated, sum := fileDiffs(before, after, only)
	if rs.Final && only == "" {
		if err := s.Store.PutRunChanges(r.Context(), rs.RunID, rs.Before, rs.After, sum); err != nil {
			s.Log.Warn("store a run's changes", "run", rs.RunID, "err", err)
		}
	}
	respond(w, map[string]any{"run_id": rs.RunID, "workspace_id": *rs.WorkspaceID,
		"before": pointOf(rs.Before), "after": pointOf(rs.After), "files": files, "truncated": truncated}, nil)
}

// changeSummaries returns each named run's changes summary, computing and
// storing the ones a finished run with a workspace does not have yet. A
// run left out has none (null).
func (s *Server) changeSummaries(ctx context.Context, runIDs []string) (map[string]*store.ChangeSummary, error) {
	states, err := s.Store.RunChangeStates(ctx, runIDs)
	if err != nil {
		return nil, err
	}
	out := map[string]*store.ChangeSummary{}
	ids := make([]string, 0, len(states))
	for id := range states {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		st := states[id]
		if !st.Pending {
			out[id] = st.Summary
			continue
		}
		rs, err := s.Store.GetRunSnapshots(ctx, id)
		if err != nil {
			return nil, err
		}
		before, after, err := s.runSnapshots(ctx, rs)
		if err != nil {
			return nil, err
		}
		_, _, sum := fileDiffs(before, after, "")
		if err := s.Store.PutRunChanges(ctx, id, rs.Before, rs.After, sum); err != nil {
			return nil, err
		}
		out[id] = &sum
	}
	return out, nil
}

// withRunChanges fills changes on the run items of thread items.
func (s *Server) withRunChanges(ctx context.Context, items []store.ThreadItem) error {
	var ids []string
	for _, it := range items {
		if it.Run != nil {
			ids = append(ids, it.Run.ID)
		}
	}
	got, err := s.changeSummaries(ctx, ids)
	if err != nil {
		return err
	}
	for _, it := range items {
		if it.Run != nil {
			it.Run.Changes = got[it.Run.ID]
		}
	}
	return nil
}

// withRunsChanges fills changes on runs.
func (s *Server) withRunsChanges(ctx context.Context, runs []store.Run) error {
	ids := make([]string, 0, len(runs))
	for _, r := range runs {
		ids = append(ids, r.ID)
	}
	got, err := s.changeSummaries(ctx, ids)
	if err != nil {
		return err
	}
	for i := range runs {
		runs[i].Changes = got[runs[i].ID]
	}
	return nil
}
