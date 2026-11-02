package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/kensac/orbit/internal/push"
	"github.com/kensac/orbit/internal/store"
)

// attentionItem is an open attention row with the app link it opens.
type attentionItem struct {
	store.Attention
	Link string `json:"link"`
}

func (s *Server) linkBase() string {
	if s.Push != nil {
		return s.Push.LinkBase
	}
	return "/orbit"
}

// listAttention serves GET /attention: everything waiting on a person,
// across agents, newest first.
func (s *Server) listAttention(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListAttention(r.Context(), intParam(r, "limit"))
	items := make([]attentionItem, 0, len(list))
	for _, a := range list {
		items = append(items, attentionItem{Attention: a, Link: push.Link(s.linkBase(), a)})
	}
	respond(w, map[string]any{"items": items}, err)
}

func (s *Server) dismissAttention(w http.ResponseWriter, r *http.Request) {
	err := s.Store.DismissAttention(r.Context(), r.PathValue("id"), actorOrOperator(r))
	respond(w, map[string]any{"ok": err == nil}, err)
}

func actorOrOperator(r *http.Request) string {
	if a := actor(r, ""); a != "" {
		return a
	}
	return "operator"
}

// pushInfo serves GET /push: whether push is configured and the key a
// browser subscribes with.
func (s *Server) pushInfo(w http.ResponseWriter, _ *http.Request) {
	if s.Push == nil {
		respond(w, map[string]any{"enabled": false, "public_key": nil}, nil)
		return
	}
	respond(w, map[string]any{"enabled": true, "public_key": s.Push.Keys.Public}, nil)
}

// listPushSubscriptions serves GET /push/subscriptions: the caller's own
// devices (every device for the operator token).
func (s *Server) listPushSubscriptions(w http.ResponseWriter, r *http.Request) {
	subs, err := s.Store.ListPushSubscriptions(r.Context(), actor(r, ""))
	respond(w, map[string]any{"subscriptions": orEmpty(subs)}, err)
}

type subscriptionBody struct {
	Endpoint string `json:"endpoint"`
	Keys     struct {
		P256DH string `json:"p256dh"`
		Auth   string `json:"auth"`
	} `json:"keys"`
	Label string `json:"label"`
}

// savePushSubscription serves POST /push/subscriptions with a browser's
// PushSubscription.toJSON() and an optional label.
func (s *Server) savePushSubscription(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		writeErr(w, http.StatusServiceUnavailable, "push is not configured (ORBIT_VAPID_*)")
		return
	}
	var b subscriptionBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil {
		writeErr(w, http.StatusBadRequest, "body: "+err.Error())
		return
	}
	if err := push.AllowedEndpoint(b.Endpoint); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	label := strings.TrimSpace(b.Label)
	if len(label) > 100 {
		label = label[:100]
	}
	id, err := s.Store.SavePushSubscription(r.Context(), store.PushSubscription{
		Endpoint: b.Endpoint, P256DH: b.Keys.P256DH, Auth: b.Keys.Auth, Author: actorOrOperator(r), Label: label})
	respondStatus(w, http.StatusCreated, map[string]any{"id": id}, err)
}

// deletePushSubscription serves DELETE /push/subscriptions with
// {"endpoint": ...}: the device stops receiving pushes.
func (s *Server) deletePushSubscription(w http.ResponseWriter, r *http.Request) {
	var b subscriptionBody
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 8<<10)).Decode(&b); err != nil || b.Endpoint == "" {
		writeErr(w, http.StatusBadRequest, "body: want {\"endpoint\": ...}")
		return
	}
	err := s.Store.RevokePushSubscription(r.Context(), b.Endpoint)
	respond(w, map[string]any{"ok": err == nil}, err)
}

// testPush serves POST /push/test: one push to the caller's devices now.
func (s *Server) testPush(w http.ResponseWriter, r *http.Request) {
	if s.Push == nil {
		writeErr(w, http.StatusServiceUnavailable, "push is not configured (ORBIT_VAPID_*)")
		return
	}
	n, err := s.Push.Test(r.Context(), actor(r, ""))
	respond(w, map[string]any{"sent": n}, err)
}
