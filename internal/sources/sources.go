// Package sources fetches an agent's inputs before its run. The harness holds
// every credential a source needs; the model only ever sees the fetched
// bytes, written to a file in the run's workdir.
package sources

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// Fetched is one source's content.
type Fetched struct {
	Name      string
	MediaType string
	Content   []byte
	// Meta records how it was fetched (pages, status), for the transcript.
	Meta map[string]any
}

// Env resolves ${VAR} references in source settings.
type Env func(string) (string, bool)

// Vars are run-time values a source URL or query may reference with
// {{name}}: {{now}}, {{today}}, {{month_start}}, {{prev_month_start}},
// {{prev_month_end}}, {{days_ago_N}} (N in 1..366), all as YYYY-MM-DD in
// the agent's timezone except {{now}} (RFC 3339).
func Vars(now time.Time) map[string]string {
	d := func(t time.Time) string { return t.Format("2006-01-02") }
	ms := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	v := map[string]string{
		"now":              now.Format(time.RFC3339),
		"today":            d(now),
		"month_start":      d(ms),
		"prev_month_start": d(ms.AddDate(0, -1, 0)),
		"prev_month_end":   d(ms.AddDate(0, 0, -1)),
	}
	for n := 1; n <= 366; n++ {
		v["days_ago_"+strconv.Itoa(n)] = d(now.AddDate(0, 0, -n))
	}
	return v
}

func render(s string, vars map[string]string, env Env) (string, error) {
	for k, v := range vars {
		s = strings.ReplaceAll(s, "{{"+k+"}}", v)
	}
	if i := strings.Index(s, "{{"); i >= 0 {
		return "", fmt.Errorf("unknown template variable in %q", s[i:])
	}
	return spec.ExpandEnv(s, env)
}

// Fetcher fetches sources.
type Fetcher struct {
	Client *http.Client
	Env    Env
}

// Fetch fetches one source.
func (f *Fetcher) Fetch(ctx context.Context, s spec.Source, vars map[string]string) (*Fetched, error) {
	switch s.Kind {
	case "http":
		return f.http(ctx, s, vars)
	case "prometheus":
		return f.prometheus(ctx, s, vars)
	case "file":
		return f.file(s)
	}
	return nil, fmt.Errorf("unknown source kind %q", s.Kind)
}

func (f *Fetcher) file(s spec.Source) (*Fetched, error) {
	fh, err := os.Open(s.Path)
	if err != nil {
		return nil, err
	}
	defer fh.Close()
	b, err := readBounded(fh, s.MaxBytes)
	if err != nil {
		return nil, err
	}
	return &Fetched{Name: s.Name, MediaType: mediaTypeFor(s.Path, b), Content: b, Meta: map[string]any{"path": s.Path}}, nil
}

func (f *Fetcher) http(ctx context.Context, s spec.Source, vars map[string]string) (*Fetched, error) {
	base, err := render(s.URL, vars, f.Env)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range s.Query {
		rv, err := render(v, vars, f.Env)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", k, err)
		}
		q.Set(k, rv)
	}
	headers := http.Header{}
	for k, v := range s.Headers {
		rv, err := render(v, vars, f.Env)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		headers.Set(k, rv)
	}
	if s.Paginate == nil {
		u.RawQuery = q.Encode()
		b, ct, err := f.get(ctx, u.String(), headers, s.MaxBytes)
		if err != nil {
			return nil, err
		}
		return &Fetched{Name: s.Name, MediaType: ct, Content: b, Meta: map[string]any{"url": redact(u)}}, nil
	}

	p := s.Paginate
	maxPages := p.MaxPages
	if maxPages == 0 {
		// A guard against a total_pages field that never stops the loop,
		// not a cap on data: 1000 pages of 100 is far past any source here.
		maxPages = 1000
	}
	var items []json.RawMessage
	var total int64
	pages := 0
	for page := 1; ; page++ {
		if page > maxPages {
			return nil, fmt.Errorf("source %s: more than %d pages; raise paginate.max_pages", s.Name, maxPages)
		}
		q.Set(p.PageParam, strconv.Itoa(page))
		u.RawQuery = q.Encode()
		b, _, err := f.get(ctx, u.String(), headers, s.MaxBytes-total)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		total += int64(len(b))
		pages++
		var doc any
		if err := json.Unmarshal(b, &doc); err != nil {
			return nil, fmt.Errorf("page %d: not JSON: %w", page, err)
		}
		raw, err := lookup(doc, p.ItemsPath)
		if err != nil {
			return nil, fmt.Errorf("page %d: %w", page, err)
		}
		arr, ok := raw.([]any)
		if !ok {
			return nil, fmt.Errorf("page %d: %s is not an array", page, p.ItemsPath)
		}
		for _, it := range arr {
			ib, _ := json.Marshal(it)
			items = append(items, ib)
		}
		if len(arr) == 0 {
			break
		}
		if p.TotalPagesPath != "" {
			tp, err := lookup(doc, p.TotalPagesPath)
			if err != nil {
				return nil, fmt.Errorf("page %d: %w", page, err)
			}
			n, ok := tp.(float64)
			if !ok {
				return nil, fmt.Errorf("page %d: %s is not a number", page, p.TotalPagesPath)
			}
			if page >= int(n) {
				break
			}
		}
	}
	if items == nil {
		items = []json.RawMessage{}
	}
	out, err := json.Marshal(items)
	if err != nil {
		return nil, err
	}
	q.Del(p.PageParam)
	u.RawQuery = q.Encode()
	return &Fetched{Name: s.Name, MediaType: "application/json", Content: out,
		Meta: map[string]any{"url": redact(u), "pages": pages, "items": len(items)}}, nil
}

func (f *Fetcher) prometheus(ctx context.Context, s spec.Source, vars map[string]string) (*Fetched, error) {
	base, err := render(s.URL, vars, f.Env)
	if err != nil {
		return nil, err
	}
	expr, err := render(s.Expr, vars, f.Env)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + "/api/v1/query")
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"query": {expr}}.Encode()
	b, _, err := f.get(ctx, u.String(), http.Header{}, s.MaxBytes)
	if err != nil {
		return nil, err
	}
	var r struct {
		Status string          `json:"status"`
		Error  string          `json:"error"`
		Data   json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("prometheus: %w", err)
	}
	if r.Status != "success" {
		return nil, fmt.Errorf("prometheus: %s", r.Error)
	}
	return &Fetched{Name: s.Name, MediaType: "application/json", Content: r.Data, Meta: map[string]any{"expr": expr}}, nil
}

func (f *Fetcher) get(ctx context.Context, u string, h http.Header, max int64) ([]byte, string, error) {
	if max <= 0 {
		return nil, "", fmt.Errorf("source exceeds max_bytes")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, "", err
	}
	req.Header = h.Clone()
	if req.Header.Get("User-Agent") == "" {
		// Not Go's default: CrowdSec and Cloudflare block library user
		// agents with a 403 that reads like an auth failure.
		req.Header.Set("User-Agent", "orbit/1 (+https://github.com/kensac/orbit)")
	}
	resp, err := f.Client.Do(req)
	if err != nil {
		return nil, "", err
	}
	defer resp.Body.Close()
	b, err := readBounded(resp.Body, max)
	if err != nil {
		return nil, "", err
	}
	if resp.StatusCode/100 != 2 {
		return nil, "", fmt.Errorf("GET %s: %s: %s", redactString(u), resp.Status, firstLine(string(b)))
	}
	ct := resp.Header.Get("Content-Type")
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = ct[:i]
	}
	if ct == "" {
		ct = mediaTypeFor("", b)
	}
	return b, ct, nil
}

func readBounded(r io.Reader, max int64) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("source exceeds max_bytes (%d)", max)
	}
	return b, nil
}

// lookup walks a dotted path ("pagination.total_pages") through decoded JSON.
func lookup(doc any, path string) (any, error) {
	cur := doc
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: %q is not an object", path, part)
		}
		if cur, ok = m[part]; !ok {
			return nil, fmt.Errorf("%s: missing %q", path, part)
		}
	}
	return cur, nil
}

func mediaTypeFor(path string, b []byte) string {
	switch {
	case strings.HasSuffix(path, ".json") || json.Valid(b):
		return "application/json"
	case strings.HasSuffix(path, ".md"):
		return "text/markdown"
	}
	return "text/plain"
}

// redact drops query values that look like credentials from a URL kept in
// the transcript.
func redact(u *url.URL) string {
	c := *u
	q := c.Query()
	for k := range q {
		lk := strings.ToLower(k)
		if strings.Contains(lk, "key") || strings.Contains(lk, "token") || strings.Contains(lk, "secret") || strings.Contains(lk, "password") {
			q.Set(k, "REDACTED")
		}
	}
	c.RawQuery = q.Encode()
	c.User = nil
	return c.String()
}

func redactString(s string) string {
	u, err := url.Parse(s)
	if err != nil {
		return "(unparseable url)"
	}
	return redact(u)
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 300 {
		s = s[:300]
	}
	return s
}
