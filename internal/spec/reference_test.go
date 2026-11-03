package spec

import (
	"os"
	"reflect"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// referenceDoc is the field reference a person reads instead of this package.
const referenceDoc = "../../docs/agents/spec-reference.md"

// referenceTables maps each heading of referenceDoc to the struct its first
// table documents.
var referenceTables = map[string]any{
	"orbit.yaml":                        Deployment{},
	"`defaults`":                        Defaults{},
	"`channels.<name>`":                 Channel{},
	"`endpoints.<name>`":                Endpoint{},
	"`endpoints.<name>.params.<name>`":  EndpointParam{},
	"`runners.<name>`":                  RunnerSpec{},
	"`github`":                          GitHub{},
	"`github.box`":                      GitHubBox{},
	"`build`":                           Build{},
	"`build.repositories.<owner/name>`": BuildRepo{},
	"`sandbox` (orbit.yaml)":            SandboxService{},
	"`credentials.<VAR>`":               Credential{},
	"`connectors.<name>`":               Connector{},
	"`connectors.<name>.auth`":          ConnectorAuth{},
	"`google_actions`":                  GoogleActions{},
	"`google_actions.gmail_draft`":      GoogleActionBinding{},
	"`google_actions.calendar_create`":  GoogleCalendarBinding{},
	"`webhook_secrets.<VAR>`":           WebhookSecret{},
	"`mail`":                            Mail{},
	"`mail.imap`":                       IMAP{},
	"`board`":                           Board{},
	"Agent":                             Agent{},
	"`budget`":                          AgentBudget{},
	"`triggers[]`":                      Trigger{},
	"`sources[]`":                       Source{},
	"`sources[].paginate`":              Paginate{},
	"`sources[].by_trigger.<trigger>`":  SourceOverride{},
	"`transforms[]`":                    Transform{},
	"`gates[]`":                         Gate{},
	"`notify`":                          NotifySpec{},
	"`repos[]`":                         RepoRef{},
	"`session`":                         SessionSpec{},
	"`web`":                             WebSpec{},
	"`permissions`":                     Permissions{},
	"`learn`":                           LearnSpec{},
	"`hooks[]`":                         Hook{},
}

var (
	headingRE  = regexp.MustCompile(`^#{2,3} (.+)$`)
	fieldRowRE = regexp.MustCompile("^\\| `([a-z_]+)` \\|")
)

// TestReferenceCoversSpec holds the reference to the structs: each mapped
// section's first table lists exactly the struct's yaml fields, so a field
// added or removed without its row fails here.
func TestReferenceCoversSpec(t *testing.T) {
	b, err := os.ReadFile(referenceDoc)
	if err != nil {
		t.Fatal(err)
	}
	rows := map[string][]string{}
	section, inTable, done := "", false, map[string]bool{}
	for line := range strings.SplitSeq(string(b), "\n") {
		if m := headingRE.FindStringSubmatch(line); m != nil {
			section, inTable = m[1], false
			continue
		}
		if !strings.HasPrefix(line, "|") {
			if inTable {
				done[section] = true
			}
			inTable = false
			continue
		}
		inTable = true
		if done[section] {
			continue
		}
		if m := fieldRowRE.FindStringSubmatch(line); m != nil {
			rows[section] = append(rows[section], m[1])
		}
	}
	for heading, v := range referenceTables {
		var want []string
		typ := reflect.TypeOf(v)
		for i := range typ.NumField() {
			tag, _, _ := strings.Cut(typ.Field(i).Tag.Get("yaml"), ",")
			if tag != "" && tag != "-" {
				want = append(want, tag)
			}
		}
		got := rows[heading]
		slices.Sort(want)
		slices.Sort(got)
		if !slices.Equal(got, want) {
			t.Errorf("%s section %q documents %v; %s has %v", referenceDoc, heading, got, typ.Name(), want)
		}
	}
}
