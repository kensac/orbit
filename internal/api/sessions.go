package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/store"
)

// sessionRoutes are imported sessions (docs/api.md, Imported sessions).
func (s *Server) sessionRoutes(api func(string, http.HandlerFunc)) {
	api("POST /api/v1/sessions/import", s.importSessions)
	api("GET /api/v1/sessions", s.listSessions)
	api("GET /api/v1/sessions/{id}", s.getSession)
	api("GET /api/v1/sessions/{id}/lines", s.sessionLines)
	api("POST /api/v1/sessions/{id}/learn", s.learnSession)
}

// learnSession queues a learning run of an imported session now, whatever its age.
func (s *Server) learnSession(w http.ResponseWriter, r *http.Request) {
	id, err := s.Engine.LearnSessionNow(r.Context(), r.PathValue("id"))
	if err == nil {
		s.Wake()
	}
	respondStatus(w, http.StatusAccepted, map[string]any{"run_id": id}, err)
}

// sessionImportBodyLimit bounds one import: orbit import claude-sessions sends at most importChunkBytes of lines (8 MiB) per request, and one line may be claudeimport.MaxSessionLine past that.
const sessionImportBodyLimit = 24 << 20

// sessionRepoMax bounds a session's repo name, as a client read it.
const sessionRepoMax = 400

type sessionChunkIn struct {
	SessionID  string            `json:"session_id"`
	Repo       string            `json:"repo"`
	FromOffset int64             `json:"from_offset"`
	FromPrefix string            `json:"from_prefix_sha256"`
	Rewrite    bool              `json:"rewrite"`
	ToOffset   int64             `json:"to_offset"`
	ToPrefix   string            `json:"to_prefix_sha256"`
	Lines      []json.RawMessage `json:"lines"`
}

func (s *Server) importSessions(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun   bool             `json:"dry_run"`
		Source   string           `json:"source"`
		Sessions []sessionChunkIn `json:"sessions"`
	}
	if !decode(w, r, sessionImportBodyLimit, &body, bodyRequired) {
		return
	}
	if body.Source == "" {
		body.Source = claudeimport.SourceClaudeCode
	}
	chunks := make([]store.SessionChunk, 0, len(body.Sessions))
	for i, in := range body.Sessions {
		if len(in.Repo) > sessionRepoMax {
			respond(w, nil, fmt.Errorf("%w: session %d: repo is longer than %d bytes", store.ErrInvalid, i, sessionRepoMax))
			return
		}
		c := store.SessionChunk{SessionID: in.SessionID, Repo: in.Repo, FromOffset: in.FromOffset, FromPrefix: in.FromPrefix,
			Rewrite: in.Rewrite, ToOffset: in.ToOffset, ToPrefix: in.ToPrefix, Lines: make([]store.SessionLine, 0, len(in.Lines))}
		for j, raw := range in.Lines {
			// The client normalizes too, so a credential never leaves the laptop; the server's own pass is what is stored.
			b, err := claudeimport.NormalizeLine(raw)
			if err != nil {
				respond(w, nil, fmt.Errorf("%w: session %d line %d: %w", store.ErrInvalid, i, j, err))
				return
			}
			m := claudeimport.ParseLineMeta(b)
			c.Lines = append(c.Lines, store.SessionLine{Content: b, Type: m.Type, Cwd: m.Cwd, Branch: m.Branch, At: m.At,
				Title: m.Title, Prompt: m.Prompt})
		}
		chunks = append(chunks, c)
	}
	res, err := s.Store.ImportSessions(r.Context(), body.Source, chunks, body.DryRun)
	respond(w, map[string]any{"dry_run": body.DryRun, "results": orEmpty(res)}, err)
}

func (s *Server) listSessions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.SessionFilter{Source: q.Get("source"), Repo: q.Get("repo")}
	if !queryInt(w, r, "limit", &f.Limit) {
		return
	}
	if b := q.Get("before"); b != "" {
		c, err := store.ParseConversationCursor(b)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "before: "+err.Error())
			return
		}
		f.Before = c
	}
	page, err := s.Store.ListExternalSessions(r.Context(), f)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, listPage("sessions", page.Sessions, page.HasMore, page.NextBefore), nil)
}

func (s *Server) getSession(w http.ResponseWriter, r *http.Request) {
	e, err := s.Store.GetExternalSession(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"session": e}, err)
}

func (s *Server) sessionLines(w http.ResponseWriter, r *http.Request) {
	var after, limit int
	if !queryInt(w, r, "after", &after) || !queryInt(w, r, "limit", &limit) {
		return
	}
	if limit <= 0 {
		limit = 200
	}
	lines, err := s.Store.ExternalSessionLines(r.Context(), r.PathValue("id"), after, limit)
	if errors.Is(err, store.ErrNotFound) {
		respond(w, nil, err)
		return
	}
	type line struct {
		N      int             `json:"n"`
		Digest string          `json:"digest"`
		Line   json.RawMessage `json:"line"`
	}
	out := make([]line, 0, len(lines))
	for _, l := range lines {
		out = append(out, line{N: l.N, Digest: l.Digest, Line: json.RawMessage(l.Content)})
	}
	respond(w, map[string]any{"lines": out}, err)
}
