package api

import (
	"net/http"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

func (s *Server) replayEvaluation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Cases []engine.EvaluationCase `json:"cases"`
	}
	if !decode(w, r, engine.MaxEvaluationBatchInput+(engine.MaxEvaluationCases*512)+1024, &body, bodyRequired) {
		return
	}
	if err := engine.ValidateEvaluationCases(body.Cases); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	snap := s.Engine.Current()
	if snap == nil || snap.Config == nil {
		writeErr(w, http.StatusServiceUnavailable, "configuration unavailable")
		return
	}
	a, ok := snap.Config.Agents[r.PathValue("name")]
	if !ok || !a.IsEnabled() {
		respond(w, nil, store.ErrNotFound)
		return
	}
	results, err := s.Engine.Replay(r.Context(), r.PathValue("name"), body.Cases)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"results": results}, nil)
}
