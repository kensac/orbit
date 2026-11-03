package api

import (
	"net/http"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

func (s *Server) gradeEvaluation(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Target string             `json:"target_agent"`
		Cases  []engine.GradeCase `json:"cases"`
	}
	limit := engine.MaxEvaluationGradeCases*(6*engine.MaxEvaluationGradeRubric+engine.MaxEvaluationGradeOutput+6*256+1024) + 1024
	if !decode(w, r, int64(limit), &body, bodyRequired) {
		return
	}
	if err := engine.ValidateGradeCases(body.Cases); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if body.Target == r.PathValue("name") {
		writeErr(w, http.StatusBadRequest, "grader must be separate from the replay target")
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
	results, err := s.Engine.Grade(r.Context(), r.PathValue("name"), body.Target, body.Cases)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"grades": results}, nil)
}
