package spec

import (
	"encoding/json"
	"strings"
	"testing"
)

const connectorBase = trustBase + `
  HW_TOKEN: {url_prefix: "https://housing.example.com/"}
connectors:
  hw:
    url: https://housing.example.com/mcp
    description: Listings.
    auth: {kind: header, header: Authorization, value: "Bearer ${HW_TOKEN}"}
    write_tools: [create_view]
  gws: {url: "https://gws.example.com/mcp", auth: {kind: oauth}}
`

func loadConnectors(t *testing.T, deployment string, files map[string]string) (*Config, error) {
	t.Helper()
	all := map[string]string{"orbit.yaml": deployment}
	for k, v := range files {
		all[k] = v
	}
	return LoadAll(write(t, all), nil)
}

func TestConnectorsLoad(t *testing.T) {
	cfg, err := loadConnectors(t, connectorBase, map[string]string{"agents/f.yaml": `
name: f
charter: x
connectors: [hw, gws]
actions:
  "mcp:hw:find": approve
  "mcp:gws:delete": deny
`})
	if err != nil {
		t.Fatal(err)
	}
	a, hw := cfg.Agents["f"], cfg.Deployment.Connectors["hw"]
	for tool, want := range map[string]Rule{"find": RuleApprove, "create_view": RuleApprove, "other": RuleAuto} {
		if got := a.ToolRule(hw, "hw", tool); got != want {
			t.Errorf("rule for %s = %s, want %s", tool, got, want)
		}
	}
	if got := a.ToolRule(cfg.Deployment.Connectors["gws"], "gws", "delete"); got != RuleDeny {
		t.Errorf("an agent's deny = %s", got)
	}
}

func TestCatalogConnectorsLoadWithApprovalByDefault(t *testing.T) {
	dir := write(t, map[string]string{
		"orbit.yaml": connectorBase,
		"agents/f.yaml": `name: f
charter: x
connectors: [public-api]
`,
	})
	cfg, err := LoadAllWithConnectors(dir, nil, map[string]Connector{
		"public-api": {URL: "https://service.example/mcp", Description: "Public API", Auth: ConnectorAuth{Kind: ConnectorAuthOAuth}},
	})
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.Deployment.Connectors["public-api"]
	if !c.CatalogManaged || c.AuthKind() != ConnectorAuthOAuth {
		t.Fatalf("catalog connector = %+v", c)
	}
	a := cfg.Agents["f"]
	if got := a.ToolRule(c, "public-api", "read"); got != RuleApprove {
		t.Fatalf("unspecified catalog tool rule = %s, want approve", got)
	}
	a.Actions = ActionSet{}
	a.Actions["mcp:public-api:read"] = RuleDeny
	if got := a.ToolRule(c, "public-api", "read"); got != RuleDeny {
		t.Fatalf("explicit rule = %s, want deny", got)
	}
}

func TestCatalogConnectorCannotReplaceConfiguredConnector(t *testing.T) {
	dir := write(t, map[string]string{"orbit.yaml": connectorBase, "agents/f.yaml": "name: f\ncharter: x\n"})
	_, err := LoadAllWithConnectors(dir, nil, map[string]Connector{"hw": {URL: "https://other.example/mcp"}})
	if err == nil || !strings.Contains(err.Error(), `connector "hw" is defined in orbit.yaml and the catalog`) {
		t.Fatalf("collision error = %v", err)
	}
}

func TestStoredCatalogConnectorRetainsItsSafetyPolicy(t *testing.T) {
	dir := write(t, map[string]string{"orbit.yaml": connectorBase, "agents/f.yaml": "name: f\ncharter: x\nconnectors: [public-api]\n"})
	cfg, err := LoadAllWithConnectors(dir, nil, map[string]Connector{
		"public-api": {URL: "https://service.example/mcp", Auth: ConnectorAuth{Kind: ConnectorAuthOAuth}},
	})
	if err != nil {
		t.Fatal(err)
	}
	_, dep, err := cfg.DeploymentJSON()
	if err != nil {
		t.Fatal(err)
	}
	agent, err := json.Marshal(cfg.Agents["f"])
	if err != nil {
		t.Fatal(err)
	}
	got, err := FromStored(dep, []StoredAgent{{Name: "f", SHA: cfg.Agents["f"].SHA, JSON: agent}})
	if err != nil {
		t.Fatal(err)
	}
	c := got.Deployment.Connectors["public-api"]
	if !c.CatalogManaged || c.DefaultToolRule != RuleApprove {
		t.Fatalf("stored connector lost its safety policy: %+v", c)
	}
}

func TestConnectorMistakesFailTheLoad(t *testing.T) {
	cases := map[string]struct{ deployment, agent, want string }{
		"unknown":      {connectorBase, "connectors: [nope]", `connectors: "nope" is not a connector in orbit.yaml`},
		"rule":         {connectorBase, "connectors: [hw]\nactions: {\"mcp:gws:x\": auto}", `the agent does not list connector "gws"`},
		"action name":  {connectorBase, "connectors: [hw]\nactions: {\"connector:hw/x\": auto}", "a connector tool's rule is named mcp:<connector>:<tool>"},
		"bad rule key": {connectorBase, "connectors: [hw]\nactions: {\"mcp:hw\": auto}", "want mcp:<connector>:<tool>"},
		"not a credential": {strings.Replace(connectorBase, "${HW_TOKEN}", "${ORBIT_API_TOKEN}", 1), "",
			"${ORBIT_API_TOKEN} is not in orbit.yaml's credentials"},
		"credential elsewhere": {strings.Replace(connectorBase, "https://housing.example.com/mcp", "https://evil.example/mcp", 1), "",
			"credential HW_TOKEN may be sent only to https://housing.example.com/"},
		"stdio":    {connectorBase + "  local: {transport: stdio, url: 'https://x.example/'}\n", "", "transport stdio is not supported yet"},
		"reserved": {connectorBase + "  orbit: {url: 'https://x.example/mcp'}\n", "", "orbit's own MCP server"},
		"codex": {strings.Replace(connectorBase, "runners: {r: {kind: claude-code}}", "runners: {r: {kind: claude-code}, cx: {kind: codex}}", 1), "runner: cx\nconnectors: [hw]",
			"connectors: need a claude-code runner"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := loadConnectors(t, tc.deployment, map[string]string{"agents/f.yaml": "name: f\ncharter: x\n" + tc.agent + "\n"})
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestAddingAConnectorWidensAnAgent(t *testing.T) {
	cur := &Agent{Connectors: []string{"hw"}}
	next := &Agent{Connectors: []string{"hw", "gws"}, Actions: ActionSet{"mcp:hw:create_view": RuleAuto}}
	got := strings.Join(Widening(cur, next), "; ")
	if !strings.Contains(got, "connector gws") || !strings.Contains(got, "action mcp:hw:create_view: auto") {
		t.Fatalf("widening = %q", got)
	}
	if w := Widening(next, cur); len(w) != 0 {
		t.Fatalf("removing a connector widened: %v", w)
	}
}
