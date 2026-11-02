package actions

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kensac/orbit/internal/spec"
)

// A webhook action sends a JSON body to an endpoint in orbit.yaml. The model chooses the body and, where the endpoint's path has placeholders, their values, each checked against the endpoint's params; never the URL, the method or a header.

type webhookParams struct {
	Path map[string]json.RawMessage `json:"path"`
	Body json.RawMessage            `json:"body"`
}

func webhookType(name string, ep spec.Endpoint) Type {
	method := ep.Method
	if method == "" {
		method = http.MethodPost
	}
	desc := fmt.Sprintf("Send a JSON body to the %q endpoint (%s", name, method)
	if ep.Path != "" {
		desc += " " + ep.Path
	}
	desc += ")."
	if ep.Description != "" {
		desc += " " + ep.Description
	}
	body := map[string]any{"type": "object"}
	if ep.BodySchema != nil {
		body = ep.BodySchema
	}
	props := map[string]any{"body": body}
	required := []string{"body"}
	if names := ep.PathParams(); len(names) > 0 {
		pp := map[string]any{}
		for _, n := range names {
			pp[n] = ep.Params[n].JSONSchema()
		}
		props["path"] = map[string]any{"type": "object", "properties": pp, "required": names, "additionalProperties": false}
		required = append(required, "path")
		desc += " path fills the placeholders of " + ep.Path + "."
	}
	return Type{
		Name:        "webhook:" + name,
		Description: desc,
		ParamsSchema: map[string]any{
			"type":                 "object",
			"properties":           props,
			"required":             required,
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, _, err := decodeWebhook(name, ep, p)
			return err
		},
	}
}

// decodeWebhook checks params against the endpoint and returns the URL and the body to send.
func decodeWebhook(name string, ep spec.Endpoint, p json.RawMessage) (string, json.RawMessage, error) {
	var v webhookParams
	if err := strictDecode(p, &v); err != nil {
		return "", nil, err
	}
	if len(v.Body) == 0 || v.Body[0] != '{' {
		return "", nil, fmt.Errorf("webhook: body must be a JSON object")
	}
	u, err := ep.RenderURL(v.Path)
	if err != nil {
		return "", nil, fmt.Errorf("webhook:%s: %w", name, err)
	}
	sch, err := ep.CompileBodySchema()
	if err != nil {
		return "", nil, fmt.Errorf("webhook:%s: body_schema: %w", name, err)
	}
	if sch != nil {
		inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(v.Body))
		if err != nil {
			return "", nil, fmt.Errorf("webhook:%s: body: %w", name, err)
		}
		if err := sch.Validate(inst); err != nil {
			var ve *jsonschema.ValidationError
			if errors.As(err, &ve) {
				return "", nil, fmt.Errorf("webhook:%s: body does not match the endpoint's schema: %s", name, leaves(ve.BasicOutput()))
			}
			return "", nil, fmt.Errorf("webhook:%s: body: %w", name, err)
		}
	}
	return u, v.Body, nil
}

// leaves names each failing location of a schema check.
func leaves(o *jsonschema.OutputUnit) string {
	var parts []string
	var walk func(u *jsonschema.OutputUnit)
	walk = func(u *jsonschema.OutputUnit) {
		if u.Error != nil && len(u.Errors) == 0 {
			loc := u.InstanceLocation
			if loc == "" {
				loc = "/"
			}
			parts = append(parts, loc+": "+u.Error.String())
		}
		for i := range u.Errors {
			walk(&u.Errors[i])
		}
	}
	walk(o)
	if len(parts) == 0 {
		return "does not match"
	}
	if len(parts) > 10 {
		parts = append(parts[:10], fmt.Sprintf("and %d more", len(parts)-10))
	}
	return strings.Join(parts, "; ")
}

// webhook sends one webhook action. A credential in a header is sent only to a URL under its prefix (spec.Credentials), and no redirect is followed: a write is not re-sent somewhere else, so a 3xx is the result and fails the action.
func (e *Executor) webhook(ctx context.Context, name string, params json.RawMessage) (json.RawMessage, error) {
	ep, ok := e.Deployment.Endpoints[name]
	if !ok {
		return nil, fmt.Errorf("endpoint %q is not configured", name)
	}
	u, body, err := decodeWebhook(name, ep, params)
	if err != nil {
		return nil, err
	}
	method := ep.Method
	if method == "" {
		method = http.MethodPost
	}
	for _, r := range ep.EnvRefs() {
		if err := e.Deployment.Credentials.Check(r, u); err != nil {
			return nil, fmt.Errorf("endpoint %q: %w", name, err)
		}
	}
	req, err := http.NewRequestWithContext(ctx, method, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "orbit/1 (+https://github.com/kensac/orbit)")
	for k, v := range ep.Headers {
		rv, err := spec.ExpandEnv(v, e.Env)
		if err != nil {
			return nil, fmt.Errorf("endpoint %q header %s: %w", name, k, err)
		}
		req.Header.Set(k, rv)
	}
	client := *e.Client
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := client.Do(req)
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
