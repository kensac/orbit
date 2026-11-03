package spec

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/kensac/orbit/internal/extensions"
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

func TestStdioConnectorRequiresAnExactLocalToolPackage(t *testing.T) {
	dir := stdioSpecFixture(t)
	pkg, err := extensions.ValidateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	orbit := base + "sandbox: {sandboxd: 'sandboxd:8080'}\nextensions:\n  enabled: true\n  packages:\n    daily-total: {path: '" + dir + "', sha256: " + pkg.Digest + "}\nconnectors:\n  local: {transport: stdio, package: daily-total, write_tools: [sum]}\n"
	agent := "name: local-agent\ncharter: x\nconnectors: [local]\nactions: {\"mcp:local:sum\": approve}\n"
	cfg, err := loadConnectors(t, orbit, map[string]string{"agents/local-agent.yaml": agent})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Deployment.Connectors["local"].AuthKind() != ConnectorAuthNone || cfg.ExtensionPackages["daily-total"].Digest != pkg.Digest {
		t.Fatal("stdio package or no-auth policy was not retained")
	}
	bad := strings.Replace(orbit, "transport: stdio, package: daily-total, write_tools: [sum]", "transport: stdio, package: daily-total, auth: {kind: oauth}", 1)
	if _, err := loadConnectors(t, bad, map[string]string{"agents/local-agent.yaml": agent}); err == nil || !strings.Contains(err.Error(), "takes no authentication configuration") {
		t.Fatalf("stdio OAuth error = %v", err)
	}
	badAgent := strings.Replace(agent, "mcp:local:sum", "mcp:local:missing", 1)
	if _, err := loadConnectors(t, orbit, map[string]string{"agents/local-agent.yaml": badAgent}); err == nil || !strings.Contains(err.Error(), "stdio package does not declare tool") {
		t.Fatalf("undeclared tool error = %v", err)
	}
}

func stdioSpecFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "extensions", "testdata", "reducer")
	manifest, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	op := m["operations"].([]any)[0].(map[string]any)
	op["kind"] = "tool"
	delete(op, "inputs")
	manifest, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	for name, body := range map[string][]byte{"manifest.json": manifest} {
		if err := os.WriteFile(filepath.Join(dir, name), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	entry, err := os.ReadFile(filepath.Join(src, "reducer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reducer"), entry, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestCalendarSkipRequiresListedReadOnlyConnectorTool(t *testing.T) {
	calendar := `calendar_skip:
  connector: hw
  tool: find
  window_after: 1h
  arguments: {from: "{{window_start}}", to: "{{window_end}}"}
`
	for name, tc := range map[string]struct{ listed, tool, want string }{
		"listed read tool": {"connectors: [hw]\n", "find", ""},
		"not listed":       {"", "find", `calendar_skip.connector "hw" must also appear in connectors`},
		"configured write": {"connectors: [hw]\n", "create_view", `calendar_skip.tool "create_view" must not be listed in connectors.hw.write_tools`},
	} {
		t.Run(name, func(t *testing.T) {
			body := "name: f\ncharter: x\n" + tc.listed + strings.Replace(calendar, "tool: find", "tool: "+tc.tool, 1)
			_, err := loadConnectors(t, connectorBase, map[string]string{"agents/f.yaml": body})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
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
		"stdio":    {connectorBase + "  local: {transport: stdio, url: 'https://x.example/'}\n", "", "stdio transport takes a package and no url"},
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
