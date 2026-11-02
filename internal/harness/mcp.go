package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// MCP tool names as the model sees them: Claude Code prefixes a server's
// tools with mcp__<server>__.
const (
	MCPServer = "orbit"
	ToolQuery = "mcp__orbit__query"
	ToolCalc  = "mcp__orbit__calc"
)

// mcpProtocol is the MCP revision the server speaks when the client asks
// for one it does not know; it answers with the client's own otherwise.
const mcpProtocol = "2025-06-18"

// ServeMCP is a Model Context Protocol server on stdio (newline-delimited
// JSON-RPC 2.0) with two tools: query, over the JSON files under root, and
// calc, exact arithmetic. It returns when in ends.
//
// Each successful query or calc result is also appended to LedgerPath(state),
// the record the figure check reads of what orbit computed. With a search
// socket it also serves search (search.go), whose results are earlier
// writing, not computation, and so never back a figure.
func ServeMCP(in io.Reader, out io.Writer, root, state, search, version string) error {
	sc := bufio.NewScanner(in)
	sc.Buffer(make([]byte, 64<<10), 4<<20)
	enc := json.NewEncoder(out)
	for sc.Scan() {
		line := bytes.TrimSpace(sc.Bytes())
		if len(line) == 0 {
			continue
		}
		var req struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		if err := json.Unmarshal(line, &req); err != nil {
			if err := enc.Encode(rpcError(nil, -32700, "parse error")); err != nil {
				return err
			}
			continue
		}
		if len(req.ID) == 0 {
			continue // a notification: nothing to answer
		}
		resp := handle(req.ID, req.Method, req.Params, root, state, search, version)
		if err := enc.Encode(resp); err != nil {
			return err
		}
	}
	return sc.Err()
}

func rpcError(id json.RawMessage, code int, msg string) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": msg}}
}

func rpcResult(id json.RawMessage, result any) map[string]any {
	return map[string]any{"jsonrpc": "2.0", "id": id, "result": result}
}

func handle(id json.RawMessage, method string, params json.RawMessage, root, state, search, version string) map[string]any {
	switch method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		_ = json.Unmarshal(params, &p)
		pv := p.ProtocolVersion
		if pv == "" {
			pv = mcpProtocol
		}
		return rpcResult(id, map[string]any{
			"protocolVersion": pv,
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": MCPServer, "version": version},
		})
	case "ping":
		return rpcResult(id, map[string]any{})
	case "tools/list":
		tools := []any{
			map[string]any{
				"name": "query",
				"description": "Filter, group and aggregate the records of a JSON input file. Counts and sums are exact " +
					"(numbers are read as written, never rounded). Use it for every count and total you report.",
				"inputSchema": QuerySchema,
			},
			map[string]any{
				"name":        "calc",
				"description": "Exact decimal arithmetic: + - * / and parentheses. Use it for any arithmetic you report.",
				"inputSchema": CalcSchema,
			},
		}
		if search != "" {
			tools = append(tools, map[string]any{
				"name": "search",
				"description": "Full-text search over your own earlier work: your runs' reports and errors, what you wrote " +
					"while working, your tool calls' inputs, your person's messages, your questions and your memory. " +
					"Ranked best first, matches marked **like this**, each with its run and conversation.",
				"inputSchema": SearchSchema,
			})
		}
		return rpcResult(id, map[string]any{"tools": tools})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return rpcError(id, -32602, "invalid params")
		}
		var res map[string]any
		var err error
		if p.Name == "search" && search != "" {
			res, err = callSearch(search, p.Arguments)
		} else {
			res, err = call(p.Name, p.Arguments, root)
		}
		if err != nil {
			// A tool failure is a result the model reads, not a protocol error.
			return rpcResult(id, map[string]any{"isError": true,
				"content": []any{map[string]any{"type": "text", "text": err.Error()}}})
		}
		b, _ := json.Marshal(res)
		if p.Name != "search" {
			ledger(state, p.Name, string(b))
		}
		return rpcResult(id, map[string]any{"content": []any{map[string]any{"type": "text", "text": string(b)}}})
	}
	return rpcError(id, -32601, "method not found: "+method)
}

// ledger appends one result. A failed write loses only the check's
// evidence, which then sends the figure back; it never changes a result.
func ledger(state, tool, result string) {
	if state == "" {
		return
	}
	if err := os.MkdirAll(state, 0o700); err != nil {
		return
	}
	f, err := os.OpenFile(LedgerPath(state), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(map[string]string{"tool": tool, "result": result})
	_, _ = f.Write(append(b, '\n'))
}

func call(name string, args json.RawMessage, root string) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	switch name {
	case "query":
		var q Query
		if err := dec.Decode(&q); err != nil {
			return nil, fmt.Errorf("query: %w", err)
		}
		return RunQuery(root, q)
	case "calc":
		var c struct {
			Expression string `json:"expression"`
		}
		if err := dec.Decode(&c); err != nil {
			return nil, fmt.Errorf("calc: %w", err)
		}
		return Calc(c.Expression)
	}
	return nil, fmt.Errorf("unknown tool %q", name)
}

func callSearch(socket string, args json.RawMessage) (map[string]any, error) {
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	var req SearchRequest
	if err := dec.Decode(&req); err != nil {
		return nil, fmt.Errorf("search: %w", err)
	}
	return Search(socket, req)
}
