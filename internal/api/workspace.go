package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"mime"
	"net/http"
	"path"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// The workspace API (docs/api.md, Workspace; design docs/workspace.md).
// The snapshot backend serves a conversation's newest stored snapshot, so
// reads work whichever host ran the conversation and whether or not its
// sandbox is up.

// Workspace limits (docs/workspace.md, Auth, scope and limits).
const (
	// EditorReadMax is the largest file GET /files returns as text.
	EditorReadMax = 2 << 20
	// treeLimit and treeLimitMax are a tree page's default and most entries.
	treeLimit, treeLimitMax = 500, 2000
	// readRate and writeRate are requests a second per person per
	// workspace, each class with a burst of one second's worth.
	readRate, writeRate = 100, 20
	// snapshotCacheBytes bounds the indexed snapshots kept in memory: two of
	// the largest a workspace can be (workspace.MaxBytes).
	snapshotCacheBytes = 2 * workspace.MaxBytes
)

// backendSnapshot is the backend every workspace has today.
const backendSnapshot = "snapshot"

// capabilities is what a client may show for a workspace: the backend's
// abilities, narrowed by the person's (docs/api.md, Finding a workspace).
type capabilities struct {
	Read      bool     `json:"read"`
	Write     bool     `json:"write"`
	Terminals bool     `json:"terminals"`
	Git       bool     `json:"git"`
	Watch     bool     `json:"watch"`
	Presets   []string `json:"presets"`
}

// snapshotCapabilities are the snapshot backend's: reads and writes, no
// terminals or git (orbit never runs git over a snapshot), and no change
// stream until it is built.
var snapshotCapabilities = capabilities{Read: true, Write: true, Presets: []string{}}

type asOfDoc struct {
	SnapshotID int64     `json:"snapshot_id"`
	RunID      *string   `json:"run_id"`
	At         time.Time `json:"at"`
}

type workspaceDoc struct {
	ID             string       `json:"id"`
	Scope          string       `json:"scope"`
	ConversationID *string      `json:"conversation_id"`
	Agent          string       `json:"agent"`
	Backend        string       `json:"backend"`
	Cwd            string       `json:"cwd"`
	Capabilities   capabilities `json:"capabilities"`
	AsOf           *asOfDoc     `json:"as_of"`
	Box            any          `json:"box"`
	CreatedAt      time.Time    `json:"created_at"`
}

// entryDoc is a tree entry as the API shows it.
type entryDoc struct {
	Path        string    `json:"path"`
	Name        string    `json:"name"`
	Kind        string    `json:"kind"`
	Bytes       int64     `json:"bytes"`
	Mode        string    `json:"mode"`
	ModifiedAt  time.Time `json:"modified_at"`
	Version     string    `json:"version,omitempty"`
	Target      *string   `json:"target"`
	Ignored     bool      `json:"ignored"`
	Git         *string   `json:"git"`
	HasChildren *bool     `json:"has_children"`
}

// wsErr writes a workspace error: the message and its check.
func wsErr(w http.ResponseWriter, status int, check, msg string) {
	wsErrWith(w, status, check, msg, nil)
}

// wsErrWith is wsErr with more fields in the body.
func wsErrWith(w http.ResponseWriter, status int, check, msg string, extra map[string]any) {
	body := map[string]any{"error": msg, "check": check}
	for k, v := range extra {
		body[k] = v
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// wsFail answers a store or index error with its check.
func wsFail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound), errors.Is(err, fs.ErrNotExist), store.IsBadID(err):
		wsErr(w, http.StatusNotFound, "not_found", "not found")
	case errors.Is(err, workspace.ErrPath):
		wsErr(w, http.StatusBadRequest, "path", err.Error())
	default:
		writeErr(w, http.StatusInternalServerError, err.Error())
	}
}

// inWorkspaceGroup reports whether the request may change a workspace:
// the operator token may; a person must be in ORBIT_WORKSPACE_GROUP when it
// is set (unset, every person the token check admits is).
func (s *Server) inWorkspaceGroup(r *http.Request) bool {
	p := principalOf(r)
	if p.Person == "" || s.WorkspaceGroup == "" {
		return true
	}
	return slices.Contains(p.Groups, s.WorkspaceGroup)
}

// workspaceState is the workspace routes' memory: indexed snapshots and
// rate buckets. One per Server, made on first use.
type workspaceState struct {
	mu      sync.Mutex
	cache   map[int64]*cachedSnapshot
	bytes   int64
	tick    int64
	buckets map[string]*bucket
	swept   time.Time
}

type cachedSnapshot struct {
	snap *workspace.Snapshot
	used int64
}

func (s *Server) wsState() *workspaceState {
	s.wsOnce.Do(func() {
		s.ws = &workspaceState{cache: map[int64]*cachedSnapshot{}, buckets: map[string]*bucket{}}
	})
	return s.ws
}

// emptySnapshot is the tree of a workspace without a snapshot yet.
var emptySnapshot, _ = workspace.Open(nil, nil)

// snapshot returns a workspace's newest snapshot, indexed, and its row
// (nil when it has none).
func (s *Server) snapshot(ctx context.Context, t *store.WorkspaceTree) (*workspace.Snapshot, *store.Workspace, error) {
	if t.ConversationID == nil {
		return nil, nil, store.ErrNotFound
	}
	meta, err := s.Store.LatestSnapshot(ctx, *t.ConversationID)
	if errors.Is(err, store.ErrNotFound) {
		return emptySnapshot, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	snap, err := s.snapshotByID(ctx, meta.ID)
	if err != nil {
		return nil, nil, err
	}
	return snap, meta, nil
}

// snapshotByID returns one stored snapshot, indexed, from the cache or
// the database.
func (s *Server) snapshotByID(ctx context.Context, id int64) (*workspace.Snapshot, error) {
	st := s.wsState()
	st.mu.Lock()
	if c, ok := st.cache[id]; ok {
		st.tick++
		c.used = st.tick
		st.mu.Unlock()
		return c.snap, nil
	}
	st.mu.Unlock()
	archive, err := s.Store.SnapshotArchive(ctx, id)
	if err != nil {
		return nil, err
	}
	snap, err := workspace.Open(archive, hiddenFile)
	if err != nil {
		return nil, err
	}
	s.cacheSnapshot(id, snap)
	return snap, nil
}

// cacheSnapshot keeps an indexed snapshot, evicting the least recently used
// beyond snapshotCacheBytes (never the one just added).
func (s *Server) cacheSnapshot(id int64, snap *workspace.Snapshot) {
	st := s.wsState()
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.cache[id]; ok {
		return
	}
	st.tick++
	st.cache[id] = &cachedSnapshot{snap: snap, used: st.tick}
	st.bytes += snap.Size()
	for st.bytes > snapshotCacheBytes && len(st.cache) > 1 {
		var oldest int64 = -1
		for k, c := range st.cache {
			if k != id && (oldest < 0 || c.used < st.cache[oldest].used) {
				oldest = k
			}
		}
		st.bytes -= st.cache[oldest].snap.Size()
		delete(st.cache, oldest)
	}
}

// bucket is a token bucket: rate tokens a second, at most rate held.
type bucket struct {
	tokens float64
	at     time.Time
}

// allow spends one request of a class ("read" or "write") for the person
// on a workspace, or answers 429 with Retry-After. Buckets are per
// instance.
func (s *Server) allow(w http.ResponseWriter, r *http.Request, workspaceID, class string, rate float64) bool {
	st := s.wsState()
	now := time.Now()
	key := principalOf(r).Person + "\x00" + workspaceID + "\x00" + class
	st.mu.Lock()
	if now.Sub(st.swept) > time.Minute {
		// A bucket idle long enough to be full again is the same as none.
		for k, b := range st.buckets {
			if now.Sub(b.at) > time.Minute {
				delete(st.buckets, k)
			}
		}
		st.swept = now
	}
	b, ok := st.buckets[key]
	if !ok {
		b = &bucket{tokens: rate, at: now}
		st.buckets[key] = b
	}
	b.tokens = math.Min(rate, b.tokens+now.Sub(b.at).Seconds()*rate)
	b.at = now
	ok = b.tokens >= 1
	wait := 0.0
	if ok {
		b.tokens--
	} else {
		wait = (1 - b.tokens) / rate
	}
	st.mu.Unlock()
	if !ok {
		w.Header().Set("Retry-After", strconv.Itoa(int(math.Ceil(wait))))
		wsErr(w, http.StatusTooManyRequests, "rate", fmt.Sprintf("over %d %ss a second on this workspace", int(rate), class))
	}
	return ok
}

// rate is a configured rate, or def when unset.
func (s *Server) rate(set, def float64) float64 {
	if set > 0 {
		return set
	}
	return def
}

// workspaceRoutes adds the workspace API.
func (s *Server) workspaceRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/v1/conversations/{id}/workspace", s.conversationWorkspace)
	api("GET /api/v1/agents/{name}/workspace", s.agentWorkspace)
	api("GET /api/v1/workspaces/{id}/tree", s.readRoute(s.workspaceTree))
	api("GET /api/v1/workspaces/{id}/stat", s.readRoute(s.workspaceStat))
	api("GET /api/v1/workspaces/{id}/files", s.readRoute(s.workspaceRead))
	api("GET /api/v1/workspaces/{id}/download", s.readRoute(s.workspaceDownload))
	api("GET /api/v1/workspaces/{id}/events", s.readRoute(s.workspaceEvents))
}

// readRoute resolves {id} to a workspace and spends a read.
func (s *Server) readRoute(h func(http.ResponseWriter, *http.Request, *store.WorkspaceTree)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := s.Store.GetWorkspaceTree(r.Context(), r.PathValue("id"))
		if err != nil {
			wsFail(w, err)
			return
		}
		if !s.allow(w, r, t.ID, "read", s.rate(s.WorkspaceReadRate, readRate)) {
			return
		}
		h(w, r, t)
	}
}

// chatAgent reports whether a configured agent has workspaces.
func (s *Server) chatAgent(name string) bool {
	a, ok := s.Engine.Current().Config.Agents[name]
	return ok && a.Chat
}

// conversationWorkspace is GET /conversations/{id}/workspace.
func (s *Server) conversationWorkspace(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		wsFail(w, err)
		return
	}
	s.serveConversationWorkspace(w, r, c)
}

// agentWorkspace is GET /agents/{name}/workspace: on the snapshot backend,
// the workspace of the agent's default conversation, where its agent-level
// routes speak.
func (s *Server) agentWorkspace(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if _, ok := s.Engine.Current().Config.Agents[name]; !ok {
		wsFail(w, store.ErrNotFound)
		return
	}
	id, err := s.Store.DefaultConversationID(r.Context(), name)
	if err != nil {
		wsFail(w, err)
		return
	}
	c, err := s.Store.GetConversation(r.Context(), id)
	if err != nil {
		wsFail(w, err)
		return
	}
	s.serveConversationWorkspace(w, r, c)
}

func (s *Server) serveConversationWorkspace(w http.ResponseWriter, r *http.Request, c *store.Conversation) {
	if c.WorkspaceID == nil && !s.chatAgent(c.Agent) {
		respond(w, map[string]any{"workspace": nil}, nil)
		return
	}
	t, err := s.Store.ConversationWorkspace(r.Context(), c.ID)
	if err != nil {
		wsFail(w, err)
		return
	}
	if !s.allow(w, r, t.ID, "read", s.rate(s.WorkspaceReadRate, readRate)) {
		return
	}
	doc, err := s.workspaceDoc(r, t)
	if err != nil {
		wsFail(w, err)
		return
	}
	respond(w, map[string]any{"workspace": doc}, nil)
}

// workspaceDoc is a workspace as GET .../workspace returns it.
func (s *Server) workspaceDoc(r *http.Request, t *store.WorkspaceTree) (*workspaceDoc, error) {
	caps := snapshotCapabilities
	caps.Write = caps.Write && s.inWorkspaceGroup(r)
	doc := &workspaceDoc{ID: t.ID, Scope: t.Scope, ConversationID: t.ConversationID, Agent: t.Agent,
		Backend: backendSnapshot, Cwd: "", Capabilities: caps, CreatedAt: t.CreatedAt}
	meta, err := s.Store.LatestSnapshot(r.Context(), *t.ConversationID)
	switch {
	case errors.Is(err, store.ErrNotFound):
	case err != nil:
		return nil, err
	default:
		doc.AsOf = &asOfDoc{SnapshotID: meta.ID, RunID: meta.RunID, At: meta.CreatedAt}
	}
	return doc, nil
}

// withWorkspaceIDs fills workspace_id on conversations of chat agents,
// giving each one its workspace if it has none yet.
func (s *Server) withWorkspaceIDs(ctx context.Context, convs []store.Conversation) error {
	var ids []string
	for _, c := range convs {
		if c.WorkspaceID == nil && s.chatAgent(c.Agent) {
			ids = append(ids, c.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	got, err := s.Store.EnsureConversationWorkspaces(ctx, ids)
	if err != nil {
		return err
	}
	for i := range convs {
		if id, ok := got[convs[i].ID]; ok && convs[i].WorkspaceID == nil {
			convs[i].WorkspaceID = &id
		}
	}
	return nil
}

// pathParam reads and checks a path query parameter.
func pathParam(w http.ResponseWriter, r *http.Request, name string) (string, bool) {
	p, err := workspace.CleanPath(r.URL.Query().Get(name))
	if err != nil {
		wsErr(w, http.StatusBadRequest, "path", name+": "+strings.TrimPrefix(err.Error(), "path: "))
		return "", false
	}
	return p, true
}

// noRef answers a ref: a past version comes from history, which the
// snapshot backend does not serve yet, so no ref names one.
func noRef(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Query().Get("ref") != "" {
		wsErr(w, http.StatusNotFound, "not_found", "no such ref")
		return true
	}
	return false
}

func entryOf(snap *workspace.Snapshot, e *workspace.Entry, withVersion bool) entryDoc {
	d := entryDoc{Path: e.Path, Name: e.Name(), Kind: e.Kind, Bytes: e.Bytes,
		Mode: fmt.Sprintf("%04o", e.Mode.Perm()), ModifiedAt: e.ModTime, Ignored: snap.Ignored(e)}
	if e.Path == "" {
		d.Name = ""
	}
	switch e.Kind {
	case workspace.KindSymlink:
		t := e.Target
		d.Target = &t
	case workspace.KindDir:
		h := snap.HasChildren(e.Path)
		d.HasChildren = &h
	case workspace.KindFile:
		if withVersion {
			d.Version = snap.VersionOf(e)
		}
	}
	return d
}

// cursorKey orders a directory's entries as the index does: directories
// first, then by name. A tree cursor is the last key of a page.
func cursorKey(e *workspace.Entry) string {
	if e.Kind == workspace.KindDir {
		return "0" + e.Name()
	}
	return "1" + e.Name()
}

// workspaceTree is GET /workspaces/{id}/tree: one directory, a page.
func (s *Server) workspaceTree(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	p, ok := pathParam(w, r, "path")
	if !ok {
		return
	}
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	if limit <= 0 {
		limit = treeLimit
	}
	limit = min(limit, treeLimitMax)
	var after string
	if c := r.URL.Query().Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		if err != nil || len(b) < 2 || (b[0] != '0' && b[0] != '1') {
			wsErr(w, http.StatusBadRequest, "request", "cursor: not a cursor from next_cursor")
			return
		}
		after = string(b)
	}
	snap, _, err := s.snapshot(r.Context(), t)
	if err != nil {
		wsFail(w, err)
		return
	}
	dir, err := snap.Lookup(p)
	if err != nil {
		wsFail(w, err)
		return
	}
	if dir.Kind != workspace.KindDir {
		wsErr(w, http.StatusBadRequest, "path", fmt.Sprintf("path: %q is not a directory", p))
		return
	}
	children := snap.Children(p)
	start := 0
	if after != "" {
		start = sort.Search(len(children), func(i int) bool { return cursorKey(children[i]) > after })
	}
	end := min(len(children), start+limit)
	entries := make([]entryDoc, 0, end-start)
	for _, e := range children[start:end] {
		entries = append(entries, entryOf(snap, e, false))
	}
	var next any
	if end < len(children) {
		next = base64.RawURLEncoding.EncodeToString([]byte(cursorKey(children[end-1])))
	}
	respond(w, map[string]any{"path": p, "entries": entries, "next_cursor": next}, nil)
}

// workspaceStat is GET /workspaces/{id}/stat.
func (s *Server) workspaceStat(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	p, ok := pathParam(w, r, "path")
	if !ok {
		return
	}
	snap, _, err := s.snapshot(r.Context(), t)
	if err != nil {
		wsFail(w, err)
		return
	}
	e, err := snap.Lookup(p)
	if err != nil {
		wsFail(w, err)
		return
	}
	respond(w, map[string]any{"entry": entryOf(snap, e, true)}, nil)
}

// fileOf resolves a path to a regular file, or answers why it is not one.
func fileOf(w http.ResponseWriter, snap *workspace.Snapshot, p string) (*workspace.Entry, bool) {
	e, err := snap.Lookup(p)
	if err != nil {
		wsFail(w, err)
		return nil, false
	}
	switch e.Kind {
	case workspace.KindDir:
		wsErr(w, http.StatusBadRequest, "path", fmt.Sprintf("path: %q is a directory", p))
		return nil, false
	case workspace.KindSymlink:
		wsErr(w, http.StatusBadRequest, "path", fmt.Sprintf("path: %q is a symlink, which is not followed", p))
		return nil, false
	}
	return e, true
}

// workspaceRead is GET /workspaces/{id}/files: a file for the editor.
func (s *Server) workspaceRead(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	p, ok := pathParam(w, r, "path")
	if !ok || noRef(w, r) {
		return
	}
	snap, _, err := s.snapshot(r.Context(), t)
	if err != nil {
		wsFail(w, err)
		return
	}
	e, ok := fileOf(w, snap, p)
	if !ok {
		return
	}
	doc := entryOf(snap, e, true)
	resp := map[string]any{"entry": doc, "reason": nil}
	switch {
	case e.Bytes > EditorReadMax:
		resp["reason"] = "too_large"
	case !workspace.IsText(e.Data()):
		resp["reason"] = "binary"
	default:
		resp["text"] = string(e.Data())
	}
	w.Header().Set("ETag", strconv.Quote(doc.Version))
	respond(w, resp, nil)
}

// workspaceDownload is GET /workspaces/{id}/download: a file as a
// download, a raster image inline when asked, a directory as a tar.gz.
func (s *Server) workspaceDownload(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	p, ok := pathParam(w, r, "path")
	if !ok || noRef(w, r) {
		return
	}
	snap, _, err := s.snapshot(r.Context(), t)
	if err != nil {
		wsFail(w, err)
		return
	}
	e, err := snap.Lookup(p)
	if err != nil {
		wsFail(w, err)
		return
	}
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; sandbox")
	switch e.Kind {
	case workspace.KindSymlink:
		wsErr(w, http.StatusBadRequest, "path", fmt.Sprintf("path: %q is a symlink, which is not followed", p))
	case workspace.KindDir:
		name := "workspace"
		if p != "" {
			name = path.Base(p)
		}
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": name + ".tar.gz"}))
		if err := snap.WriteTarGz(w, e); err != nil {
			s.Log.Warn("workspace download", "workspace", t.ID, "path", p, "err", err)
		}
	default:
		b := e.Data()
		disp, mt := "attachment", "application/octet-stream"
		if r.URL.Query().Get("inline") == "1" {
			if sniffed := http.DetectContentType(b); rasterImage(sniffed) {
				disp, mt = "inline", sniffed
			}
		}
		w.Header().Set("Content-Type", mt)
		w.Header().Set("Content-Disposition", mime.FormatMediaType(disp, map[string]string{"filename": path.Base(p)}))
		w.Header().Set("ETag", strconv.Quote(snap.VersionOf(e)))
		w.Header().Set("Content-Length", strconv.Itoa(len(b)))
		_, _ = w.Write(b)
	}
}

// rasterImage is a media type a browser shows as a picture and never runs:
// an image, not SVG (which can carry script).
func rasterImage(mt string) bool {
	mt, _, _ = mime.ParseMediaType(mt)
	return strings.HasPrefix(mt, "image/") && mt != "image/svg+xml"
}

// workspaceEvents is GET /workspaces/{id}/events: the audit, newest first.
func (s *Server) workspaceEvents(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
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
	page, err := s.Store.ListWorkspaceEvents(r.Context(), t.ID, before, limit)
	if err != nil {
		wsFail(w, err)
		return
	}
	resp := map[string]any{"events": orEmpty(page.Events), "next_before": nil}
	if page.HasMore {
		resp["next_before"] = strconv.FormatInt(page.NextBefore, 10)
	}
	respond(w, resp, nil)
}
