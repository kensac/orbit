package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/store"
)

// The in-run search tool (harness.ToolSearch). The tool server Claude Code
// starts holds no database credential, so it asks this process over a unix
// socket, one per agent: the socket is the scope, and a request can name no
// other agent. A socket is served from the agent's first run here for the
// life of the process, so a live Claude Code process, whose tool server
// outlives any one run, keeps reaching it.

// searchDefault and searchMost are the in-run page: a model reads every
// result, so fewer than the API's.
const (
	searchDefault = 10
	searchMost    = 50
)

// searchSocketPath is per process and agent, short, and outside every workdir: a unix socket path
// is at most 104 bytes on macOS (108 on Linux), and a run's state directory
// under the data directory can be longer than that.
func (e *Engine) searchSocketPath(agent string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d\x00%s\x00%s", os.Getpid(), e.DataDir, agent))
	return filepath.Join(os.TempDir(), "orbit-search-"+hex.EncodeToString(h[:8])+".sock")
}

// searchSocket returns the agent's search socket, serving it on first use.
// It returns "" when it cannot listen, and the run goes without the tool.
func (e *Engine) searchSocket(agent string) string {
	e.searchMu.Lock()
	defer e.searchMu.Unlock()
	if p, ok := e.searchLn[agent]; ok {
		return p
	}
	path := e.searchSocketPath(agent)
	_ = os.Remove(path) // a socket a previous process left
	ln, err := net.Listen("unix", path)
	if err != nil {
		e.Log.Warn("search socket", "agent", agent, "err", err)
		return ""
	}
	if err := os.Chmod(path, 0o600); err != nil {
		_ = ln.Close()
		e.Log.Warn("search socket", "agent", agent, "err", err)
		return ""
	}
	if e.searchLn == nil {
		e.searchLn = map[string]string{}
	}
	e.searchLn[agent] = path
	go e.serveSearch(ln, agent)
	return path
}

func (e *Engine) serveSearch(ln net.Listener, agent string) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				e.Log.Warn("search socket", "agent", agent, "err", err)
			}
			return
		}
		go func() {
			defer func() { _ = conn.Close() }()
			_ = conn.SetDeadline(time.Now().Add(30 * time.Second))
			var resp harness.SearchResponse
			req, err := harness.ReadSearchRequest(conn)
			if err == nil {
				resp.Result, err = e.searchFor(context.Background(), agent, req)
			}
			if err != nil {
				resp.Error = err.Error()
			}
			b, _ := json.Marshal(resp)
			_, _ = conn.Write(append(b, '\n'))
		}()
	}
}

// searchFor runs req within agent's records and shapes the page for a
// model: each match's snippet with the matched words in **bold**.
func (e *Engine) searchFor(ctx context.Context, agent string, req harness.SearchRequest) (map[string]any, error) {
	loc := time.UTC
	if a, ok := e.Current().Config.Agents[agent]; ok {
		if l, err := time.LoadLocation(a.Timezone); err == nil {
			loc = l
		}
	}
	q := store.SearchQuery{Q: req.Q, Agent: agent, ConversationID: req.ConversationID, Limit: req.Limit}
	if q.Limit <= 0 {
		q.Limit = searchDefault
	}
	q.Limit = min(q.Limit, searchMost)
	for k := range strings.SplitSeq(req.Kind, ",") {
		if k = strings.TrimSpace(k); k != "" {
			q.Kinds = append(q.Kinds, k)
		}
	}
	var err error
	if q.From, err = store.SearchBound(req.From, loc, false); err != nil {
		return nil, fmt.Errorf("from: %w", err)
	}
	if q.To, err = store.SearchBound(req.To, loc, true); err != nil {
		return nil, fmt.Errorf("to: %w", err)
	}
	page, err := e.Store.Search(ctx, q)
	if err != nil {
		if store.IsBadID(err) {
			return nil, errors.New("conversation_id is not a UUID")
		}
		return nil, err
	}
	results := make([]map[string]any, len(page.Results))
	for i, r := range page.Results {
		var snip strings.Builder
		for _, p := range r.Parts {
			if p.Match {
				snip.WriteString("**" + p.Text + "**")
			} else {
				snip.WriteString(p.Text)
			}
		}
		m := map[string]any{"kind": r.Kind, "at": r.At.In(loc).Format(time.RFC3339), "snippet": snip.String()}
		if r.RunID != nil {
			m["run_id"] = *r.RunID
		}
		if r.ConversationID != nil {
			m["conversation_id"] = *r.ConversationID
		}
		if r.Tool != nil {
			m["tool"] = *r.Tool
		}
		results[i] = m
	}
	return map[string]any{"total": page.Total, "results": results}, nil
}
