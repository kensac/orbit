package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/store"
)

func (s *Server) accountClaudeConfig(w http.ResponseWriter, r *http.Request) (store.Box, store.BoxAccount, *sandbox.Client, bool) {
	b, ok := s.workspaceBox(w, r)
	if !ok {
		return b, store.BoxAccount{}, nil, false
	}
	a, ok := s.workspaceAccount(w, r, b, r.PathValue("aid"))
	if !ok {
		return b, a, nil, false
	}
	c, ok := s.boxClient(w)
	return b, a, c, ok
}

func (s *Server) getAccountClaudeConfig(w http.ResponseWriter, r *http.Request) {
	b, a, c, ok := s.accountClaudeConfig(w, r)
	if !ok {
		return
	}
	st, err := c.Box(r.Context(), b.Agent)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if st.State != sandbox.BoxRunning {
		writeCheck(w, http.StatusConflict, "box_stopped", "start a terminal for this account before reading its Claude config")
		return
	}
	if !slices.Contains(st.Accounts, a.Slot) {
		writeCheck(w, http.StatusConflict, "account_unmounted", "restart the box before reading this account's Claude config")
		return
	}
	if err := c.PrepareAccount(r.Context(), b.Agent, a.Slot); err != nil {
		respond(w, nil, err)
		return
	}
	config, err := c.ClaudeConfigStatus(r.Context(), b.Agent, a.Slot)
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"config": config}, nil)
}

func (s *Server) putAccountClaudeConfig(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	expected := r.Header.Get("If-Match")
	bundleDigest := r.Header.Get("X-Orbit-Bundle-SHA256")
	if !validConfigDigest(expected) || !validConfigDigest(bundleDigest) {
		writeErr(w, http.StatusBadRequest, "If-Match and X-Orbit-Bundle-SHA256 must be lowercase sha256 values")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sandbox.MaxClaudeBundleBytes)
	archive, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "Claude config bundle exceeds its size limit")
		return
	}
	b, a, c, ok := s.accountClaudeConfig(w, r)
	if !ok {
		return
	}
	if !s.readyBox(w, r, c, b, &a) {
		return
	}
	config, err := c.ApplyClaudeConfig(r.Context(), b.Agent, a.Slot, expected, bundleDigest, archive)
	if err != nil {
		var se *sandbox.Error
		if errors.As(err, &se) {
			if se.Status == http.StatusConflict {
				current, readErr := c.ClaudeConfigStatus(r.Context(), b.Agent, a.Slot)
				if readErr == nil {
					writeClaudeConfigConflict(w, current)
					return
				}
			}
			writeErr(w, se.Status, se.Message)
			return
		}
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"config": config}, nil)
}

func validConfigDigest(v string) bool {
	if len(v) != 64 {
		return false
	}
	for _, c := range v {
		if !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'f') {
			return false
		}
	}
	return true
}

func writeClaudeConfigConflict(w http.ResponseWriter, current sandbox.ClaudeConfigStatus) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusConflict)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": "Claude config changed since planning", "check": "changed", "config": current})
}
