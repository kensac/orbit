package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/webhook"
)

// hook serves POST /hooks/{agent}/{trigger}: a signed webhook delivery starts a run of the agent's webhook trigger. The signature is the credential (no authentik token, no forward-auth: senders cannot sign in), checked against the trigger's secret before anything is stored. A delivery id already recorded answers the first run and starts nothing (webhook_deliveries_once). The body is kept as received; the run's webhook sources read it.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	name, trig := r.PathValue("agent"), r.PathValue("trigger")
	cfg := s.Engine.Current().Config
	a, ok := cfg.Agents[name]
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	t, index, ok := a.WebhookTrigger(trig)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	outcome := func(o string) { metrics.WebhookDelivery(a.Name, trig, o) }
	ws := cfg.Deployment.WebhookSecrets[t.Secret]
	secret, ok := s.Engine.Env(t.Secret)
	if !ok || secret == "" {
		// Validation cannot see the environment; an unset secret verifies nothing.
		outcome("unconfigured")
		writeErr(w, http.StatusServiceUnavailable, fmt.Sprintf("webhook secret %s is not set", t.Secret))
		return
	}
	limit := t.WebhookMaxBytes()
	if r.ContentLength > limit {
		outcome("too_large")
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", limit))
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		outcome("too_large")
		writeErr(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("the body is larger than %d bytes", limit))
		return
	}
	if err != nil {
		writeErr(w, http.StatusBadRequest, "reading the body: "+err.Error())
		return
	}
	d, err := webhook.Verify(ws.Scheme, []byte(secret), r.Header, body, time.Now(), spec.WebhookTolerance)
	switch {
	case errors.Is(err, webhook.ErrSignature):
		outcome("bad_signature")
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	case errors.Is(err, webhook.ErrStale):
		outcome("stale")
		writeErr(w, http.StatusUnauthorized, err.Error())
		return
	case errors.Is(err, webhook.ErrMalformed):
		outcome("malformed")
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	case err != nil:
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ws.Scheme == "github" && d.Event == "ping" {
		// GitHub sends one when a hook is created or tested: it proves the secret, and asks nothing of the agent.
		outcome("ping")
		respond(w, map[string]any{"ping": true}, nil)
		return
	}
	if !a.IsEnabled() {
		outcome("disabled")
		writeErr(w, http.StatusConflict, "the agent is disabled")
		return
	}
	media := "text/plain"
	if json.Valid(body) {
		media = "application/json"
	}
	prompt := t.Prompt
	if prompt != "" {
		prompt += "\n\n"
	}
	prompt += "Webhook " + trig + ": delivery " + d.ID
	if d.Event != "" {
		prompt += ", event " + d.Event
	}
	prompt += "."
	id, dup, err := s.Store.RecordWebhook(r.Context(), store.NewWebhook{
		Agent: a.Name, SpecSHA: a.SHA, TriggerName: trig, TriggerIndex: index,
		DeliveryID: d.ID, Event: d.Event, SentAt: d.SentAt, MediaType: media, Body: body, Prompt: prompt,
	})
	if err != nil {
		respond(w, nil, err)
		return
	}
	if dup {
		outcome("duplicate")
		respond(w, map[string]any{"run_id": id, "duplicate": true}, nil)
		return
	}
	outcome("accepted")
	s.Wake()
	respondStatus(w, http.StatusAccepted, map[string]any{"run_id": id, "duplicate": false}, nil)
}
