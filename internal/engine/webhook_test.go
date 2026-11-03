package engine_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

const quietOutput = `{"summary": "triaged", "report": "ok", "notify": {"send": false, "priority": "default", "title": ""},
  "memory": {"update": false, "content": ""}, "questions": [], "actions": []}`

func webhookHarness(t *testing.T, fr *fakeRunner) (*engine.Engine, *store.Store) {
	t.Helper()
	e, s, _ := harnessFiles(t, fr, "", map[string]string{
		"orbit.yaml": "webhook_secrets:\n  HOOK: {scheme: github, agents: [hooked]}\n",
		"agents/hooked.yaml": `
name: hooked
charter: Triage issues.
triggers: [{webhook: issues, secret: HOOK}]
sources: [{name: event, kind: webhook}]
transforms:
  - name: opened
    source: event
    kind: json-project
    params: {items: ".", where: [{field: action, op: eq, value: opened}], fields: [issue.title]}
gates: [{kind: items_above, input: opened, n: 0}]
`,
	})
	return e, s
}

func deliver(t *testing.T, e *engine.Engine, s *store.Store, id, body string) (string, bool) {
	t.Helper()
	a := e.Current().Config.Agents["hooked"]
	run, dup, err := s.RecordWebhook(ctx, store.NewWebhook{Agent: "hooked", SpecSHA: a.SHA, TriggerName: "issues", TriggerIndex: 0,
		DeliveryID: id, Event: "issues", MediaType: "application/json", Body: []byte(body), Prompt: "Webhook issues: delivery " + id + "."})
	if err != nil {
		t.Fatal(err)
	}
	return run, dup
}

func execNext(t *testing.T, e *engine.Engine, s *store.Store) *store.Run {
	t.Helper()
	c, err := s.ClaimRun(ctx, "test")
	if err != nil || c == nil {
		t.Fatalf("claim: %v %v", c, err)
	}
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	r, err := s.GetRun(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestAWebhookDeliveryIsARunsSource: the delivery's body is the run's webhook source, transforms and gates apply to it as to a fetched one, and a retried delivery starts nothing.
func TestAWebhookDeliveryIsARunsSource(t *testing.T) {
	fr := &fakeRunner{output: quietOutput}
	e, s := webhookHarness(t, fr)

	body := `{"action":"opened","issue":{"title":"Login is broken"}}`
	first, dup := deliver(t, e, s, "d-1", body)
	if dup {
		t.Fatal("a new delivery is not a duplicate")
	}
	if again, dup := deliver(t, e, s, "d-1", body); !dup || again != first {
		t.Fatalf("a retried delivery = %s dup %v, want the first run %s", again, dup, first)
	}
	r := execNext(t, e, s)
	if r.ID != first || r.Status != store.StatusSucceeded || r.Trigger != store.TriggerWebhook || fr.calls != 1 {
		t.Fatalf("run %s = %s (%s), calls %d", r.ID, r.Status, r.Trigger, fr.calls)
	}
	var names []string
	for _, in := range fr.got.Inputs {
		names = append(names, in.Name)
		if in.Name == "opened" && string(in.Content) != `[{"issue.title":"Login is broken"}]` {
			t.Fatalf("opened = %s", in.Content)
		}
	}
	if !strings.Contains(strings.Join(names, ","), "opened") || strings.Contains(strings.Join(names, ","), "event") {
		t.Fatalf("inputs %v: the transform's output is shown and the source it reads is withheld", names)
	}
	arts, err := s.ListArtifacts(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	kept := false
	for _, a := range arts {
		kept = kept || a.Name == "sources/event.json"
	}
	if !kept {
		t.Fatalf("artifacts %+v: the delivery's body is kept as the withheld source", arts)
	}
	if !strings.Contains(fr.got.Prompt, "delivery d-1") {
		t.Fatalf("prompt %q does not say what started the run", fr.got.Prompt)
	}

	// Another action: the transform finds nothing, the gate skips without a model call.
	deliver(t, e, s, "d-2", `{"action":"labeled","issue":{"title":"Login is broken"}}`)
	r = execNext(t, e, s)
	if r.Status != store.StatusSkipped || r.Gate == nil || *r.Gate != "items_above" || fr.calls != 1 {
		t.Fatalf("labeled = %s gate %v, calls %d", r.Status, r.Gate, fr.calls)
	}

	// A manual run has no delivery: the source is absent, its transform skipped, and the run still reaches the model.
	a := e.Current().Config.Agents["hooked"]
	if _, err := s.EnqueueRun(ctx, store.NewRun{Agent: "hooked", SpecSHA: a.SHA, Trigger: "manual"}); err != nil {
		t.Fatal(err)
	}
	r = execNext(t, e, s)
	if r.Status != store.StatusSucceeded || fr.calls != 2 {
		t.Fatalf("manual = %s, calls %d", r.Status, fr.calls)
	}
	for _, in := range fr.got.Inputs {
		if in.Name == "opened" || in.Name == "event" {
			t.Fatalf("a manual run was given %s", in.Name)
		}
	}
	evs, err := s.ListEvents(ctx, r.ID, 0, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	absent := false
	for _, ev := range evs {
		if ev.Kind == "orbit.source_absent" {
			var p map[string]any
			_ = json.Unmarshal(ev.Payload, &p)
			absent = p["source"] == "event"
		}
	}
	if !absent {
		t.Fatal("no orbit.source_absent event for the webhook source of a manual run")
	}
}
