package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// MaxEvaluationInput bounds one saved case sent to a backend.
const MaxEvaluationInput = 8 << 20

// MaxEvaluationCases and MaxEvaluationBatchInput bound one replay request.
const (
	MaxEvaluationCases      = 32
	MaxEvaluationBatchInput = 32 << 20
)

const maxEvaluationOutput = 512 << 10

// EvaluationCase supplies one named saved input to a replay.
type EvaluationCase struct {
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

// ReplayResult is the unpersisted output and reported backend cost of one evaluation replay.
type ReplayResult struct {
	Name    string          `json:"name"`
	Output  json.RawMessage `json:"output"`
	CostUSD *float64        `json:"cost_usd"`
}

// Replay runs saved inputs through one current agent charter and skill snapshot without recording runs.
// It grants no agent tools, connectors, hooks, sources or actions.
func (e *Engine) Replay(ctx context.Context, name string, cases []EvaluationCase) ([]ReplayResult, error) {
	if err := ValidateEvaluationCases(cases); err != nil {
		return nil, err
	}
	snap := e.Current()
	if snap == nil || snap.Config == nil {
		return nil, errors.New("configuration is unavailable")
	}
	a, ok := snap.Config.Agents[name]
	if !ok || !a.IsEnabled() {
		return nil, fmt.Errorf("agent %q is unavailable", name)
	}
	newRunner := e.NewRunner
	if newRunner == nil {
		newRunner = runner.New
	}
	getenv := e.Env
	if getenv == nil {
		getenv = os.LookupEnv
	}
	r, err := newRunner(a.Runner, snap.Config.Deployment.Runners[a.Runner], getenv)
	if err != nil {
		return nil, err
	}
	skills, err := e.currentSkills(ctx, a)
	if err != nil {
		return nil, err
	}
	results := make([]ReplayResult, 0, len(cases))
	for _, c := range cases {
		result, err := e.replayCase(ctx, snap, a, r, skills, c)
		if err != nil {
			return nil, fmt.Errorf("evaluation case %q: %w", c.Name, err)
		}
		results = append(results, *result)
	}
	return results, nil
}

// ValidateEvaluationCases checks every case before a replay can spend backend time.
func ValidateEvaluationCases(cases []EvaluationCase) error {
	if len(cases) == 0 || len(cases) > MaxEvaluationCases {
		return fmt.Errorf("evaluation must contain 1 to %d cases", MaxEvaluationCases)
	}
	totalInput := 0
	caseNames := make(map[string]bool, len(cases))
	for _, c := range cases {
		if c.Name == "" || len(c.Name) > 256 || caseNames[c.Name] {
			return fmt.Errorf("evaluation case name is empty, oversized or duplicated: %q", c.Name)
		}
		caseNames[c.Name] = true
		if len(c.Input) == 0 || len(c.Input) > MaxEvaluationInput || !json.Valid(c.Input) {
			return fmt.Errorf("evaluation case %q input must be valid JSON no larger than %d bytes", c.Name, MaxEvaluationInput)
		}
		totalInput += len(c.Input)
		if totalInput > MaxEvaluationBatchInput {
			return fmt.Errorf("evaluation inputs exceed %d bytes", MaxEvaluationBatchInput)
		}
	}
	return nil
}

func (e *Engine) replayCase(ctx context.Context, snap *Snapshot, a *spec.Agent, r runner.Runner, skills []store.Skill, c EvaluationCase) (*ReplayResult, error) {
	workdir, err := os.MkdirTemp("", "orbit-eval-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(workdir)
	if err := os.Mkdir(filepath.Join(workdir, "inputs"), 0o700); err != nil {
		return nil, err
	}
	home := filepath.Join(workdir, ".home")
	if err := os.Mkdir(home, 0o700); err != nil {
		return nil, err
	}
	inputFile := filepath.Join("inputs", "reference.json")
	if err := writeFile(filepath.Join(workdir, inputFile), c.Input); err != nil {
		return nil, err
	}
	inputs := []runner.Input{{Name: "reference", Path: inputFile, MediaType: "application/json", Content: c.Input}}
	plugin := ""
	if r.Kind() == "claude-code" {
		plugin, err = writeSkills(home, skills, false)
	} else if len(skills) > 0 {
		const skillPath = "inputs/_skills.md"
		content := skillsInput(skills)
		if err = writeFile(filepath.Join(workdir, skillPath), content); err == nil {
			inputs = append(inputs, runner.Input{Name: "skills", Path: skillPath, MediaType: "text/markdown", Content: content})
		}
	}
	if err != nil {
		return nil, err
	}
	clone := *a
	clone.Notify.When = "never"
	system := systemPrompt(&clone, nil, inputs, r.Kind() != "anthropic", nil, false)
	system += skillsContract(skills, plugin != "", false)
	system += "\nEvaluation runs are isolated. Do not claim to have fetched sources, read memory or prior messages, used tools, or performed actions. Propose no actions.\n"
	rc := &store.RunContext{}
	prompt := userPrompt(&clone, time.Now(), promptRun{Trigger: "manual", Wake: "Evaluation replay: answer using the current charter and supplied saved inputs only."}, rc, nil, inputs)
	schema := OutputSchema(nil, snap.Config.Deployment.Defaults.MemoryMaxBytes, true)
	timeout := time.Duration(a.Timeout)
	if timeout <= 0 {
		timeout = 5 * time.Minute
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	result, err := r.Run(runCtx, runner.Request{Workdir: workdir, System: system, Prompt: prompt,
		Inputs: inputs, OutputSchema: schema, SkillsPlugin: plugin}, func(runner.Event) {})
	if err != nil {
		return nil, err
	}
	if result == nil || len(result.Output) > maxEvaluationOutput || !json.Valid(result.Output) {
		return nil, errors.New("evaluation backend returned invalid or oversized JSON output")
	}
	if _, err := validate(schema, result.Output); err != nil {
		return nil, fmt.Errorf("evaluation backend output does not match the contract: %w", err)
	}
	if result.CostUSD != nil && (math.IsNaN(*result.CostUSD) || math.IsInf(*result.CostUSD, 0) || *result.CostUSD < 0) {
		return nil, errors.New("evaluation backend returned an invalid cost")
	}
	return &ReplayResult{Name: c.Name, Output: result.Output, CostUSD: result.CostUSD}, nil
}

func (e *Engine) currentSkills(ctx context.Context, a *spec.Agent) ([]store.Skill, error) {
	if e.Store == nil {
		if len(a.Skills) > 0 {
			return nil, errors.New("current skills require the store")
		}
		return nil, nil
	}
	names, err := e.agentSkills(ctx, a)
	if err != nil {
		return nil, err
	}
	return e.Store.SkillsByName(ctx, names)
}
