package runner

import (
	"encoding/json"
	"path/filepath"
	"strings"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/tokens"
)

// PrefixOf is the stable prefix req sends on a backend of kind with model.
// The CLI writes its working directory into its own system prompt, so a
// workdir that changes between runs is a part of its own.
func PrefixOf(kind, model, charter string, req Request) tokens.Prefix {
	tools := append([]string{}, req.Tools...)
	if req.SkillsPlugin != "" {
		tools = append(tools, "Skill")
	}
	var mcp []any
	if req.Harness != nil && req.Chat == nil {
		names := []string{harness.ToolQuery, harness.ToolCalc}
		if req.Harness.Search != "" {
			names = append(names, harness.ToolSearch)
		}
		if req.SkillsPlugin != "" {
			names = append(names, harness.ToolSkill)
		}
		mcp = append(mcp, names)
	}
	workdir := req.Workdir
	if req.Chat != nil {
		for _, t := range req.Chat.Tools {
			mcp = append(mcp, map[string]any{"name": t.Name, "description": t.Description, "input_schema": t.InputSchema})
		}
		if req.Chat.Sandbox != nil {
			workdir = sandbox.WorkDir
		}
	}
	if kind != "claude-code" {
		workdir = ""
	}
	mcpJSON, _ := json.Marshal(mcp)
	toolsJSON, _ := json.Marshal(tools)
	values := map[string]string{
		"model":   kind + "/" + model,
		"tools":   string(toolsJSON),
		"mcp":     string(mcpJSON),
		"schema":  string(req.OutputSchema),
		"skills":  strings.TrimPrefix(filepath.Base(req.SkillsPlugin), "."),
		"system":  req.System,
		"workdir": workdir,
		"charter": strings.TrimSpace(charter),
	}
	return tokens.NewPrefix(values)
}
