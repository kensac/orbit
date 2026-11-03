package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"

	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/store"
)

func (s *Server) putAccountClaudePlugin(w http.ResponseWriter, r *http.Request) {
	if !s.requireWorkspaceGroup(w, r) {
		return
	}
	pluginID := r.PathValue("plugin")
	if !sandbox.ValidClaudePluginID(pluginID) {
		writeErr(w, http.StatusBadRequest, "plugin ID must be name@marketplace")
		return
	}
	snap := s.Engine.Current()
	if snap == nil || snap.Config == nil || snap.Config.Deployment.Box == nil {
		writeCheck(w, http.StatusForbidden, "plugin_not_pinned", "Claude plugin import requires an operator digest pin")
		return
	}
	pinned := snap.Config.Deployment.Box.PluginDigests[pluginID]
	if !validConfigDigest(pinned) {
		writeCheck(w, http.StatusForbidden, "plugin_not_pinned", "Claude plugin is not pinned by the operator")
		return
	}
	digest := r.Header.Get("X-Orbit-Plugin-SHA256")
	archiveDigest := r.Header.Get("X-Orbit-Bundle-SHA256")
	if !validConfigDigest(digest) || !validConfigDigest(archiveDigest) {
		writeErr(w, http.StatusBadRequest, "plugin and bundle digests must be lowercase sha256 values")
		return
	}
	if digest != pinned {
		writeCheck(w, http.StatusForbidden, "plugin_digest_mismatch", "Claude plugin payload does not match the operator pin")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, sandbox.MaxClaudeBundleBytes)
	archive, err := io.ReadAll(r.Body)
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "Claude plugin archive exceeds its size limit")
		return
	}
	if len(archive) == 0 {
		writeErr(w, http.StatusBadRequest, "Claude plugin archive is empty")
		return
	}
	h := sha256.Sum256(archive)
	if hex.EncodeToString(h[:]) != archiveDigest {
		writeErr(w, http.StatusBadRequest, "Claude plugin archive digest does not match its bytes")
		return
	}
	b, a, c, ok := s.accountClaudeConfig(w, r)
	if !ok {
		return
	}
	s.accountConfigMu.Lock()
	defer s.accountConfigMu.Unlock()
	running, err := s.Store.ListTerminals(r.Context(), b.ID, store.TerminalRunning)
	if err != nil {
		respond(w, nil, err)
		return
	}
	for _, terminal := range running {
		if terminal.AccountID != nil && *terminal.AccountID == a.ID {
			writeCheck(w, http.StatusConflict, "account_busy", "end this account's terminals before importing a plugin")
			return
		}
	}
	if !s.readyBox(w, r, c, b, &a) {
		return
	}
	s.Log.Info("Claude plugin import requested", "workspace", b.ID, "account", a.ID, "plugin", pluginID, "digest", pinned, "by", actor(r, "operator"))
	result, err := c.InstallClaudePlugin(r.Context(), b.Agent, a.Slot, pluginID, digest, archiveDigest, archive)
	if err != nil {
		var se *sandbox.Error
		if errors.As(err, &se) {
			writeErr(w, se.Status, se.Message)
			return
		}
		respond(w, nil, err)
		return
	}
	if result.Plugin != pluginID || result.Digest != pinned || result.Files < 1 {
		writeErr(w, http.StatusBadGateway, "sandboxd returned an invalid plugin install result")
		return
	}
	s.Log.Info("Claude plugin imported into box account", "workspace", b.ID, "account", a.ID, "plugin", pluginID, "digest", pinned, "files", result.Files)
	respond(w, map[string]any{"plugin": result}, nil)
}

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
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
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
