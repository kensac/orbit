package spec

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// DeploymentJSON is the deployment as control records it for workers (after defaults), and its hash. It names secrets only by variable, so it is safe in the database.
func (c *Config) DeploymentJSON() (sha string, doc []byte, err error) {
	doc, err = json.Marshal(c.Deployment)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(doc)
	return hex.EncodeToString(sum[:]), doc, nil
}

// StoredAgent is one agent as the database holds it: its current spec's hash and canonical JSON.
type StoredAgent struct {
	Name string
	SHA  string
	JSON []byte
}

// FromStored rebuilds a configuration from what control recorded, so a worker runs exactly the specs control loaded without reading any file. Each agent keeps its recorded SHA (runs and sessions compare it).
func FromStored(deployment []byte, agents []StoredAgent) (*Config, error) {
	cfg := &Config{Agents: map[string]*Agent{}}
	if err := json.Unmarshal(deployment, &cfg.Deployment); err != nil {
		return nil, fmt.Errorf("stored deployment: %w", err)
	}
	for _, sa := range agents {
		a := &Agent{}
		if err := json.Unmarshal(sa.JSON, a); err != nil {
			return nil, fmt.Errorf("stored agent %q: %w", sa.Name, err)
		}
		a.SHA = sa.SHA
		cfg.Agents[sa.Name] = a
	}
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Capabilities is what a host advertises (ORBIT_CAPABILITIES). Any is the wildcard "*": every agent, whatever it needs.
type Capabilities struct {
	Any  bool
	List []string
}

// ParseCapabilities reads a comma- or space-separated list; "*" alone is Any. An empty string is no capabilities: only agents that need nothing.
func ParseCapabilities(s string) (Capabilities, error) {
	fields := strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ' ' || r == '\n' || r == '\t' })
	if len(fields) == 1 && fields[0] == "*" {
		return Capabilities{Any: true}, nil
	}
	var c Capabilities
	for _, f := range fields {
		if !CapabilityRE.MatchString(f) {
			return Capabilities{}, fmt.Errorf("capability %q: want name or name:qualifier, lower case (or * alone)", f)
		}
		if !slices.Contains(c.List, f) {
			c.List = append(c.List, f)
		}
	}
	slices.Sort(c.List)
	return c, nil
}

// Covers reports whether a host with c may run agent a.
func (c Capabilities) Covers(a *Agent) bool {
	if c.Any {
		return true
	}
	for _, n := range a.Needs {
		if !slices.Contains(c.List, n) {
			return false
		}
	}
	return true
}

// String is the list as a host advertises it.
func (c Capabilities) String() string {
	if c.Any {
		return "*"
	}
	return strings.Join(c.List, ",")
}
