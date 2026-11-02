package spec

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

func TestAStoredConfigIsTheLoadedOne(t *testing.T) {
	cfg, err := Load("../../examples")
	if err != nil {
		t.Fatal(err)
	}
	_, dep, err := cfg.DeploymentJSON()
	if err != nil {
		t.Fatal(err)
	}
	var agents []StoredAgent
	for n, a := range cfg.Agents {
		b, _ := json.Marshal(a)
		agents = append(agents, StoredAgent{Name: n, SHA: a.SHA, JSON: b})
	}
	got, err := FromStored(dep, agents)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, cfg) {
		t.Fatalf("a worker's configuration differs from control's:\n%+v\n%+v", got.Deployment, cfg.Deployment)
	}
	if n := got.Agents["finance"].Needs; len(n) != 2 {
		t.Fatalf("needs = %v", n)
	}
}

func TestCapabilities(t *testing.T) {
	c, err := ParseCapabilities("sure, docker-network:hetzner,sure")
	if err != nil || c.Any || c.String() != "docker-network:hetzner,sure" {
		t.Fatalf("%+v %v", c, err)
	}
	finance := &Agent{Needs: []string{"sure", "docker-network:hetzner"}}
	if !c.Covers(finance) || !c.Covers(&Agent{}) || c.Covers(&Agent{Needs: []string{"browser"}}) {
		t.Fatal("covers")
	}
	none, _ := ParseCapabilities("")
	if none.Covers(finance) || !none.Covers(&Agent{}) {
		t.Fatal("no capabilities run only agents that need nothing")
	}
	if all, _ := ParseCapabilities("*"); !all.Any || !all.Covers(finance) {
		t.Fatal("* covers everything")
	}
	for _, bad := range []string{"Sure", "a b:", "*,sure", "x:y:z"} {
		if _, err := ParseCapabilities(bad); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

func TestNeedsAreValidated(t *testing.T) {
	dir := write(t, map[string]string{"orbit.yaml": base, "agents/a.yaml": `
name: a
charter: x
needs: [sure, sure, "Bad Cap"]
`})
	_, err := Load(dir)
	if err == nil || !strings.Contains(err.Error(), `"sure" is listed twice`) || !strings.Contains(err.Error(), `"Bad Cap" is not a capability`) {
		t.Fatalf("err = %v", err)
	}
}
