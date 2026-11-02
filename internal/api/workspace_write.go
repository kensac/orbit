package api

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/workspace"
)

// Workspace writes (docs/api.md, Workspace, Files). On the snapshot backend
// each is a new snapshot authored by the person, made from the newest one
// and stored only if that is still the newest and no run of the
// conversation is running (store.PutPersonWrite). Nothing is deleted: a
// trashed path stays in the snapshot it was in.

// Write limits (docs/workspace.md, Auth, scope and limits).
const (
	// EditorWriteMax is the largest body PUT /files takes.
	EditorWriteMax = 8 << 20
	// WorkspaceUploadMax is the largest file of a multipart upload.
	WorkspaceUploadMax = 64 << 20
	// WorkspaceSignedMax is the largest file a signed upload URL is issued
	// for; the snapshot backend also holds the whole tree to
	// workspace.MaxBytes.
	WorkspaceSignedMax = 1 << 30
	// writeAttempts is how often a write is applied again when another
	// write stored a snapshot between its read and its commit.
	writeAttempts = 3
)

// wsError is a workspace error an operation returns: written as is.
type wsError struct {
	status int
	check  string
	msg    string
	extra  map[string]any
}

func (e *wsError) Error() string { return e.msg }

func wsErrorf(status int, check, format string, a ...any) *wsError {
	return &wsError{status: status, check: check, msg: fmt.Sprintf(format, a...)}
}

// editErr turns an Edit's error into the workspace error it is.
func editErr(err error) error {
	var we *wsError
	switch {
	case err == nil, errors.As(err, &we):
		return err
	case errors.Is(err, workspace.ErrExists):
		return wsErrorf(http.StatusConflict, "exists", "%s", err.Error())
	case errors.Is(err, workspace.ErrPath):
		return wsErrorf(http.StatusBadRequest, "path", "%s", err.Error())
	case errors.Is(err, fs.ErrNotExist):
		return wsErrorf(http.StatusNotFound, "not_found", "not found")
	}
	return err
}

// plan is what an operation decided: the edited tree and what to record.
type plan struct {
	edit     *workspace.Edit
	events   []store.WorkspaceEventIn
	trash    []store.TrashIn
	restored string
	// reply builds the response from the stored snapshot.
	reply  func(after *workspace.Snapshot, res *store.WriteResult) any
	status int
}

// writeRoute resolves {id}, checks the workspace group, spends a write and
// runs h.
func (s *Server) writeRoute(h func(http.ResponseWriter, *http.Request, *store.WorkspaceTree)) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		t, err := s.Store.GetWorkspaceTree(r.Context(), r.PathValue("id"))
		if err != nil {
			wsFail(w, err)
			return
		}
		if !s.inWorkspaceGroup(r) {
			wsErr(w, http.StatusForbidden, "group", fmt.Sprintf("changing a workspace needs membership of %q", s.WorkspaceGroup))
			return
		}
		if !s.allow(w, r, t.ID, "write", s.rate(s.WorkspaceWriteRate, writeRate)) {
			return
		}
		h(w, r, t)
	}
}

// requestID is the request's X-Request-Id when it sent a usable one, else a
// new one; it is echoed back and recorded with each event.
func requestID(w http.ResponseWriter, r *http.Request) string {
	id := r.Header.Get("X-Request-Id")
	ok := id != "" && len(id) <= 128
	for _, c := range id {
		if c < 0x21 || c > 0x7e {
			ok = false
		}
	}
	if !ok {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		id = hex.EncodeToString(b)
	}
	w.Header().Set("X-Request-Id", id)
	return id
}

// commit applies op to the newest snapshot and stores the result as the
// person's, again from the newest when another write got in first.
func (s *Server) commit(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree, person string,
	op func(snap *workspace.Snapshot, base *store.Workspace) (*plan, error)) {
	if t.ConversationID == nil {
		wsErr(w, http.StatusConflict, "read_only", "this workspace's backend cannot be written here")
		return
	}
	reqID := requestID(w, r)
	for range writeAttempts {
		snap, base, err := s.snapshot(r.Context(), t)
		if err != nil {
			wsFail(w, err)
			return
		}
		p, err := op(snap, base)
		if err = editErr(err); err != nil {
			writeWsError(w, err)
			return
		}
		archive, files, err := p.edit.Archive()
		if errors.Is(err, workspace.ErrTooLarge) {
			wsErr(w, http.StatusRequestEntityTooLarge, "too_large", err.Error())
			return
		}
		if err != nil {
			wsFail(w, err)
			return
		}
		var baseID int64
		if base != nil {
			baseID = base.ID
		}
		res, err := s.Store.PutPersonWrite(r.Context(), store.PersonWrite{WorkspaceID: t.ID, ConversationID: *t.ConversationID,
			BaseID: baseID, Archive: archive, Files: files, Person: person, RequestID: reqID,
			Events: p.events, Trash: p.trash, Restored: p.restored})
		switch {
		case errors.Is(err, store.ErrStale):
			continue
		case errors.Is(err, store.ErrWorkspaceBusy):
			wsErr(w, http.StatusConflict, "busy", err.Error())
			return
		case errors.Is(err, store.ErrConflict):
			wsErr(w, http.StatusConflict, "exists", "this upload link has been used")
			return
		case err != nil:
			wsFail(w, err)
			return
		}
		after, err := workspace.Open(archive, hiddenFile)
		if err != nil {
			wsFail(w, err)
			return
		}
		s.cacheSnapshot(res.Snapshot.ID, after)
		respondStatus(w, p.status, p.reply(after, res), nil)
		return
	}
	wsErr(w, http.StatusConflict, "busy", "the workspace kept changing; try again")
}

func writeWsError(w http.ResponseWriter, err error) {
	var we *wsError
	if errors.As(err, &we) {
		wsErrWith(w, we.status, we.check, we.msg, we.extra)
		return
	}
	wsFail(w, err)
}

// writablePath reads a path parameter a write targets: clean, not the
// root, not orbit's own state.
func writablePath(p, name string) (string, error) {
	p, err := workspace.CleanPath(p)
	switch {
	case err != nil:
		return "", wsErrorf(http.StatusBadRequest, "path", "%s: %s", name, strings.TrimPrefix(err.Error(), "path: "))
	case p == "":
		return "", wsErrorf(http.StatusBadRequest, "path", "%s: the root cannot be written", name)
	case hiddenFile(p):
		return "", wsErrorf(http.StatusBadRequest, "path", "%s: %q is orbit's own state", name, p)
	}
	return p, nil
}

// etag is a version as a client sends it back: quoted or bare, a weak
// prefix ignored.
func etag(h string) string {
	h = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(h), "W/"))
	if u, err := strconv.Unquote(h); err == nil {
		return u
	}
	return h
}

// versionMismatch is the 412 for a version that is not the current one:
// the body carries the current entry, or null when the path is gone.
func versionMismatch(snap *workspace.Snapshot, e *workspace.Entry, p string) error {
	var cur any
	if e != nil {
		cur = entryOf(snap, e, true)
	}
	return &wsError{status: http.StatusPreconditionFailed, check: "version", extra: map[string]any{"current": cur},
		msg: fmt.Sprintf("%q is not at the version given", p)}
}

// guard checks a version a client gives for a path: "" passes; any other
// must be the path's current file version.
func guard(snap *workspace.Snapshot, e *workspace.Entry, p, version string) error {
	if version == "" {
		return nil
	}
	if e == nil || e.Kind != workspace.KindFile || snap.VersionOf(e) != etag(version) {
		return versionMismatch(snap, e, p)
	}
	return nil
}

// lookup is Snapshot.Lookup with a missing path as nil.
func lookup(snap *workspace.Snapshot, p string) (*workspace.Entry, error) {
	e, err := snap.Lookup(p)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return e, err
}

func size(n int) *int64 {
	v := int64(n)
	return &v
}

// subtreeBytes is the bytes of the files at and under e.
func subtreeBytes(snap *workspace.Snapshot, e *workspace.Entry) int64 {
	var n int64
	_ = snap.Walk(e, func(x *workspace.Entry) error {
		n += x.Bytes
		return nil
	})
	return n
}

func entryReply(p string) func(*workspace.Snapshot, *store.WriteResult) any {
	return func(after *workspace.Snapshot, _ *store.WriteResult) any {
		e, err := after.Lookup(p)
		if err != nil {
			return map[string]any{"entry": nil}
		}
		return map[string]any{"entry": entryOf(after, e, true)}
	}
}

// workspaceWriteRoutes adds the write routes.
func (s *Server) workspaceWriteRoutes(api func(string, http.HandlerFunc), mux *http.ServeMux) {
	api("PUT /api/v1/workspaces/{id}/files", s.writeRoute(s.workspaceWrite))
	api("POST /api/v1/workspaces/{id}/mkdir", s.writeRoute(s.workspaceMkdir))
	api("POST /api/v1/workspaces/{id}/rename", s.writeRoute(s.workspaceRename))
	api("POST /api/v1/workspaces/{id}/trash", s.writeRoute(s.workspaceTrash))
	api("GET /api/v1/workspaces/{id}/trash", s.readRoute(s.workspaceTrashList))
	api("POST /api/v1/workspaces/{id}/trash/{item}/restore", s.writeRoute(s.workspaceRestore))
	api("POST /api/v1/workspaces/{id}/upload", s.writeRoute(s.workspaceUpload))
	api("POST /api/v1/workspaces/{id}/upload/url", s.writeRoute(s.workspaceUploadURL))
	// The signature in the URL is the credential (workspaceSignedUpload).
	mux.HandleFunc("PUT /api/v1/workspaces/signed", s.workspaceSignedUpload)
}

// workspaceWrite is PUT /workspaces/{id}/files: the body is the file.
func (s *Server) workspaceWrite(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	p, err := writablePath(r.URL.Query().Get("path"), "path")
	if err != nil {
		writeWsError(w, err)
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, EditorWriteMax))
	var tooBig *http.MaxBytesError
	if errors.As(err, &tooBig) {
		wsErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a write is at most %d bytes", EditorWriteMax))
		return
	}
	if err != nil {
		wsErr(w, http.StatusBadRequest, "request", err.Error())
		return
	}
	ifMatch, ifNone := r.Header.Get("If-Match"), strings.TrimSpace(r.Header.Get("If-None-Match"))
	s.commit(w, r, t, principalOf(r).Person, func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		e, err := lookup(snap, p)
		if err != nil {
			return nil, err
		}
		ev := store.WorkspaceEventIn{Op: "write", Path: p, VersionAfter: workspace.Version(body), Bytes: size(len(body))}
		status := http.StatusCreated
		switch {
		case e != nil && e.Kind != workspace.KindFile:
			return nil, wsErrorf(http.StatusBadRequest, "path", "path: %q is a %s", p, e.Kind)
		case e != nil && ifNone == "*":
			return nil, &wsError{status: http.StatusConflict, check: "exists", msg: fmt.Sprintf("%q exists", p),
				extra: map[string]any{"current": entryOf(snap, e, true)}}
		case e != nil && ifMatch == "":
			return nil, wsErrorf(http.StatusPreconditionRequired, "version_required", "%q exists: send If-Match with the version you read", p)
		case e != nil && etag(ifMatch) != "*" && etag(ifMatch) != snap.VersionOf(e):
			return nil, versionMismatch(snap, e, p)
		case e == nil && ifMatch != "":
			return nil, versionMismatch(snap, nil, p)
		case e != nil:
			ev.VersionBefore, status = snap.VersionOf(e), http.StatusOK
		}
		edit := snap.Edit(time.Now())
		if err := edit.PutFile(p, body); err != nil {
			return nil, err
		}
		return &plan{edit: edit, events: []store.WorkspaceEventIn{ev}, status: status, reply: entryReply(p)}, nil
	})
}

// workspaceMkdir is POST /workspaces/{id}/mkdir.
func (s *Server) workspaceMkdir(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	var body struct {
		Path string `json:"path"`
	}
	if !wsDecode(w, r, &body, bodyRequired) {
		return
	}
	p, err := writablePath(body.Path, "path")
	if err != nil {
		writeWsError(w, err)
		return
	}
	s.commit(w, r, t, principalOf(r).Person, func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		edit := snap.Edit(time.Now())
		if err := edit.Mkdir(p); err != nil {
			return nil, err
		}
		return &plan{edit: edit, events: []store.WorkspaceEventIn{{Op: "mkdir", Path: p}}, status: http.StatusCreated, reply: entryReply(p)}, nil
	})
}

// wsDecode is decode for the workspace routes: a bad body is a 400 with
// check "request".
func wsDecode(w http.ResponseWriter, r *http.Request, into any, rule bodyRule) bool {
	return decodeOr(w, r, 16<<10, into, rule, func(msg string) { wsErr(w, http.StatusBadRequest, "request", msg) })
}

// workspaceRename is POST /workspaces/{id}/rename.
func (s *Server) workspaceRename(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	var body struct {
		From    string `json:"from"`
		To      string `json:"to"`
		Version string `json:"version"`
	}
	if !wsDecode(w, r, &body, bodyRequired) {
		return
	}
	from, err := writablePath(body.From, "from")
	if err == nil {
		body.To, err = writablePath(body.To, "to")
	}
	if err != nil {
		writeWsError(w, err)
		return
	}
	to := body.To
	s.commit(w, r, t, principalOf(r).Person, func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		e, err := snap.Lookup(from)
		if err != nil {
			return nil, err
		}
		if err := guard(snap, e, from, body.Version); err != nil {
			return nil, err
		}
		ev := store.WorkspaceEventIn{Op: "rename", Path: to, From: from}
		if e.Kind == workspace.KindFile {
			ev.VersionBefore = snap.VersionOf(e)
			ev.VersionAfter = ev.VersionBefore
		}
		edit := snap.Edit(time.Now())
		if err := edit.Move(from, to); err != nil {
			return nil, err
		}
		return &plan{edit: edit, events: []store.WorkspaceEventIn{ev}, status: http.StatusOK, reply: entryReply(to)}, nil
	})
}

// trashOne plans moving e to the trash.
func trashOne(snap *workspace.Snapshot, edit *workspace.Edit, e *workspace.Entry) (store.TrashIn, store.WorkspaceEventIn, error) {
	n := subtreeBytes(snap, e)
	ev := store.WorkspaceEventIn{Op: "trash", Path: e.Path, Bytes: &n}
	if e.Kind == workspace.KindFile {
		ev.VersionBefore = snap.VersionOf(e)
	}
	return store.TrashIn{Path: e.Path, Kind: e.Kind, Bytes: n}, ev, edit.Remove(e.Path)
}

// workspaceTrash is POST /workspaces/{id}/trash.
func (s *Server) workspaceTrash(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	var body struct {
		Path    string `json:"path"`
		Version string `json:"version"`
	}
	if !wsDecode(w, r, &body, bodyRequired) {
		return
	}
	p, err := writablePath(body.Path, "path")
	if err != nil {
		writeWsError(w, err)
		return
	}
	s.commit(w, r, t, principalOf(r).Person, func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		e, err := snap.Lookup(p)
		if err != nil {
			return nil, err
		}
		if err := guard(snap, e, p, body.Version); err != nil {
			return nil, err
		}
		edit := snap.Edit(time.Now())
		item, ev, err := trashOne(snap, edit, e)
		if err != nil {
			return nil, err
		}
		return &plan{edit: edit, events: []store.WorkspaceEventIn{ev}, trash: []store.TrashIn{item}, status: http.StatusOK,
			reply: func(_ *workspace.Snapshot, res *store.WriteResult) any { return map[string]any{"trash": res.Trash[0]} }}, nil
	})
}

// trashCursor encodes a trash position for a client, which treats it as
// opaque.
func trashCursor(c *store.TrashCursor) string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.At.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

// workspaceTrashList is GET /workspaces/{id}/trash.
func (s *Server) workspaceTrashList(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	var after *store.TrashCursor
	if c := r.URL.Query().Get("cursor"); c != "" {
		b, err := base64.RawURLEncoding.DecodeString(c)
		at, id, ok := strings.Cut(string(b), "|")
		tm, terr := time.Parse(time.RFC3339Nano, at)
		if err != nil || !ok || terr != nil || !store.IsUUID(id) {
			wsErr(w, http.StatusBadRequest, "request", "cursor: not a cursor from next_cursor")
			return
		}
		after = &store.TrashCursor{At: tm, ID: id}
	}
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	items, next, err := s.Store.ListTrash(r.Context(), t.ID, after, limit)
	if err != nil {
		wsFail(w, err)
		return
	}
	resp := map[string]any{"items": orEmpty(items), "next_cursor": nil}
	if next != nil {
		resp["next_cursor"] = trashCursor(next)
	}
	respond(w, resp, nil)
}

// workspaceRestore is POST /workspaces/{id}/trash/{item}/restore: the item
// back at its path or at to, from the snapshot that holds it.
func (s *Server) workspaceRestore(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	var body struct {
		To string `json:"to"`
	}
	if !wsDecode(w, r, &body, bodyOptional) {
		return
	}
	item, err := s.Store.GetTrashItem(r.Context(), t.ID, r.PathValue("item"))
	if err != nil {
		wsFail(w, err)
		return
	}
	to := item.Path
	if body.To != "" {
		if to, err = writablePath(body.To, "to"); err != nil {
			writeWsError(w, err)
			return
		}
	}
	src, err := s.snapshotByID(r.Context(), item.SnapshotID)
	if err != nil {
		wsFail(w, err)
		return
	}
	s.commit(w, r, t, principalOf(r).Person, func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		edit := snap.Edit(time.Now())
		if err := edit.Insert(src, item.Path, to); err != nil {
			return nil, err
		}
		ev := store.WorkspaceEventIn{Op: "restore", Path: to, From: item.Path, Bytes: &item.Bytes}
		if e, err := src.Lookup(item.Path); err == nil && e.Kind == workspace.KindFile {
			ev.VersionAfter = src.VersionOf(e)
		}
		return &plan{edit: edit, events: []store.WorkspaceEventIn{ev}, restored: item.ID, status: http.StatusOK, reply: entryReply(to)}, nil
	})
}

// uploadPart is one file of a multipart upload.
type uploadPart struct {
	name string
	data []byte
}

// workspaceUpload is POST /workspaces/{id}/upload?dir=&overwrite=1: every
// file part, into one snapshot.
func (s *Server) workspaceUpload(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	dir, err := workspace.CleanPath(r.URL.Query().Get("dir"))
	if err == nil && dir != "" && hiddenFile(dir) {
		err = fmt.Errorf("%q is orbit's own state", dir)
	}
	if err != nil {
		wsErr(w, http.StatusBadRequest, "path", "dir: "+strings.TrimPrefix(err.Error(), "path: "))
		return
	}
	overwrite := r.URL.Query().Get("overwrite") == "1"
	r.Body = http.MaxBytesReader(w, r.Body, workspace.MaxBytes+multipartOverhead)
	mr, err := r.MultipartReader()
	if err != nil {
		wsErr(w, http.StatusBadRequest, "request", "want multipart/form-data with files: "+err.Error())
		return
	}
	var parts []uploadPart
	seen := map[string]bool{}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			wsErr(w, http.StatusRequestEntityTooLarge, "too_large", "the upload is larger than the workspace can hold")
			return
		}
		if err != nil {
			wsErr(w, http.StatusBadRequest, "request", err.Error())
			return
		}
		if part.FileName() == "" {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(part, WorkspaceUploadMax+1))
		if errors.As(err, &tooBig) {
			wsErr(w, http.StatusRequestEntityTooLarge, "too_large", "the upload is larger than the workspace can hold")
			return
		}
		if err != nil {
			wsErr(w, http.StatusBadRequest, "request", err.Error())
			return
		}
		if len(b) > WorkspaceUploadMax {
			wsErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("a file is at most %d bytes; use upload/url", WorkspaceUploadMax))
			return
		}
		name := store.UploadName(part.FileName())
		if seen[name] {
			wsErr(w, http.StatusBadRequest, "request", fmt.Sprintf("%q is in the form twice", name))
			return
		}
		seen[name] = true
		parts = append(parts, uploadPart{name: name, data: b})
	}
	if len(parts) == 0 {
		wsErr(w, http.StatusBadRequest, "request", "no file in the form")
		return
	}
	s.commit(w, r, t, principalOf(r).Person, func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		edit := snap.Edit(time.Now())
		pl := &plan{edit: edit, status: http.StatusCreated}
		var paths []string
		for _, part := range parts {
			p := workspace.Join(dir, part.name)
			if hiddenFile(p) {
				return nil, wsErrorf(http.StatusBadRequest, "path", "%q is orbit's own state", p)
			}
			e, err := lookup(snap, p)
			if err != nil {
				return nil, err
			}
			if e != nil {
				if e.Kind != workspace.KindFile || !overwrite {
					return nil, &wsError{status: http.StatusConflict, check: "exists", msg: fmt.Sprintf("%q exists", p),
						extra: map[string]any{"current": entryOf(snap, e, true)}}
				}
				item, ev, err := trashOne(snap, edit, e)
				if err != nil {
					return nil, err
				}
				pl.trash, pl.events = append(pl.trash, item), append(pl.events, ev)
			}
			if err := edit.PutFile(p, part.data); err != nil {
				return nil, err
			}
			pl.events = append(pl.events, store.WorkspaceEventIn{Op: "upload", Path: p, VersionAfter: workspace.Version(part.data), Bytes: size(len(part.data))})
			paths = append(paths, p)
		}
		pl.reply = func(after *workspace.Snapshot, _ *store.WriteResult) any {
			out := []entryDoc{}
			for _, p := range paths {
				if e, err := after.Lookup(p); err == nil {
					out = append(out, entryOf(after, e, true))
				}
			}
			return map[string]any{"entries": out}
		}
		return pl, nil
	})
}

// workspaceUploadKey keys workspace upload URLs from the operator token,
// apart from conversation uploads' key, so neither kind of URL verifies as
// the other.
func (s *Server) workspaceUploadKey() []byte {
	m := hmac.New(sha256.New, []byte(s.Token))
	m.Write([]byte("orbit workspace upload url"))
	return m.Sum(nil)
}

func (s *Server) workspaceUploadSig(v url.Values) string {
	m := hmac.New(sha256.New, s.workspaceUploadKey())
	for _, k := range []string{"w", "p", "t", "b", "e", "a", "k"} {
		m.Write([]byte(k + "=" + v.Get(k) + "\x00"))
	}
	return hex.EncodeToString(m.Sum(nil))
}

// workspaceUploadURL is POST /workspaces/{id}/upload/url: a signed URL the
// browser PUTs one file to, for a file larger than its proxy takes.
func (s *Server) workspaceUploadURL(w http.ResponseWriter, r *http.Request, t *store.WorkspaceTree) {
	var body struct {
		Path      string `json:"path"`
		Bytes     int64  `json:"bytes"`
		MediaType string `json:"media_type"`
	}
	if !wsDecode(w, r, &body, bodyRequired) {
		return
	}
	p, err := writablePath(body.Path, "path")
	if err != nil {
		writeWsError(w, err)
		return
	}
	switch {
	case body.Bytes < 0 || body.Bytes > WorkspaceSignedMax:
		wsErr(w, http.StatusBadRequest, "request", fmt.Sprintf("bytes must be 0 to %d", WorkspaceSignedMax))
		return
	case body.Bytes > workspace.MaxBytes:
		wsErr(w, http.StatusRequestEntityTooLarge, "too_large", fmt.Sprintf("this workspace holds at most %d bytes", workspace.MaxBytes))
		return
	}
	mt := strings.TrimSpace(body.MediaType)
	if mt == "" {
		mt = "application/octet-stream"
	}
	if _, _, err := mime.ParseMediaType(mt); err != nil {
		wsErr(w, http.StatusBadRequest, "request", "media_type: "+err.Error())
		return
	}
	snap, _, err := s.snapshot(r.Context(), t)
	if err != nil {
		wsFail(w, err)
		return
	}
	if e, err := lookup(snap, p); err != nil || e != nil {
		if err == nil {
			err = &wsError{status: http.StatusConflict, check: "exists", msg: fmt.Sprintf("%q exists", p),
				extra: map[string]any{"current": entryOf(snap, e, true)}}
		}
		writeWsError(w, editErr(err))
		return
	}
	if len(s.Token) == 0 {
		wsErr(w, http.StatusServiceUnavailable, "request", "signed uploads need ORBIT_API_TOKEN")
		return
	}
	base := s.publicURL()
	if base == "" {
		wsErr(w, http.StatusServiceUnavailable, "request", "signed uploads need public_url in orbit.yaml")
		return
	}
	nonce := make([]byte, 16)
	_, _ = rand.Read(nonce)
	exp := time.Now().Add(UploadURLTTL).Truncate(time.Second)
	v := url.Values{}
	v.Set("w", t.ID)
	v.Set("p", p)
	v.Set("t", mt)
	v.Set("b", strconv.FormatInt(body.Bytes, 10))
	v.Set("e", strconv.FormatInt(exp.Unix(), 10))
	v.Set("a", principalOf(r).Person)
	v.Set("k", hex.EncodeToString(nonce))
	v.Set("s", s.workspaceUploadSig(v))
	respond(w, map[string]any{
		"url": base + "/api/v1/workspaces/signed?" + v.Encode(), "method": http.MethodPut,
		"headers": map[string]string{"Content-Type": mt}, "expires_at": exp, "max_bytes": body.Bytes,
	}, nil)
}

// workspaceSignedUpload is PUT /api/v1/workspaces/signed: the body is the
// file. The signature stands in for a token: it names one workspace, one
// path, a size bound, an expiry and the person it was issued to (who was in
// the workspace group then).
func (s *Server) workspaceSignedUpload(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	if len(s.Token) == 0 || !hmac.Equal([]byte(s.workspaceUploadSig(q)), []byte(q.Get("s"))) {
		writeErr(w, http.StatusForbidden, "the upload link is not valid")
		return
	}
	exp, _ := strconv.ParseInt(q.Get("e"), 10, 64)
	if time.Now().Unix() > exp {
		writeErr(w, http.StatusForbidden, "the upload link has expired")
		return
	}
	t, err := s.Store.GetWorkspaceTree(r.Context(), q.Get("w"))
	if err != nil {
		wsFail(w, err)
		return
	}
	limit, _ := strconv.ParseInt(q.Get("b"), 10, 64)
	b, err := io.ReadAll(io.LimitReader(r.Body, limit+1))
	if err != nil {
		wsErr(w, http.StatusBadRequest, "request", err.Error())
		return
	}
	if int64(len(b)) > limit {
		wsErr(w, http.StatusRequestEntityTooLarge, "too_large", "the file is larger than the link allows ("+q.Get("b")+" bytes)")
		return
	}
	p := q.Get("p")
	nonce := q.Get("k")
	s.commit(w, r, t, q.Get("a"), func(snap *workspace.Snapshot, _ *store.Workspace) (*plan, error) {
		e, err := lookup(snap, p)
		if err != nil {
			return nil, err
		}
		if e != nil {
			return nil, &wsError{status: http.StatusConflict, check: "exists", msg: fmt.Sprintf("%q exists", p),
				extra: map[string]any{"current": entryOf(snap, e, true)}}
		}
		edit := snap.Edit(time.Now())
		if err := edit.PutFile(p, b); err != nil {
			return nil, err
		}
		return &plan{edit: edit, status: http.StatusCreated, reply: entryReply(p),
			events: []store.WorkspaceEventIn{{Op: "upload", Path: p, VersionAfter: workspace.Version(b), Bytes: size(len(b)), Nonce: nonce}}}, nil
	})
}
