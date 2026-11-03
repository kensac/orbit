package store_test

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"testing"

	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/testdb"
)

func TestTemplateTaskPinsWorkerSpecAndHidesItsAgent(t *testing.T) {
	s := testdb.New(t)
	seed(t, s, "parent")
	parentRun := manual(t, s, "parent")
	claimed, err := s.ClaimRun(ctx, "worker")
	if err != nil || claimed == nil || claimed.ID != parentRun {
		t.Fatalf("claim = %+v, %v", claimed, err)
	}
	actions, err := s.RecordOutcome(ctx, parentRun, "parent", store.RunResult{Attempt: 1, Status: "succeeded"}, store.Outcome{
		Actions: []store.ProposedAction{{Type: "delegate", Params: json.RawMessage(`{}`), Rule: "auto"}},
	})
	if err != nil || len(actions) != 1 {
		t.Fatalf("record parent action = %v, %v", actions, err)
	}
	spec := []byte(`{"name":"worker-01","connectors":["drive"]}`)
	sum := sha256.Sum256(spec)
	head := hex.EncodeToString(sum[:])
	delegated, err := s.Delegate(ctx, store.Delegation{ActionID: actions[0], RunID: parentRun, Tasks: []store.NewTask{{
		Key: "report", Agent: "worker-01", TemplateName: "weekly-report", TemplateVersion: 3,
		WorkerSpec: spec, WorkerSHA: head, Fresh: true, Brief: "Prepare a report",
	}}})
	if err != nil || len(delegated) != 1 || delegated[0].Agent != "template:weekly-report" {
		t.Fatalf("delegate = %+v, %v", delegated, err)
	}
	task, err := s.GetTask(ctx, delegated[0].ID)
	if err != nil || task.Template == nil || task.Template.Name != "weekly-report" || task.Template.Version != 3 || task.Agent != "template:weekly-report" {
		t.Fatalf("task provenance = %+v, %v", task, err)
	}
	pinned, err := s.TaskWorkerSpec(ctx, "worker-01", head)
	if err != nil {
		t.Fatalf("pinned worker spec: %v", err)
	}
	var pinnedSpec struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(pinned, &pinnedSpec); err != nil || pinnedSpec.Name != "worker-01" {
		t.Fatalf("pinned worker spec = %s, %v", pinned, err)
	}
	agents, err := s.ListAgents(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, agent := range agents {
		if agent.Name == "worker-01" {
			t.Fatal("template task worker appeared as a regular agent")
		}
	}
}
