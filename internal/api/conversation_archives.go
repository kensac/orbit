package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"

	"github.com/kensac/orbit/internal/store"
)

const conversationArchiveBodyLimit = 32 << 20

type conversationArchiveIn struct {
	SourceAgent          string          `json:"source_agent"`
	SourceConversationID string          `json:"source_conversation_id"`
	SourceBundleSHA256   string          `json:"source_bundle_sha256"`
	Snapshot             json.RawMessage `json:"snapshot"`
}

func (s *Server) conversationArchiveRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/v1/agents/{name}/conversation-archives", s.listConversationArchives)
	api("POST /api/v1/agents/{name}/conversation-archives/import", s.importConversationArchives)
	api("GET /api/v1/agents/{name}/conversation-archives/{id}", s.getConversationArchive)
}

func (s *Server) importConversationArchives(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun   bool                    `json:"dry_run"`
		Archives []conversationArchiveIn `json:"archives"`
	}
	if !decode(w, r, conversationArchiveBodyLimit, &body, bodyRequired) {
		return
	}
	target := r.PathValue("name")
	if _, ok := s.Engine.Current().Config.Agents[target]; !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	archives := make([]store.NewConversationArchive, 0, len(body.Archives))
	for i, input := range body.Archives {
		if err := store.ValidateConversationArchiveSnapshot(input.SourceConversationID, input.Snapshot); err != nil {
			respond(w, nil, fmt.Errorf("archive %d: %w", i, err))
			return
		}
		archives = append(archives, store.NewConversationArchive{TargetAgent: target,
			SourceAgent: input.SourceAgent, SourceConversationID: input.SourceConversationID,
			SourceBundleSHA256: input.SourceBundleSHA256, Snapshot: input.Snapshot,
			ImportedBy: actor(r, "")})
	}
	results, err := s.Store.ImportConversationArchives(r.Context(), archives, body.DryRun)
	respond(w, map[string]any{"dry_run": body.DryRun, "results": orEmpty(results)}, err)
}
func (s *Server) listConversationArchives(w http.ResponseWriter, r *http.Request) {
	var limit int
	if !queryInt(w, r, "limit", &limit) {
		return
	}
	if limit <= 0 {
		limit = 50
	}
	if limit > 200 {
		writeErr(w, http.StatusBadRequest, "limit must be at most 200")
		return
	}
	before := int64(0)
	if value := r.URL.Query().Get("before_id"); value != "" {
		parsed, err := strconv.ParseInt(value, 10, 64)
		if err != nil || parsed <= 0 {
			writeErr(w, http.StatusBadRequest, "before_id must be a positive integer")
			return
		}
		before = parsed
	}
	archives, more, err := s.Store.ListConversationArchives(r.Context(), r.PathValue("name"), limit, before)
	var nextBefore *string
	if more && len(archives) > 0 {
		value := strconv.FormatInt(archives[len(archives)-1].ID, 10)
		nextBefore = &value
	}
	respond(w, map[string]any{"archives": orEmpty(archives), "has_more": more, "next_before_id": nextBefore}, err)
}

func (s *Server) getConversationArchive(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeErr(w, http.StatusBadRequest, "archive id must be a positive integer")
		return
	}
	archive, err := s.Store.GetConversationArchive(r.Context(), r.PathValue("name"), id)
	respond(w, map[string]any{"archive": archive}, err)
}
