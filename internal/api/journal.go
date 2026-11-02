package api

import (
	"net/http"
	"strconv"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/journal"
)

// journalEpoch is the epoch a request names (?epoch=), or 0 for the run's
// latest.
func journalEpoch(r *http.Request) (int, bool) {
	q := r.URL.Query().Get("epoch")
	if q == "" {
		return 0, true
	}
	n, err := strconv.Atoi(q)
	return n, err == nil && n >= 1
}

// readJournal serves GET /runs/{id}/journal: one epoch's item revisions in
// order, after a cursor (docs/api.md).
func (s *Server) readJournal(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	epoch, ok := journalEpoch(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "epoch must be a positive integer")
		return
	}
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	if err := s.Store.SyncJournal(r.Context(), id); err != nil {
		respond(w, nil, err)
		return
	}
	h, err := s.Store.GetJournalHead(r.Context(), id, epoch)
	if err != nil {
		respond(w, nil, err)
		return
	}
	after, reset := 0, ""
	if q := r.URL.Query().Get("after"); q != "" {
		c, ok := journal.ParseCursor(q)
		if !ok {
			writeErr(w, http.StatusBadRequest, "after is not a journal cursor (epoch.seq)")
			return
		}
		after, reset = journal.Resume(c, h.Epoch, h.Last)
	}
	items, more, err := s.Store.ReadJournal(r.Context(), id, h.Epoch, after, limit)
	if err != nil {
		respond(w, nil, err)
		return
	}
	cursor := journal.Cursor{Epoch: h.Epoch, Seq: after}
	if len(items) > 0 {
		cursor.Seq = items[len(items)-1].Seq
	}
	var resetV any
	if reset != "" {
		resetV = reset
	}
	respond(w, map[string]any{"epoch": h.Epoch, "epochs": orEmpty(h.Epochs), "items": orEmpty(items),
		"cursor": cursor.String(), "has_more": more, "reset": resetV}, nil)
}

// journalItem serves GET /runs/{id}/journal/items/{item}: an item's latest
// revision.
func (s *Server) journalItem(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	epoch, ok := journalEpoch(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "epoch must be a positive integer")
		return
	}
	h, err := s.Store.GetJournalHead(r.Context(), id, epoch)
	if err != nil {
		respond(w, nil, err)
		return
	}
	it, err := s.Store.JournalItem(r.Context(), id, h.Epoch, r.PathValue("item"))
	respond(w, map[string]any{"item": it}, err)
}

// journalBody serves GET /runs/{id}/journal/bodies/{sha256}: the whole
// value behind a bounded head, as stored.
func (s *Server) journalBody(w http.ResponseWriter, r *http.Request) {
	b, err := s.Store.JournalBody(r.Context(), r.PathValue("id"), r.PathValue("sha256"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	ct := "application/octet-stream"
	if utf8.Valid(b) {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b)
}
