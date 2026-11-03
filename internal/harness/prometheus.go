package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
)

// The Prometheus tool: read-only PromQL against the Prometheus an agent's prometheus source names, run by orbit, never by the model. As with the repository tools, the tool server asks the engine over a unix socket it serves for that agent alone (engine/prometheus.go), and the engine reads the source's URL and credentials from the agent's spec in force.

// PrometheusTool is the tool's name, unprefixed.
const PrometheusTool = "prometheus_query"

// ToolPrometheus is the tool as the model sees it.
const ToolPrometheus = "mcp__" + MCPServer + "__" + PrometheusTool

// PrometheusDescription is the tool's description.
const PrometheusDescription = "Run a read-only PromQL query against one of your prometheus sources: an instant query at time, or with start a range query from start to end at step. " +
	"Exact and current, from Prometheus itself; answers {resultType, result} as Prometheus does. Aggregate (sum by, topk, rate) rather than reading raw series."

// PrometheusSchema is the tool's input schema.
var PrometheusSchema = map[string]any{
	"type": "object", "additionalProperties": false, "required": []string{"query"},
	"properties": map[string]any{
		"source": str("the prometheus source's name; may be omitted when you have one"),
		"query":  map[string]any{"type": "string", "minLength": 1, "maxLength": 4000, "description": "PromQL"},
		"time":   str("instant query time: RFC 3339, now, or now-<duration> (now-1h, now-2d); default now"),
		"start":  str("range query start, as time; makes this a range query"),
		"end":    str("range query end, as time; default now"),
		"step":   str("range query resolution: a duration (30s, 5m, 1h); default the range over 250 points"),
	},
}

// Prometheus asks the engine listening on socket to run one query with args as the model wrote them.
func Prometheus(socket string, args json.RawMessage) (string, error) {
	return ask(socket, "the prometheus tool", RepoRequest{Tool: PrometheusTool, Args: args})
}

// ReadPrometheusRequest reads one request line, for the engine's side.
func ReadPrometheusRequest(conn net.Conn) (RepoRequest, error) {
	var req RepoRequest
	line, err := bufio.NewReader(io.LimitReader(conn, 64<<10)).ReadBytes('\n')
	if err != nil {
		return req, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("prometheus tool: %w", err)
	}
	if req.Tool != PrometheusTool {
		return req, fmt.Errorf("unknown tool %q", req.Tool)
	}
	return req, nil
}
