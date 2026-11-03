package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/sources"
	"github.com/kensac/orbit/internal/spec"
)

// prometheus_query: an agent with a prometheus source may also query that Prometheus during its run, read-only (instant and range queries, bounded in sources.PromQuery), at the source's URL with the source's credentials. orbit runs the request; the model writes only the PromQL and the times.

// promSources are the agent's prometheus sources.
func promSources(a *spec.Agent) []spec.Source {
	var out []spec.Source
	for _, s := range a.Sources {
		if s.Kind == "prometheus" {
			out = append(out, s)
		}
	}
	return out
}

// promSocket returns the agent's prometheus tool socket, or "" for an agent with no prometheus source.
func (e *Engine) promSocket(a *spec.Agent) string {
	if len(promSources(a)) == 0 {
		return ""
	}
	agent := a.Name
	return e.agentSocket("prometheus", agent, func(conn net.Conn) {
		var resp harness.RepoResponse
		req, err := harness.ReadPrometheusRequest(conn)
		if err == nil {
			resp.Text, err = e.promTool(context.Background(), agent, req.Args)
		}
		if err != nil {
			resp.Error = err.Error()
		}
		b, _ := json.Marshal(resp)
		_, _ = conn.Write(append(b, '\n'))
	})
}

// promChatTools adds prometheus_query to a chat run's own tools.
func (e *Engine) promChatTools(a *spec.Agent, chat *runner.Chat) {
	if len(promSources(a)) == 0 {
		return
	}
	chat.Tools = append(chat.Tools, runner.ChatTool{Name: harness.PrometheusTool, Description: harness.PrometheusDescription, InputSchema: harness.PrometheusSchema})
	inner, agent := chat.Call, a.Name
	chat.Call = func(ctx context.Context, name string, args json.RawMessage) (string, bool) {
		if name != harness.PrometheusTool {
			return inner(ctx, name, args)
		}
		text, err := e.promTool(ctx, agent, args)
		if err != nil {
			return name + ": " + err.Error(), true
		}
		return text, false
	}
}

type promArgs struct {
	Source string `json:"source"`
	Query  string `json:"query"`
	Time   string `json:"time"`
	Start  string `json:"start"`
	End    string `json:"end"`
	Step   string `json:"step"`
}

// promTool runs one prometheus_query for agent against a prometheus source of its spec in force.
func (e *Engine) promTool(ctx context.Context, agent string, raw json.RawMessage) (string, error) {
	snap := e.Current()
	a, ok := snap.Config.Agents[agent]
	if !ok {
		return "", fmt.Errorf("agent %s is not configured", agent)
	}
	var args promArgs
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&args); err != nil {
		return "", fmt.Errorf("arguments: %w", err)
	}
	srcs := promSources(a)
	var src *spec.Source
	for i := range srcs {
		if srcs[i].Name == args.Source || (args.Source == "" && len(srcs) == 1) {
			src = &srcs[i]
		}
	}
	if src == nil {
		names := make([]string, len(srcs))
		for i, s := range srcs {
			names[i] = s.Name
		}
		return "", fmt.Errorf("source %q is not one of your prometheus sources (%s)", args.Source, strings.Join(names, ", "))
	}
	now := time.Now().In(a.Location())
	q := sources.PromQuery{Expr: args.Query}
	var err error
	if args.Start != "" {
		if args.Time != "" {
			return "", fmt.Errorf("time is for an instant query; a range query takes start, end and step")
		}
		if q.Start, err = sources.ParsePromTime(args.Start, now); err != nil {
			return "", fmt.Errorf("start: %w", err)
		}
		if q.End, err = sources.ParsePromTime(args.End, now); err != nil {
			return "", fmt.Errorf("end: %w", err)
		}
		if args.Step != "" {
			if q.Step, err = sources.ParsePromDuration(args.Step); err != nil {
				return "", fmt.Errorf("step: %w", err)
			}
		}
	} else {
		if args.End != "" || args.Step != "" {
			return "", fmt.Errorf("end and step are for a range query, which needs start")
		}
		if args.Time != "" {
			if q.Time, err = sources.ParsePromTime(args.Time, now); err != nil {
				return "", fmt.Errorf("time: %w", err)
			}
		}
	}
	f := &sources.Fetcher{Client: e.Client, Env: e.Env, Credentials: snap.Config.Deployment.Credentials, Restricted: a.Restricted()}
	b, err := f.PromQuery(ctx, *src, sources.Vars{Now: now}, q)
	if err != nil {
		return "", err
	}
	return string(b), nil
}
