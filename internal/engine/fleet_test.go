package engine_test

import (
	"os"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// remote is an orbit worker on another host: the same database, a data
// volume of its own, its configuration read from the database as control
// recorded it, and no channel credential or approval secret.
func remote(t *testing.T, control *engine.Engine, name string) *engine.Engine {
	t.Helper()
	s := control.Store
	sha, doc, err := control.Current().Config.DeploymentJSON()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SyncDeployment(ctx, sha, doc); err != nil {
		t.Fatal(err)
	}
	dep, rows, err := s.StoredConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var agents []spec.StoredAgent
	for _, r := range rows {
		agents = append(agents, spec.StoredAgent{Name: r.Name, SHA: r.SHA, JSON: r.JSON})
	}
	cfg, err := spec.FromStored(dep, agents)
	if err != nil {
		t.Fatal(err)
	}
	for n, a := range cfg.Agents {
		if want := control.Current().Config.Agents[n].SHA; a.SHA != want {
			t.Fatalf("stored %s sha %s, want %s", n, a.SHA, want)
		}
	}
	w := &engine.Engine{Store: s, DataDir: t.TempDir(), Env: control.Env, Client: control.Client,
		NewRunner: control.NewRunner, Log: control.Log, HeartbeatEvery: control.HeartbeatEvery,
		Outbox: true, Worker: name, SessionStore: true}
	if err := w.SetConfig(cfg); err != nil {
		t.Fatal(err)
	}
	return w
}

func claimOn(t *testing.T, s *store.Store, c store.Claim) *store.ClaimedRun {
	t.Helper()
	got, err := s.ClaimRunFor(ctx, c)
	if err != nil || got == nil {
		t.Fatalf("claim: %v %v", got, err)
	}
	return got
}

func enqueue(t *testing.T, e *engine.Engine) {
	t.Helper()
	a := e.Current().Config.Agents["finance"]
	if _, err := e.Store.EnqueueRun(ctx, store.NewRun{Agent: "finance", SpecSHA: a.SHA, Trigger: "manual", Prompt: "check"}); err != nil {
		t.Fatal(err)
	}
}

func TestAWorkerLeavesDeliveryAndActionsToControl(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	control, s, h := harness(t, fr)
	w := remote(t, control, "oci")
	enqueue(t, w)
	c := claimOn(t, s, store.Claim{Worker: "oci/0"})
	if err := w.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "succeeded" {
		t.Fatalf("run %s: %v", run.Status, run.Error)
	}
	if len(h.messages) != 0 || len(h.calls) != 0 {
		t.Fatalf("a worker sent %d notifications and called %d endpoints", len(h.messages), len(h.calls))
	}
	acts, _ := s.ListActions(ctx, "finance", "", 0)
	for _, a := range acts {
		if a.Status == "executed" {
			t.Fatalf("a worker executed %s", a.Type)
		}
	}

	if err := control.DeliverOutbox(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.messages) != 2 {
		t.Fatalf("control sent %d notifications, want the report and one approval", len(h.messages))
	}
	report, approval := h.messages[0], h.messages[1]
	if !strings.Contains(report["title"].(string), "October spend") || report["link"] != "http://orbit.test/ui/runs/"+c.ID {
		t.Fatalf("report = %v", report)
	}
	if link, _ := approval["link"].(string); !strings.Contains(link, "/ui/actions/") || !strings.Contains(link, "sig=") {
		t.Fatalf("approval link %q is not control's signed one", link)
	}
	ds, _ := s.ListDeliveries(ctx, c.ID)
	if len(ds) != 2 || ds[0].Status != "sent" {
		t.Fatalf("deliveries = %+v", ds)
	}
	// Sent once.
	if err := control.DeliverOutbox(ctx); err != nil || len(h.messages) != 2 {
		t.Fatalf("resent: %d %v", len(h.messages), err)
	}
	// The auto action is control's to execute.
	if err := control.ExecuteActions(ctx); err != nil {
		t.Fatal(err)
	}
	if len(h.messages) != 3 {
		t.Fatalf("the auto notify action was not executed by control: %d", len(h.messages))
	}
}

func TestASessionContinuesOnAnotherHost(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	control, s, _ := harnessWith(t, fr, sessionSpec)
	control.SessionStore = true
	oci := remote(t, control, "oci")
	transmission := remote(t, control, "transmission")

	enqueue(t, oci)
	first := claimOn(t, s, store.Claim{Worker: "oci/0"})
	if err := oci.Execute(ctx, first); err != nil {
		t.Fatal(err)
	}

	// The next run is claimed on a host that never saw the session.
	enqueue(t, transmission)
	second := claimOn(t, s, store.Claim{Worker: "transmission/0"})
	if err := transmission.Execute(ctx, second); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session == nil || fr.got.Session.Resume != "session-1" {
		t.Fatalf("second run resumed %+v, want session-1", fr.got.Session)
	}
	if !strings.HasPrefix(fr.got.Workdir, transmission.DataDir) {
		t.Fatalf("ran in %s, not on transmission's volume", fr.got.Workdir)
	}
	path, _ := runner.SessionFile(fr.got.Session.Home, fr.got.Workdir, "session-1")
	if b, err := os.ReadFile(path); err != nil || string(b) != "{}\n" {
		t.Fatalf("the transcript was not written back: %q %v", b, err)
	}
	run, _ := s.GetRun(ctx, second.ID)
	if *run.SessionMode != "resumed" || *run.SessionParent != "session-1" || *run.SessionDepth != 2 {
		t.Fatalf("run session = %v %v %v", *run.SessionMode, *run.SessionParent, *run.SessionDepth)
	}

	// And back on control's own volume, from transmission's run.
	enqueue(t, control)
	third := claimOn(t, s, store.Claim{Worker: "hetzner/0", Any: true})
	if err := control.Execute(ctx, third); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "session-2" {
		t.Fatalf("third run resumed %q, want session-2", fr.got.Session.Resume)
	}
}

func TestAnInterruptedRunContinuesOnAnotherHost(t *testing.T) {
	fr := &fakeRunner{output: goodOutput, savedCost: 0.1}
	control, s, _ := harness(t, fr)
	oci := remote(t, control, "oci")
	transmission := remote(t, control, "transmission")
	enqueue(t, oci)
	c := claimOn(t, s, store.Claim{Worker: "oci/0"})
	interrupt(t, oci, fr, c)
	first := fr.got

	c2 := claimOn(t, s, store.Claim{Worker: "transmission/0"})
	if c2.ID != c.ID || c2.Attempt != 2 {
		t.Fatalf("claim = %+v", c2)
	}
	if err := transmission.Execute(ctx, c2); err != nil {
		t.Fatal(err)
	}
	got := fr.got
	if got.Session == nil || got.Session.Resume != "session-1" || got.Session.BaseCostUSD != 0.1 {
		t.Fatalf("attempt 2 resumed %+v, want session-1 at its saved total", got.Session)
	}
	if len(got.Inputs) != 1 || got.Inputs[0].Path != first.Inputs[0].Path {
		t.Fatalf("inputs = %+v, want the interrupted attempt's", got.Inputs)
	}
	at := attempts(t, s, c.ID)
	if len(at) != 2 || at[1].RecoveredFrom == nil || *at[1].RecoveredFrom != 1 {
		t.Fatalf("attempts %+v", at)
	}
}

func TestWithoutTheSessionStoreAnotherHostStartsFresh(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	control, s, _ := harnessWith(t, fr, sessionSpec)
	oci := remote(t, control, "oci")
	oci.SessionStore = false
	other := remote(t, control, "transmission")
	other.SessionStore = false
	enqueue(t, oci)
	if err := oci.Execute(ctx, claimOn(t, s, store.Claim{Worker: "oci/0"})); err != nil {
		t.Fatal(err)
	}
	enqueue(t, other)
	c := claimOn(t, s, store.Claim{Worker: "transmission/0"})
	if err := other.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if fr.got.Session.Resume != "" {
		t.Fatalf("resumed %q with no transcript on this host", fr.got.Session.Resume)
	}
}
