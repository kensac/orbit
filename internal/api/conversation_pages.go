package api

import (
	"net/http"

	"github.com/kensac/orbit/internal/store"
)

func (s *Server) publishedConversationPage(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	conversationID, pageID := r.PathValue("id"), r.PathValue("page")
	if !store.IsUUID(conversationID) || !store.IsUUID(pageID) {
		writeErr(w, http.StatusBadRequest, "not a valid id")
		return
	}
	p, err := s.Store.ConversationPage(r.Context(), conversationID, pageID)
	respond(w, map[string]any{"page": p}, err)
}
