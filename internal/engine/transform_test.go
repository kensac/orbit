package engine_test

import (
	"strings"
	"testing"
)

// TestTransformsReplaceTheirSource: a transform's output is an input of its
// own, and the source it reads is withheld from the model but kept as the
// record of what was fetched.
func TestTransformsReplaceTheirSource(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, `
transforms:
  - name: second
    source: tx
    kind: json-filter
    params: {where: [{field: page, op: eq, value: 2}]}
`)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	if run, _ := s.GetRun(ctx, c.ID); run.Status != "succeeded" {
		t.Fatalf("run = %s %v", run.Status, run.Error)
	}
	if len(fr.got.Inputs) != 1 || fr.got.Inputs[0].Name != "second" || string(fr.got.Inputs[0].Content) != `[{"page":2}]` {
		t.Fatalf("inputs = %+v", fr.got.Inputs)
	}
	if strings.Contains(fr.got.Prompt, "inputs/tx.json") || !strings.Contains(fr.got.Prompt, "inputs/second.json") {
		t.Fatalf("prompt lists the wrong inputs:\n%s", fr.got.Prompt)
	}
	arts, _ := s.ListArtifacts(ctx, c.ID)
	names := map[string]bool{}
	for _, a := range arts {
		names[a.Name] = true
	}
	if !names["sources/tx.json"] || !names["inputs/second.json"] || names["inputs/tx.json"] {
		t.Fatalf("artifacts = %v", names)
	}
}

func TestAFailedTransformFailsTheRunNamingIt(t *testing.T) {
	fr := &fakeRunner{output: goodOutput}
	e, s, _ := harnessWith(t, fr, `
transforms:
  - name: ids
    source: tx
    kind: json-project
    keep_source: true
    params: {items: nope, fields: [page]}
`)
	c := claim(t, e, s)
	if err := e.Execute(ctx, c); err != nil {
		t.Fatal(err)
	}
	run, _ := s.GetRun(ctx, c.ID)
	if run.Status != "failed" || !strings.Contains(*run.Error, "transform ids") || fr.calls != 0 {
		t.Fatalf("run = %s %v, backend calls %d", run.Status, run.Error, fr.calls)
	}
}
