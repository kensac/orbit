package runner

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/tokens"
)

// anthropicAPI calls the Messages API directly: one request, no tools, with
// the output schema enforced as a forced tool call. Inputs are inlined, so
// it suits agents whose sources are small.
type anthropicAPI struct {
	rs     spec.RunnerSpec
	key    string
	url    string
	client *http.Client
}

// inlineLimit bounds the input bytes inlined into one request. About 150k
// tokens at 4 bytes per token leaves room in a 200k window for the charter,
// memory and the answer. A run over it fails rather than truncating, since a
// truncated input reads as complete to the model.
const inlineLimit = 600 << 10

func newAnthropic(rs spec.RunnerSpec, getenv func(string) (string, bool)) (Runner, error) {
	key, ok := getenv(rs.APIKeyEnv)
	if !ok || key == "" {
		return nil, fmt.Errorf("anthropic: %s is not set", rs.APIKeyEnv)
	}
	if rs.Model == "" {
		return nil, fmt.Errorf("anthropic: model is required")
	}
	url := "https://api.anthropic.com/v1/messages"
	if u := os.Getenv("ORBIT_ANTHROPIC_URL"); u != "" {
		url = u
	}
	return &anthropicAPI{rs: rs, key: key, url: url, client: &http.Client{Timeout: 10 * time.Minute}}, nil
}

func (a *anthropicAPI) Kind() string  { return "anthropic" }
func (a *anthropicAPI) Model() string { return a.rs.Model }

func (a *anthropicAPI) Run(ctx context.Context, req Request, emit func(Event)) (*Result, error) {
	if len(req.Tools) > 0 {
		return nil, fmt.Errorf("anthropic: agent tools %v need a CLI runner", req.Tools)
	}
	var b strings.Builder
	b.WriteString(req.Prompt)
	total := 0
	for _, in := range req.Inputs {
		total += len(in.Content)
		if total > inlineLimit {
			return nil, fmt.Errorf("anthropic: inputs exceed %d bytes; use a CLI runner, which reads them as files", inlineLimit)
		}
		fmt.Fprintf(&b, "\n\n<input name=%q path=%q media_type=%q>\n%s\n</input>", in.Name, in.Path, in.MediaType, in.Content)
	}
	maxTokens := a.rs.MaxTokens
	if maxTokens == 0 {
		maxTokens = 8192
	}
	body, err := json.Marshal(map[string]any{
		"model":      cmp.Or(req.Model, a.rs.Model),
		"max_tokens": maxTokens,
		"system":     req.System,
		"messages":   []any{map[string]any{"role": "user", "content": b.String()}},
		"tools": []any{map[string]any{
			"name":         "submit",
			"description":  "Submit the run's result. Call exactly once.",
			"input_schema": req.OutputSchema,
		}},
		"tool_choice": map[string]any{"type": "tool", "name": "submit"},
	})
	if err != nil {
		return nil, err
	}
	hreq, err := http.NewRequestWithContext(ctx, http.MethodPost, a.url, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	hreq.Header.Set("content-type", "application/json")
	hreq.Header.Set("x-api-key", a.key)
	hreq.Header.Set("anthropic-version", "2023-06-01")
	resp, err := a.client.Do(hreq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxLine))
	if err != nil {
		return nil, err
	}
	emit(Event{Kind: "response", Payload: raw})
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: anthropic %d: %s", ErrBackend, resp.StatusCode, firstLine(string(raw)))
	}
	var m struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Usage   json.RawMessage `json:"usage"`
		Content []struct {
			Type  string          `json:"type"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
		StopReason string `json:"stop_reason"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("parse anthropic response: %w", err)
	}
	res := &Result{Usage: m.Usage, Tokens: tokens.FromAnthropic(m.Usage, m.Model), SessionID: m.ID, Model: m.Model}
	for _, c := range m.Content {
		if c.Type == "tool_use" && c.Name == "submit" {
			res.Output = c.Input
			return res, nil
		}
	}
	return res, fmt.Errorf("%w: anthropic stopped (%s) without calling submit", ErrBackend, m.StopReason)
}
