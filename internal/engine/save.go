package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// "Save this conversation as an agent" (agent:save): the model writes the words, code writes the reach. Everything that decides what the new agent can do (its tools, sources, rules and schedules) comes from the conversation's record (store.ConversationFacts), never from the model, and is no broader than what happened there: a tool it called, a page it fetched, an action type its person approved (as approve, never auto), a schedule asked for. The result is an agent:define proposal held for a person whatever the rule, shown to them as a dry-run diff (previewDefine).

// maxSavedSources bounds the sources a saved agent gets from pages fetched in its conversation: each is fetched before every run.
const maxSavedSources = 5

var sourceNameUnsafe = regexp.MustCompile(`[^a-z0-9-]+`)

// draftAgent is the spec agent:save p proposes from conversation conversationID of agent from, with asked, the schedules asked for in the same reply.
func (e *Engine) draftAgent(ctx context.Context, from *spec.Agent, conversationID string, p actions.SaveParams, asked []store.ScheduleAsk) (*spec.Agent, error) {
	cfg := e.Current().Config
	if cfg.Agents[p.Name] != nil {
		return nil, fmt.Errorf("agent:save: an agent named %q exists; choose another name", p.Name)
	}
	facts, err := e.Store.ConversationFacts(ctx, conversationID)
	if err != nil {
		return nil, err
	}
	a := &spec.Agent{Name: p.Name, Description: p.Description, Charter: p.Charter, Starters: p.Starters,
		Timezone: from.Timezone, Runner: from.Runner, Chat: from.Chat, MessagesTrigger: true}
	if from.Session != nil {
		sess := *from.Session
		a.Session = &sess
	}
	sandboxed := false
	for _, t := range facts.Tools {
		// mcp__ tools are orbit's own (query, calc, search, its chat tools), given to every run.
		if strings.HasPrefix(t, "mcp__") {
			continue
		}
		if !slices.Contains(spec.UnsandboxedTools, t) {
			if from.Sandbox == "" {
				continue
			}
			sandboxed = true
		}
		a.Tools = append(a.Tools, t)
	}
	if sandboxed {
		a.Sandbox, a.Needs = from.Sandbox, slices.Clone(from.Needs)
	}
	names := map[string]bool{}
	for _, raw := range facts.Fetched {
		if len(a.Sources) == maxSavedSources {
			break
		}
		u, err := url.Parse(raw)
		if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" || u.User != nil ||
			strings.Contains(raw, "${") || strings.Contains(raw, "{{") {
			continue
		}
		base := strings.Trim(sourceNameUnsafe.ReplaceAllString(strings.ToLower(strings.TrimPrefix(u.Hostname(), "www.")), "-"), "-")
		if base == "" || base[0] < 'a' || base[0] > 'z' {
			base = "web-" + base
		}
		if len(base) > 56 {
			base = strings.TrimRight(base[:56], "-")
		}
		name := base
		for i := 2; names[name]; i++ {
			name = fmt.Sprintf("%s-%d", base, i)
		}
		names[name] = true
		a.Sources = append(a.Sources, spec.Source{Name: name, Kind: "http", URL: raw})
	}
	for _, t := range facts.Approved {
		// Delegation would need targets the record does not carry; agent:save is converted, never stored as approved.
		if t == actions.SaveType || actions.IsTaskType(t) {
			continue
		}
		if a.Actions == nil {
			a.Actions = spec.ActionSet{}
		}
		a.Actions[t] = spec.RuleApprove
	}
	now := time.Now()
	for _, s := range append(facts.Schedules, asked...) {
		if _, err := actions.ApplySchedule(a, from.Timezone, actions.ScheduleAddType, actions.ScheduleParams{When: s.When, Prompt: s.Prompt}, now); err != nil {
			continue // asked in words code could not read: the proposal said so then
		}
	}
	return a, nil
}

// saveProposal turns one agent:save of run c into the agent:define it proposes, always held for a person. asked are the schedule:add proposals of the same reply aimed at the new agent, which it absorbs.
func (e *Engine) saveProposal(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, act proposed, asked []store.ScheduleAsk) store.ProposedAction {
	pa := store.ProposedAction{Type: act.Type, Params: act.Params, Reason: act.Reason, Rule: string(spec.RuleDeny)}
	p, err := actions.DecodeSave(act.Params)
	if err != nil {
		pa.Error = "invalid params: " + err.Error()
		return pa
	}
	draft, err := e.draftAgent(ctx, a, c.ConversationID, p, asked)
	if err != nil {
		pa.Error = err.Error()
		return pa
	}
	b, err := json.Marshal(map[string]any{"spec": draft})
	if err != nil {
		pa.Error = err.Error()
		return pa
	}
	if _, err := e.DefineAgent(ctx, DefineRequest{Spec: mustSpec(b), DryRun: true}); err != nil {
		pa.Error = "the drafted agent does not load: " + err.Error()
		return pa
	}
	return store.ProposedAction{Type: actions.DefineType, Params: b, Reason: "Save this conversation as an agent: " + act.Reason, Rule: string(spec.RuleApprove)}
}

func mustSpec(params []byte) json.RawMessage {
	var v struct {
		Spec json.RawMessage `json:"spec"`
	}
	_ = json.Unmarshal(params, &v)
	return v.Spec
}

// proposed is one action as a run's output carries it.
type proposed struct {
	Type   string
	Params json.RawMessage
	Reason string
}

// absorbSchedules splits a reply's actions: the schedule:add proposals aimed at an agent an agent:save of the same reply creates become that agent's schedules instead of actions of their own.
func absorbSchedules(acts []proposed) (rest []proposed, asked map[string][]store.ScheduleAsk) {
	saving := map[string]bool{}
	for _, act := range acts {
		if act.Type == actions.SaveType {
			if p, err := actions.DecodeSave(act.Params); err == nil {
				saving[p.Name] = true
			}
		}
	}
	asked = map[string][]store.ScheduleAsk{}
	for _, act := range acts {
		if act.Type == actions.ScheduleAddType {
			if p, err := actions.DecodeSchedule(act.Type, act.Params); err == nil && saving[p.Agent] {
				asked[p.Agent] = append(asked[p.Agent], store.ScheduleAsk{When: p.When, Prompt: p.Prompt})
				continue
			}
		}
		rest = append(rest, act)
	}
	return rest, asked
}

// previewDefine is a pending agent:define as a dry run: the change it makes, how it widens the agent, and a line diff of the spec from the current version (nothing, for a new agent) to the proposed one.
func (e *Engine) previewDefine(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	sp, err := actions.DecodeDefine(params)
	if err != nil {
		return nil, err
	}
	d, err := e.DefineAgent(ctx, DefineRequest{Spec: sp, Upsert: true, DryRun: true})
	if err != nil {
		return nil, err
	}
	name := d.Agent.Name
	cur := e.Current().Config.Agents[name]
	before, change := "", "create"
	if cur != nil {
		change = "update"
		if a, _, err := e.definedSpec(ctx, name); err == nil {
			before = indentSpec(a)
		}
	}
	next, err := spec.DecodeAgent(sp)
	if err != nil {
		return nil, err
	}
	widens := spec.Widening(cur, d.Agent)
	if widens == nil {
		widens = []string{}
	}
	return json.Marshal(map[string]any{"agent": name, "change": change, "widens": widens, "diff": lineDiff(before, indentSpec(next))})
}

func indentSpec(a *spec.Agent) string {
	b, _ := json.MarshalIndent(a, "", "  ")
	return string(b) + "\n"
}

// lineDiff is b against a, line by line: every line once, prefixed "  " (in both), "- " (only in a) or "+ " (only in b). Specs are a few hundred lines at most, so the quadratic longest common subsequence is cheap, and every line is shown so the person reads the whole agent.
func lineDiff(a, b string) string {
	split := func(s string) []string {
		if s == "" {
			return nil
		}
		return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
	}
	x, y := split(a), split(b)
	lcs := make([][]int, len(x)+1)
	for i := range lcs {
		lcs[i] = make([]int, len(y)+1)
	}
	for i := len(x) - 1; i >= 0; i-- {
		for j := len(y) - 1; j >= 0; j-- {
			if x[i] == y[j] {
				lcs[i][j] = lcs[i+1][j+1] + 1
			} else {
				lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
			}
		}
	}
	var out strings.Builder
	i, j := 0, 0
	for i < len(x) || j < len(y) {
		switch {
		case i < len(x) && j < len(y) && x[i] == y[j]:
			out.WriteString("  " + x[i] + "\n")
			i, j = i+1, j+1
		case j < len(y) && (i == len(x) || lcs[i][j+1] >= lcs[i+1][j]):
			out.WriteString("+ " + y[j] + "\n")
			j++
		default:
			out.WriteString("- " + x[i] + "\n")
			i++
		}
	}
	return out.String()
}

func saveName(params json.RawMessage) string {
	p, _ := actions.DecodeSave(params)
	return p.Name
}
