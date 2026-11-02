// Package actions executes what an agent proposes, once its rule allows it.
// An action type is code here, with a parameter shape the agent is told
// about and a validator; the agent supplies only parameters. Where an
// action reaches (an endpoint's URL, a channel's credentials) is
// configuration, so a model cannot aim a write at anything new.
package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Type describes one action type to the agent and validates its params.
type Type struct {
	Name        string
	Description string
	// ParamsSchema is the JSON Schema of params, shown to the agent and
	// enforced before the action is stored.
	ParamsSchema map[string]any
	Validate     func(params json.RawMessage) error
}

// Catalog returns the action types an agent's spec enables (rule auto or
// approve), in a stable order. Denied types are not offered at all.
func Catalog(a *spec.Agent, d spec.Deployment) []Type {
	var out []Type
	if r := a.RuleFor("notify"); r != spec.RuleDeny {
		out = append(out, notifyType(a))
	}
	if r := a.RuleFor("continue"); r != spec.RuleDeny {
		out = append(out, continueType())
	}
	if targets := a.DelegateTargets(); len(targets) > 0 {
		if a.RuleFor(DelegateType) != spec.RuleDeny {
			out = append(out, delegateType(targets))
		}
		if a.RuleFor(TaskMessageType) != spec.RuleDeny {
			out = append(out, taskMessageType())
		}
		if a.RuleFor(TaskCancelType) != spec.RuleDeny {
			out = append(out, taskCancelType())
		}
	}
	for _, name := range slices.Sorted(maps.Keys(d.Endpoints)) {
		typ := "webhook:" + name
		if a.RuleFor(typ) != spec.RuleDeny {
			out = append(out, webhookType(name, d.Endpoints[name]))
		}
	}
	if d.GitHub != nil && a.RuleFor(MergeType) != spec.RuleDeny {
		out = append(out, mergeType(d))
	}
	if d.GitHub != nil && len(d.GitHub.CommentRepositories) > 0 && a.RuleFor(CommentType) != spec.RuleDeny {
		out = append(out, commentType(d))
	}
	if a.RuleFor(DefineType) != spec.RuleDeny {
		out = append(out, defineType(a))
	}
	return out
}

// DefineType creates or changes an agent defined in the database, this
// agent included: its charter, schedule, tools, sources, rules. It applies
// as a new version of that agent (every version kept, revertible through
// the API), recording the run and the person it answered.
const DefineType = "agent:define"

type defineParams struct {
	Spec json.RawMessage `json:"spec"`
}

func defineType(a *spec.Agent) Type {
	return Type{
		Name: DefineType,
		Description: "Create an agent or change one (yourself included, \"" + a.Name + "\"): spec is the whole agent, " +
			"the same fields as an agents/<name>.yaml file (name, description, charter, triggers, sources, tools, actions, " +
			"session, notify, ...; durations as strings like \"15m\"). It replaces the agent's spec as a new version; " +
			"every version is kept and can be reverted. Agents defined in configuration files cannot be changed this way. " +
			"A change that adds a tool, source, credential, delegate target or need, or allows an action more readily, waits for a person's approval. " +
			"Such an agent may use, without a sandbox, only the tools " + strings.Join(spec.UnsandboxedTools, ", ") + "; never a file source or a ${VAR} but orbit.yaml's credentials.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"spec": map[string]any{"type": "object", "properties": map[string]any{
					"name": map[string]any{"type": "string", "pattern": spec.AgentNamePattern}}, "required": []string{"name"}},
			},
			"required":             []string{"spec"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := DecodeDefine(p)
			return err
		},
	}
}

// DecodeDefine reads agent:define's params: the spec, decoded as strictly
// as a file, with a name.
func DecodeDefine(p json.RawMessage) (json.RawMessage, error) {
	var v defineParams
	if err := strictDecode(p, &v); err != nil {
		return nil, err
	}
	ag, err := spec.DecodeAgent(v.Spec)
	if err != nil {
		return nil, fmt.Errorf("agent:define: spec: %w", err)
	}
	if ag.Name == "" {
		return nil, fmt.Errorf("agent:define: spec.name is required")
	}
	return v.Spec, nil
}

// MergeType squash-merges one of orbit's own PRs once its checks are green.
// In the fleet a merge is a deploy, so the executor re-checks every gate
// itself at execution time; the agent supplies only which PR and which
// commit it means.
const MergeType = "github:merge"

type mergeParams struct {
	Repo       string `json:"repo"`
	PullNumber int    `json:"pull_number"`
	HeadSHA    string `json:"head_sha"`
}

func mergeType(d spec.Deployment) Type {
	return Type{
		Name: MergeType,
		Description: "Squash-merge one of orbit's own pull requests (an orbit/* branch). " +
			"It merges only if the PR's head is still head_sha and every required check passed.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"repo":        map[string]any{"type": "string", "enum": d.GitHub.Repositories},
				"pull_number": map[string]any{"type": "integer", "minimum": 1},
				"head_sha":    map[string]any{"type": "string", "pattern": "^[0-9a-f]{40}$"},
			},
			"required":             []string{"repo", "pull_number", "head_sha"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := decodeMerge(d, p)
			return err
		},
	}
}

func decodeMerge(d spec.Deployment, p json.RawMessage) (mergeParams, error) {
	var v mergeParams
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	if !d.AllowsRepo(v.Repo) {
		return v, fmt.Errorf("github:merge: repository %q is not in github.repositories", v.Repo)
	}
	if v.PullNumber < 1 {
		return v, fmt.Errorf("github:merge: pull_number must be positive")
	}
	if len(v.HeadSHA) != 40 || strings.Trim(v.HeadSHA, "0123456789abcdef") != "" {
		return v, fmt.Errorf("github:merge: head_sha must be a full commit sha")
	}
	return v, nil
}

func notifyType(a *spec.Agent) Type {
	return Type{
		Name:        "notify",
		Description: "Send an extra message to one of your channels, separate from the run report.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"channel":  map[string]any{"type": "string", "enum": a.Notify.Channels},
				"title":    map[string]any{"type": "string", "maxLength": 200},
				"body":     map[string]any{"type": "string", "maxLength": 20000},
				"priority": map[string]any{"type": "string", "enum": []string{"low", "default", "high"}},
			},
			"required":             []string{"channel", "title", "body"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			var v notifyParams
			if err := strictDecode(p, &v); err != nil {
				return err
			}
			if v.Title == "" || v.Body == "" {
				return fmt.Errorf("notify: title and body are required")
			}
			for _, c := range a.Notify.Channels {
				if c == v.Channel {
					return nil
				}
			}
			return fmt.Errorf("notify: channel %q is not one of this agent's channels", v.Channel)
		},
	}
}

type notifyParams struct {
	Channel  string `json:"channel"`
	Title    string `json:"title"`
	Body     string `json:"body"`
	Priority string `json:"priority"`
}

// ContinueMinDelay is the shortest wait a continuation may ask for. The
// executors poll every few seconds, so anything shorter is a busy loop
// rather than a checkpoint.
const ContinueMinDelay = time.Minute

// ContinueMaxDelay is the longest. A wake further out than a day is a
// schedule, which belongs in the agent's spec where a person can see it.
const ContinueMaxDelay = 24 * time.Hour

// MaxContinueChain bounds continuations in a row: each is a full run
// (up to its timeout), so twelve is a long task's worth of checkpoints, and
// past that a loop is likelier than progress. Any run that is not a
// continuation (a message, a schedule) starts the count again.
const MaxContinueChain = 12

func continueType() Type {
	return Type{
		Name: "continue",
		Description: fmt.Sprintf("Be woken again after_minutes from now (%d to %d) to carry on a longer task. "+
			"note is what your next run is told: what you were doing and what to do next. "+
			"At most %d continuations in a row; one waits at a time.",
			int(ContinueMinDelay/time.Minute), int(ContinueMaxDelay/time.Minute), MaxContinueChain),
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"after_minutes": map[string]any{"type": "integer",
					"minimum": int(ContinueMinDelay / time.Minute), "maximum": int(ContinueMaxDelay / time.Minute)},
				"note": map[string]any{"type": "string", "minLength": 1, "maxLength": 4000},
			},
			"required":             []string{"after_minutes", "note"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			v, err := decodeContinue(p)
			if err != nil {
				return err
			}
			if v.Note == "" {
				return fmt.Errorf("continue: note is required")
			}
			d := time.Duration(v.AfterMinutes) * time.Minute
			if d < ContinueMinDelay || d > ContinueMaxDelay {
				return fmt.Errorf("continue: after_minutes must be between %d and %d",
					int(ContinueMinDelay/time.Minute), int(ContinueMaxDelay/time.Minute))
			}
			return nil
		},
	}
}

type continueParams struct {
	AfterMinutes int    `json:"after_minutes"`
	Note         string `json:"note"`
}

func decodeContinue(p json.RawMessage) (continueParams, error) {
	var v continueParams
	err := strictDecode(p, &v)
	return v, err
}

// Executor runs approved actions.
type Executor struct {
	Deployment spec.Deployment
	Channels   map[string]notify.Channel
	Client     *http.Client
	Env        func(string) (string, bool)
	// MergeOptions tunes github:merge's wait for mergeability (tests).
	MergeOptions github.MergeOptions
	// Tasks carries out delegation (delegate, task:message, task:cancel);
	// nil fails them.
	Tasks TaskOps
	// Continue enqueues a continuation for agent, asked for by run fromRun.
	// It returns the queued run's id, and queued false when one was
	// already waiting (whose id it returns instead).
	Continue func(ctx context.Context, agent, fromRun string, after time.Duration, note string) (id string, queued bool, err error)
	// Define applies an agent:define for the call's run.
	Define func(ctx context.Context, c Call, spec json.RawMessage) (json.RawMessage, error)
}

// Call is one action to execute: its type and parameters, and the agent
// and run that proposed it.
type Call struct {
	// ID is the action's own id.
	ID     string
	Agent  string
	RunID  string
	Type   string
	Params json.RawMessage
	// Rule is the rule it was recorded under: auto, or approve when a person approved it.
	Rule spec.Rule
}

// Execute performs one action and returns a JSON result to store.
func (e *Executor) Execute(ctx context.Context, c Call) (json.RawMessage, error) {
	typ, params := c.Type, c.Params
	switch {
	case typ == "continue":
		v, err := decodeContinue(params)
		if err != nil {
			return nil, err
		}
		if e.Continue == nil {
			return nil, fmt.Errorf("continue: not available in this process")
		}
		id, queued, err := e.Continue(ctx, c.Agent, c.RunID, time.Duration(v.AfterMinutes)*time.Minute, v.Note)
		if err != nil {
			return nil, err
		}
		// A continuation already waiting is the agent's next wake; this one
		// adds nothing, and its note stays in this action's params.
		result, _ := json.Marshal(map[string]any{"run_id": id, "queued": queued})
		return result, nil
	case typ == "notify":
		var p notifyParams
		if err := strictDecode(params, &p); err != nil {
			return nil, err
		}
		ch, ok := e.Channels[p.Channel]
		if !ok {
			return nil, fmt.Errorf("channel %q is not configured", p.Channel)
		}
		pr := notify.Priority(p.Priority)
		if pr == "" {
			pr = notify.Default
		}
		if err := ch.Send(ctx, notify.Message{Title: p.Title, Body: p.Body, Priority: pr}); err != nil {
			return nil, err
		}
		return json.RawMessage(`{"sent":true}`), nil
	case strings.HasPrefix(typ, "webhook:"):
		return e.webhook(ctx, strings.TrimPrefix(typ, "webhook:"), params)
	case typ == MergeType:
		return e.merge(ctx, params)
	case typ == CommentType:
		return e.comment(ctx, params)
	case typ == store.DecisionType:
		// A person approved it; the task it holds starts at the next
		// reconcile. Nothing else happens here.
		return json.RawMessage(`{"decided":true}`), nil
	case typ == DelegateType, typ == TaskMessageType, typ == TaskCancelType:
		if e.Tasks == nil {
			return nil, fmt.Errorf("%s: not available in this process", typ)
		}
		return e.executeTask(ctx, c)
	case typ == DefineType:
		sp, err := DecodeDefine(params)
		if err != nil {
			return nil, err
		}
		if e.Define == nil {
			return nil, fmt.Errorf("agent:define: not available in this process")
		}
		return e.Define(ctx, c, sp)
	}
	return nil, fmt.Errorf("unknown action type %q", typ)
}

// merge mints a merge token for the one repository, runs the gated merge,
// and revokes the token whatever happened.
func (e *Executor) merge(ctx context.Context, params json.RawMessage) (json.RawMessage, error) {
	p, err := decodeMerge(e.Deployment, params)
	if err != nil {
		return nil, err
	}
	app, err := github.NewApp(e.Deployment.GitHub, e.Env, e.Client)
	if err != nil {
		return nil, err
	}
	tok, err := app.Token(ctx, p.Repo, github.MergePermissions)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	sha, err := github.Merge(ctx, tok.Client(), github.MergeRequest{Repo: p.Repo, Number: p.PullNumber, HeadSHA: p.HeadSHA}, e.MergeOptions)
	if err != nil {
		return nil, err
	}
	return json.Marshal(map[string]any{"merged": true, "repo": p.Repo, "pull_number": p.PullNumber, "merge_sha": sha})
}

func strictDecode(p json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}
