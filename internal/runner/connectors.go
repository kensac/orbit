package runner

import (
	"context"
	"encoding/json"
	"strings"

	"github.com/kensac/orbit/internal/claudeproto"
)

// Connectors are MCP servers a claude-code run reaches through orbit (docs/ARCHITECTURE.md, Connectors). Each is declared to the CLI as an SDK MCP server ({"type": "sdk", "name": <connector>} in --mcp-config), so its JSON-RPC arrives at orbit's process as mcp_message control requests and orbit, not the CLI, talks to the real server with the credential; the CLI's environment and a sandbox never see one. Verified against the real CLI 2.1.316: several SDK servers in one --mcp-config, each listed and called as mcp__<name>__<tool>; tools/call carries _meta["claudecode/toolUseId"], the id the permission prompt names; an ask rule on an SDK tool reaches the stdio permission prompt in the default and bypassPermissions modes (dontAsk denies it unasked, so a run with such a tool runs in default mode).
type Connectors struct {
	Servers []ConnectorServer
	// Call runs one tools/call; toolUseID is the CLI's id for the call, empty when the CLI sent none.
	Call func(ctx context.Context, server, tool, toolUseID string, args json.RawMessage) ConnectorResult
	// Permit decides a call to an Ask tool when the CLI asks; it may wait for a person.
	Permit func(ctx context.Context, server, tool, toolUseID string, args json.RawMessage) claudeproto.Permission
	// Record stores a permission decision before it is answered (claudeproto.Recorded); every prompt the CLI sends is recorded, connector or not.
	Record func(req claudeproto.PermissionRequest, p claudeproto.Permission) error
}

// ConnectorServer is one connector and the tools it offers this run.
type ConnectorServer struct {
	Name  string
	Tools []ConnectorTool
}

// ConnectorTool is one tool offered. Ask sends each call to Permit first; any other tool runs when called.
type ConnectorTool struct {
	Name        string
	Description string
	InputSchema json.RawMessage
	ReadOnly    bool
	Ask         bool
}

// ConnectorResult is what a call gives the model. File is a workdir path the result was written to instead (a large one); a sandboxed run gets it copied in before the answer.
type ConnectorResult struct {
	Content json.RawMessage
	IsError bool
	File    string
}

// ConnectorToolName is a connector tool's name as the model sees it.
func ConnectorToolName(server, tool string) string { return "mcp__" + server + "__" + tool }

func (cs *Connectors) names(ask bool) []string {
	if cs == nil {
		return nil
	}
	var out []string
	for _, s := range cs.Servers {
		for _, t := range s.Tools {
			if t.Ask == ask {
				out = append(out, ConnectorToolName(s.Name, t.Name))
			}
		}
	}
	return out
}

// Allowed are the tools that run when called, for --allowedTools.
func (cs *Connectors) Allowed() []string { return cs.names(false) }

// Ask are the tools whose calls go to the permission prompt, as the settings' ask rules.
func (cs *Connectors) Ask() []string { return cs.names(true) }

// addServers declares each connector in an --mcp-config's servers.
func (cs *Connectors) addServers(servers map[string]any) {
	if cs == nil {
		return
	}
	for _, s := range cs.Servers {
		servers[s.Name] = map[string]any{"type": "sdk", "name": s.Name}
	}
}

func (cs *Connectors) find(server, tool string) (ConnectorTool, bool) {
	if cs == nil {
		return ConnectorTool{}, false
	}
	for _, s := range cs.Servers {
		if s.Name != server {
			continue
		}
		for _, t := range s.Tools {
			if t.Name == tool {
				return t, true
			}
		}
	}
	return ConnectorTool{}, false
}

// splitToolName reads mcp__<server>__<tool>; server names have no "__" (spec names are [a-z0-9-]).
func splitToolName(name string) (server, tool string, ok bool) {
	rest, ok := strings.CutPrefix(name, "mcp__")
	if !ok {
		return "", "", false
	}
	server, tool, ok = strings.Cut(rest, "__")
	return server, tool, ok && server != "" && tool != ""
}

// permission answers one can_use_tool: a connector tool that asks is Permit's to decide; anything else the CLI asks about is denied, as dontAsk would.
func (cs *Connectors) permission(ctx context.Context, req claudeproto.PermissionRequest) (claudeproto.Permission, error) {
	decide := func(ctx context.Context, r claudeproto.PermissionRequest) claudeproto.Permission {
		server, tool, ok := splitToolName(r.Tool)
		if !ok {
			return claudeproto.Permission{Message: "orbit allows only the tools this agent is configured with"}
		}
		t, ok := cs.find(server, tool)
		if !ok || !t.Ask || cs.Permit == nil {
			return claudeproto.Permission{Message: "orbit allows only the tools this agent is configured with"}
		}
		return cs.Permit(ctx, server, tool, r.ToolUseID, r.Input)
	}
	var record func(claudeproto.PermissionRequest, claudeproto.Permission) error
	if cs != nil {
		record = cs.Record
	}
	return claudeproto.Recorded(decide, record)(ctx, req)
}

// serve answers one JSON-RPC message for connector server, as an MCP server would.
func (cs *Connectors) serve(ctx context.Context, server string, msg json.RawMessage, push func(context.Context, []string) error) (json.RawMessage, error) {
	var m struct {
		ID     json.RawMessage `json:"id"`
		Method string          `json:"method"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(msg, &m); err != nil {
		return nil, err
	}
	reply := func(result any) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID, "result": result})
	}
	if len(m.ID) == 0 {
		return json.RawMessage(`{"jsonrpc":"2.0","result":{}}`), nil
	}
	switch m.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(m.Params, &p)
		return reply(map[string]any{"protocolVersion": p.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
			"serverInfo": map[string]any{"name": server, "version": "1"}})
	case "ping":
		return reply(map[string]any{})
	case "tools/list":
		tools := []map[string]any{}
		if cs != nil {
			for _, s := range cs.Servers {
				if s.Name != server {
					continue
				}
				for _, t := range s.Tools {
					tt := map[string]any{"name": t.Name, "description": t.Description, "inputSchema": t.InputSchema}
					if t.ReadOnly {
						tt["annotations"] = map[string]any{"readOnlyHint": true}
					}
					tools = append(tools, tt)
				}
			}
		}
		return reply(map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ToolUseID string `json:"claudecode/toolUseId"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(m.Params, &p); err != nil {
			return nil, err
		}
		if cs.Call == nil {
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "connectors are not available between turns"}}, "isError": true})
		}
		if _, ok := cs.find(server, p.Name); !ok {
			return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "this tool is not offered to this run"}}, "isError": true})
		}
		res := cs.Call(ctx, server, p.Name, p.Meta.ToolUseID, p.Arguments)
		if res.File != "" && push != nil {
			if err := push(ctx, []string{res.File}); err != nil {
				return reply(map[string]any{"content": []any{map[string]any{"type": "text", "text": "orbit could not copy the result into the sandbox: " + err.Error()}}, "isError": true})
			}
		}
		content := res.Content
		if len(content) == 0 {
			content = json.RawMessage(`[]`)
		}
		return reply(map[string]any{"content": content, "isError": res.IsError})
	}
	return json.Marshal(map[string]any{"jsonrpc": "2.0", "id": m.ID,
		"error": map[string]any{"code": -32601, "message": "method not found: " + m.Method}})
}
