package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/forms"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/templates"
)

// Creating an agent from a template (docs/api.md, Templates) is code: the template's version is read, its parameters checked and its slots filled (templates.Render), and the agent it gives is dry-run through DefineAgent, the path POST /agents takes, before anything is stored. The API then defines it; an agent's template:create becomes an agent:define a person approves.

// NeedsValues is a creation that cannot render yet: these parameters have no value and no default. Through the API they are the form the client shows; in a conversation orbit asks the person with them.
type NeedsValues struct {
	Template string
	Fields   []forms.Field
}

func (n *NeedsValues) Error() string {
	names := make([]string, len(n.Fields))
	for i, f := range n.Fields {
		names[i] = f.Name
	}
	return fmt.Sprintf("template %s needs values for %s", n.Template, strings.Join(names, ", "))
}

// TemplateRequest is one creation of an agent from a template.
type TemplateRequest struct {
	Template string
	// Version is the template's version; 0 is its current one.
	Version int
	// Name, when set, replaces the name the template renders.
	Name   string
	Values map[string]json.RawMessage
	DryRun bool
	// Author is the person creating it, or on whose instruction an agent proposes it.
	Author string
	// ConversationID is whose uploads a file parameter may name; "" refuses file values.
	ConversationID string
}

// FromTemplate is what a creation rendered and did.
type FromTemplate struct {
	Template *store.Template
	// Spec is the rendered agent, as given to DefineAgent.
	Spec json.RawMessage
	*Defined
	// Widens is what the new agent holds beyond nothing (spec.Widening), which is why it waits for a person when an agent proposes it.
	Widens []string
}

// renderTemplate reads the template's version and renders it with the request's values. A template that is retired, or values that leave a required or user_prompt parameter without a value, is refused (the latter as *NeedsValues).
func (e *Engine) renderTemplate(ctx context.Context, req TemplateRequest) (*store.Template, json.RawMessage, error) {
	tpl, err := e.Store.GetTemplate(ctx, req.Template, req.Version, true)
	if err != nil {
		return nil, nil, err
	}
	if tpl.Status != "active" {
		return nil, nil, fmt.Errorf("%w: template %s version %d is retired", store.ErrInvalid, tpl.Name, tpl.Version)
	}
	doc := tpl.Doc()
	if missing := doc.Missing(req.Values, templates.Required, templates.UserPrompt); len(missing) > 0 {
		n := &NeedsValues{Template: tpl.Name}
		for _, p := range missing {
			n.Fields = append(n.Fields, p.Field())
		}
		return tpl, nil, n
	}
	out, err := doc.Render(req.Values, e.uploadText(ctx, req.ConversationID))
	if err != nil {
		return nil, nil, fmt.Errorf("%w: template %s: %w", store.ErrInvalid, tpl.Name, err)
	}
	if req.Name != "" {
		var m map[string]any
		if err := json.Unmarshal(out, &m); err != nil {
			return nil, nil, err
		}
		m["name"] = req.Name
		if out, err = json.Marshal(m); err != nil {
			return nil, nil, err
		}
	}
	return tpl, out, nil
}

// uploadText reads a file parameter's value: an upload of the conversation, as UTF-8 text within templates.MaxFileBytes.
func (e *Engine) uploadText(ctx context.Context, conversationID string) templates.FileReader {
	return func(id string) (string, error) {
		if conversationID == "" {
			return "", errors.New("a file value names an upload of a conversation; give conversation_id")
		}
		u, _, err := e.Store.OpenUpload(ctx, conversationID, id)
		if err != nil {
			return "", fmt.Errorf("upload %s is not one of this conversation's: %w", id, err)
		}
		if u.Bytes > templates.MaxFileBytes {
			return "", fmt.Errorf("upload %s is %d bytes; a file parameter is at most %d", id, u.Bytes, templates.MaxFileBytes)
		}
		b, err := e.Store.UploadContents(ctx, []string{id})
		if err != nil {
			return "", err
		}
		return string(b[id]), nil
	}
}

// CreateFromTemplate renders a template and defines the agent it gives, always as a dry run first; with DryRun it stops there. The agent is new: a name already defined is refused as POST /agents refuses it.
func (e *Engine) CreateFromTemplate(ctx context.Context, req TemplateRequest) (*FromTemplate, error) {
	tpl, out, err := e.renderTemplate(ctx, req)
	if err != nil {
		return nil, err
	}
	res := &FromTemplate{Template: tpl, Spec: out}
	d, err := e.DefineAgent(ctx, DefineRequest{Spec: out, DryRun: true, Author: req.Author})
	if err != nil {
		return nil, err
	}
	res.Defined, res.Widens = d, spec.Widening(nil, d.Agent)
	if req.DryRun {
		return res, nil
	}
	if res.Defined, err = e.DefineAgent(ctx, DefineRequest{Spec: out, Author: req.Author}); err != nil {
		return nil, err
	}
	return res, nil
}

// templateQuestion is the question orbit asks for a template's user_prompt parameters. Its text is code's, the same every time, which is how the person's answers to it are found again (store.FormAnswers).
func templateQuestion(name string) string {
	return fmt.Sprintf("To create an agent from the template %s, I need these from you.", name)
}

// templateProposal is what a run's template:create becomes: the agent:define params of the rendered agent, validated by a dry run, or the question with a form that asks the person for what is missing. err is why it is denied. A user_prompt parameter's value is the person's: it comes from their answers to orbit's question in this conversation, never from the proposal, and is asked when there are none; a required parameter the proposal lacks is refused.
func (e *Engine) templateProposal(ctx context.Context, conversationID string, params json.RawMessage) (json.RawMessage, *store.Question, error) {
	p, err := actions.DecodeTemplateCreate(params)
	if err != nil {
		return nil, nil, err
	}
	tpl, err := e.Store.GetTemplate(ctx, p.Template, p.Version, false)
	if err != nil {
		return nil, nil, fmt.Errorf("template %s: %w", p.Template, err)
	}
	answers, err := e.Store.FormAnswers(ctx, conversationID, templateQuestion(tpl.Name))
	if err != nil {
		return nil, nil, err
	}
	values := map[string]json.RawMessage{}
	for _, prm := range tpl.Parameters {
		v, ok := p.Parameters[prm.Name]
		if prm.Mode == templates.UserPrompt {
			v, ok = answers[prm.Name]
		}
		if ok {
			values[prm.Name] = v
		}
	}
	for name := range p.Parameters {
		if !slices.ContainsFunc(tpl.Parameters, func(prm templates.Parameter) bool { return prm.Name == name }) {
			return nil, nil, fmt.Errorf("template %s has no parameter %q", tpl.Name, name)
		}
	}
	doc := tpl.Doc()
	if missing := doc.Missing(values, templates.Required); len(missing) > 0 {
		names := make([]string, len(missing))
		for i, m := range missing {
			names[i] = m.Name
		}
		return nil, nil, fmt.Errorf("template %s needs values for %s", tpl.Name, strings.Join(names, ", "))
	}
	// A user_prompt parameter is asked even when it has a default: the default is what the form starts with, and the choice is the person's.
	q := &store.Question{Body: templateQuestion(tpl.Name)}
	for _, prm := range tpl.Parameters {
		if _, ok := values[prm.Name]; prm.Mode == templates.UserPrompt && !ok {
			q.Fields = append(q.Fields, prm.Field())
		}
	}
	if len(q.Fields) > 0 {
		return nil, q, nil
	}
	res, err := e.CreateFromTemplate(ctx, TemplateRequest{Template: tpl.Name, Version: tpl.Version, Name: p.Name,
		Values: values, DryRun: true, ConversationID: conversationID})
	if err != nil {
		return nil, nil, err
	}
	define, err := json.Marshal(map[string]any{"spec": res.Spec,
		"template": actions.TemplateRef{Name: res.Template.Name, Version: res.Template.Version}})
	return define, nil, err
}

// templateOutcome records a run's template:create proposal as what it becomes: an agent:define waiting for a person, or, when the person must still give values, a question with their form (and the proposal denied, saying so). The define waits whatever the rules: a new agent is a widening.
func (e *Engine) templateOutcome(ctx context.Context, conversationID string, pa *store.ProposedAction, questions *[]store.Question) {
	define, ask, err := e.templateProposal(ctx, conversationID, pa.Params)
	switch {
	case err != nil:
		pa.Rule, pa.Error = string(spec.RuleDeny), err.Error()
	case ask != nil:
		*questions = append(*questions, *ask)
		names := make([]string, len(ask.Fields))
		for i, f := range ask.Fields {
			names[i] = f.Name
		}
		pa.Rule, pa.Error = string(spec.RuleDeny), "asked the person for "+strings.Join(names, ", ")+"; propose it again with their answers"
	default:
		pa.Type, pa.Params, pa.Rule = actions.DefineType, define, string(spec.RuleApprove)
	}
}
