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
	"io"
	"maps"
	"net/http"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/spec"
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
	for _, name := range slices.Sorted(maps.Keys(d.Endpoints)) {
		typ := "webhook:" + name
		if a.RuleFor(typ) != spec.RuleDeny {
			out = append(out, webhookType(name))
		}
	}
	return out
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

func webhookType(endpoint string) Type {
	return Type{
		Name:        "webhook:" + endpoint,
		Description: fmt.Sprintf("Send a JSON body to the %q endpoint.", endpoint),
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"body": map[string]any{"type": "object"},
			},
			"required":             []string{"body"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			var v struct {
				Body json.RawMessage `json:"body"`
			}
			if err := strictDecode(p, &v); err != nil {
				return err
			}
			if len(v.Body) == 0 || v.Body[0] != '{' {
				return fmt.Errorf("webhook: body must be a JSON object")
			}
			return nil
		},
	}
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
	// Continue enqueues a continuation for agent, asked for by run fromRun.
	// It returns the queued run's id, and queued false when one was
	// already waiting (whose id it returns instead).
	Continue func(ctx context.Context, agent, fromRun string, after time.Duration, note string) (id string, queued bool, err error)
}

// Call is one action to execute: its type and parameters, and the agent
// and run that proposed it.
type Call struct {
	Agent  string
	RunID  string
	Type   string
	Params json.RawMessage
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
		name := strings.TrimPrefix(typ, "webhook:")
		ep, ok := e.Deployment.Endpoints[name]
		if !ok {
			return nil, fmt.Errorf("endpoint %q is not configured", name)
		}
		var p struct {
			Body json.RawMessage `json:"body"`
		}
		if err := strictDecode(params, &p); err != nil {
			return nil, err
		}
		method := ep.Method
		if method == "" {
			method = http.MethodPost
		}
		req, err := http.NewRequestWithContext(ctx, method, ep.URL, bytes.NewReader(p.Body))
		if err != nil {
			return nil, err
		}
		req.Header.Set("Content-Type", "application/json")
		for k, v := range ep.Headers {
			rv, err := spec.ExpandEnv(v, e.Env)
			if err != nil {
				return nil, fmt.Errorf("endpoint %q header %s: %w", name, k, err)
			}
			req.Header.Set(k, rv)
		}
		resp, err := e.Client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		result, _ := json.Marshal(map[string]any{"status": resp.StatusCode, "body": string(b)})
		if resp.StatusCode/100 != 2 {
			return result, fmt.Errorf("%s %s: %s", method, name, resp.Status)
		}
		return result, nil
	}
	return nil, fmt.Errorf("unknown action type %q", typ)
}

func strictDecode(p json.RawMessage, into any) error {
	dec := json.NewDecoder(bytes.NewReader(p))
	dec.DisallowUnknownFields()
	return dec.Decode(into)
}
