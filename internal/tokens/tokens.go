// Package tokens is a backend call's token counts in one shape for every backend: what was sent fresh, read from the prompt cache, written to it, and generated. Whether a conversation's cache hits is what decides what a turn costs, because every turn sends the whole conversation again.
package tokens

import (
	"encoding/json"
	"sort"
)

// Count is one call's tokens. Input is the prompt sent uncached; CacheRead what was read from the prompt cache; CacheCreation what was written to it; Output what the model generated.
type Count struct {
	Input         int64 `json:"input"`
	Output        int64 `json:"output"`
	CacheRead     int64 `json:"cache_read"`
	CacheCreation int64 `json:"cache_creation"`
}

// Prompt is every prompt token: uncached, read from the cache and written to it.
func (c Count) Prompt() int64 { return c.Input + c.CacheRead + c.CacheCreation }

// HitRatio is the share of prompt tokens read from the cache, nil when there were none.
func HitRatio(cacheRead, prompt int64) *float64 {
	if prompt <= 0 {
		return nil
	}
	r := float64(cacheRead) / float64(prompt)
	return &r
}

// Add adds o to c.
func (c *Count) Add(o Count) {
	c.Input += o.Input
	c.Output += o.Output
	c.CacheRead += o.CacheRead
	c.CacheCreation += o.CacheCreation
}

// Usage is one attempt's tokens as orbit records them: the totals, the model that did most of the work, the backend's turn count, and the totals per model when the backend splits them (Claude Code runs subagents on other models).
type Usage struct {
	Count
	Model    string           `json:"model,omitempty"`
	NumTurns int              `json:"num_turns"`
	ByModel  map[string]Count `json:"by_model,omitempty"`
}

// FromClaude reads a Claude Code result line's token fields. usage is the turn's own (the CLI resets it per result line); perModel is model_usage_since_last_result, the same turn split by model, and modelUsage the process's running totals by model, used only for the model's name when the per-turn split is absent. A turn's totals are the per-model sums when the CLI split them, since those include subagents, and usage otherwise. Nil when the line carries no counts.
func FromClaude(usage, perModel, modelUsage json.RawMessage, numTurns int) *Usage {
	var u struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
	}
	haveUsage := json.Unmarshal(usage, &u) == nil && len(usage) > 0 && string(usage) != "null"
	type model struct {
		Input         int64   `json:"inputTokens"`
		Output        int64   `json:"outputTokens"`
		CacheRead     int64   `json:"cacheReadInputTokens"`
		CacheCreation int64   `json:"cacheCreationInputTokens"`
		CostUSD       float64 `json:"costUSD"`
	}
	var split map[string]model
	if json.Unmarshal(perModel, &split) != nil || len(split) == 0 {
		split = nil
	}
	if !haveUsage && split == nil {
		return nil
	}
	out := &Usage{NumTurns: numTurns}
	if split != nil {
		out.ByModel = map[string]Count{}
		var best float64
		for _, name := range sortedKeys(split) {
			m := split[name]
			c := Count{Input: m.Input, Output: m.Output, CacheRead: m.CacheRead, CacheCreation: m.CacheCreation}
			out.ByModel[name] = c
			out.Add(c)
			if out.Model == "" || m.CostUSD > best {
				out.Model, best = name, m.CostUSD
			}
		}
		return out
	}
	out.Count = Count{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation}
	var all map[string]model
	if json.Unmarshal(modelUsage, &all) == nil && len(all) > 0 {
		var best float64
		for _, name := range sortedKeys(all) {
			if out.Model == "" || all[name].CostUSD > best {
				out.Model, best = name, all[name].CostUSD
			}
		}
	}
	if out.Model != "" {
		out.ByModel = map[string]Count{out.Model: out.Count}
	}
	return out
}

// FromAnthropic reads a Messages API response's usage: one call, one turn.
func FromAnthropic(usage json.RawMessage, model string) *Usage {
	var u struct {
		Input         int64 `json:"input_tokens"`
		Output        int64 `json:"output_tokens"`
		CacheRead     int64 `json:"cache_read_input_tokens"`
		CacheCreation int64 `json:"cache_creation_input_tokens"`
	}
	if len(usage) == 0 || json.Unmarshal(usage, &u) != nil {
		return nil
	}
	c := Count{Input: u.Input, Output: u.Output, CacheRead: u.CacheRead, CacheCreation: u.CacheCreation}
	out := &Usage{Count: c, Model: model, NumTurns: 1}
	if model != "" {
		out.ByModel = map[string]Count{model: c}
	}
	return out
}

// FromCodex reads Codex's turn.completed usage. Codex counts cached tokens inside input_tokens, so the uncached input is the difference; OpenAI's cache has no write, so CacheCreation is 0.
func FromCodex(usage json.RawMessage, model string) *Usage {
	var u struct {
		Input  int64 `json:"input_tokens"`
		Cached int64 `json:"cached_input_tokens"`
		Output int64 `json:"output_tokens"`
	}
	if len(usage) == 0 || json.Unmarshal(usage, &u) != nil {
		return nil
	}
	c := Count{Input: max(u.Input-u.Cached, 0), Output: u.Output, CacheRead: u.Cached}
	out := &Usage{Count: c, Model: model, NumTurns: 1}
	if model != "" {
		out.ByModel = map[string]Count{model: c}
	}
	return out
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
