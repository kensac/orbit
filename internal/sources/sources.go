// Package sources fetches an agent's inputs before its run. The harness holds
// every credential a source needs; the model only ever sees the fetched
// bytes, written to a file in the run's workdir.
package sources

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/github"
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

// Vars are what a source's {{...}} templates are filled from: the run's
// time, in the agent's timezone, and its trigger (spec.Source.ByTrigger).
type Vars struct {
	Now time.Time
	// Trigger is the run's (schedule, manual, message, ...).
	Trigger string
}

// Template bounds: ten years back, beyond any history a source here keeps,
// and small enough that a typo cannot ask for year 1.
const (
	maxDaysAgo   = 3660
	maxMonthsAgo = 120
)

// templateRE is one template: a name, optionally followed by a count.
var templateRE = regexp.MustCompile(`\{\{([^{}]*)\}\}`)

// Expand fills s's templates, all dates as YYYY-MM-DD in Now's zone:
//
//   - {{now}} (RFC 3339), {{today}}, {{month_start}}, {{prev_month_start}},
//     {{prev_month_end}}, {{days_ago_N}} (N in 1..366);
//   - {{days_ago N}}: N days before today, N in 0..maxDaysAgo;
//   - {{months_ago_start N}} and {{months_ago_end N}}: the first and last
//     day of the month N months before this one, N in 0..maxMonthsAgo
//     ({{months_ago_start 1}} is {{prev_month_start}}).
//
// Anything else in {{...}}, or an unclosed {{, is an error naming it.
func (v Vars) Expand(s string) (string, error) {
	var bad error
	out := templateRE.ReplaceAllStringFunc(s, func(m string) string {
		r, err := v.one(strings.TrimSpace(m[2 : len(m)-2]))
		if err != nil && bad == nil {
			bad = fmt.Errorf("template %s in %q: %w", m, s, err)
		}
		return r
	})
	if bad != nil {
		return "", bad
	}
	if i := strings.Index(out, "{{"); i >= 0 {
		return "", fmt.Errorf("unknown template variable in %q", out[i:])
	}
	return out, nil
}

var errUnknownTemplate = errors.New("unknown template variable")

func (v Vars) one(t string) (string, error) {
	d := func(t time.Time) string { return t.Format("2006-01-02") }
	now := v.Now
	ms := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, now.Location())
	name, arg, hasArg := strings.Cut(t, " ")
	if !hasArg {
		switch name {
		case "now":
			return now.Format(time.RFC3339), nil
		case "today":
			return d(now), nil
		case "month_start":
			return d(ms), nil
		case "prev_month_start":
			return d(ms.AddDate(0, -1, 0)), nil
		case "prev_month_end":
			return d(ms.AddDate(0, 0, -1)), nil
		}
		if n, ok := strings.CutPrefix(name, "days_ago_"); ok {
			if k, err := strconv.Atoi(n); err == nil && k >= 1 && k <= 366 && strconv.Itoa(k) == n {
				return d(now.AddDate(0, 0, -k)), nil
			}
		}
		return "", errUnknownTemplate
	}
	limit := map[string]int{"days_ago": maxDaysAgo, "months_ago_start": maxMonthsAgo, "months_ago_end": maxMonthsAgo}[name]
	if limit == 0 {
		return "", errUnknownTemplate
	}
	arg = strings.TrimSpace(arg)
	n, err := strconv.Atoi(arg)
	if err != nil || n < 0 || n > limit || strconv.Itoa(n) != arg {
		return "", fmt.Errorf("%s takes a whole number from 0 to %d", name, limit)
	}
	switch name {
	case "days_ago":
		return d(now.AddDate(0, 0, -n)), nil
	case "months_ago_start":
		return d(ms.AddDate(0, -n, 0)), nil
	}
	return d(ms.AddDate(0, -n+1, -1)), nil
}

// render fills a setting's templates, then its ${VAR}s, adding every credential (spec.Credentials) it expands to used. A restricted fetcher refuses any other variable.
func (f *Fetcher) render(s string, vars Vars, used map[string]bool) (string, error) {
	s, err := vars.Expand(s)
	if err != nil {
		return "", err
	}
	for _, r := range spec.EnvRefs(s) {
		if _, ok := f.Credentials[r]; ok {
			used[r] = true
		} else if f.Restricted {
			return "", fmt.Errorf("${%s} is not in orbit.yaml's credentials", r)
		}
	}
	return spec.ExpandEnv(s, f.Env)
}

// allow refuses a request to u unless every credential in used may be sent there.
func (f *Fetcher) allow(used map[string]bool, u string) error {
	for _, n := range slices.Sorted(maps.Keys(used)) {
		if err := f.Credentials.Check(n, u); err != nil {
			return err
		}
	}
	return nil
}

// Fetcher fetches sources.
type Fetcher struct {
	Client *http.Client
	Env    Env
	// Credentials are orbit.yaml's: a ${VAR} naming one is sent only to URLs under its prefix, redirects included.
	Credentials spec.Credentials
	// Restricted is set for an agent written outside the configuration files (spec.Agent.Restricted): it may read no file and render no variable but a credential.
	Restricted bool
	// GitHub mints a token of perms for repo, one of github.read_repositories (refusing any other), for one github source's fetch; nil fails a github source.
	GitHub func(ctx context.Context, repo string, perms github.Permissions) (*github.Token, error)
	// Now is time.Now, replaceable in tests: a loki query ends at it.
	Now func() time.Time
	// Delivery is the webhook delivery that started the run, the content of its webhook sources; nil for a run no delivery started.
	Delivery *Fetched
}

// ErrAbsent is a webhook source's answer on a run no delivery started: there is nothing to read, which is not a failure.
var ErrAbsent = errors.New("no webhook delivery started this run")

// Fetch fetches one source.
func (f *Fetcher) Fetch(ctx context.Context, s spec.Source, vars Vars) (*Fetched, error) {
	switch s.Kind {
	case "http":
		return f.http(ctx, s, vars)
	case "prometheus":
		return f.prometheus(ctx, s, vars)
	case "loki":
		return f.loki(ctx, s, vars)
	case "github":
		return f.github(ctx, s)
	case "file":
		return f.file(s)
	case "webhook":
		if f.Delivery == nil {
			return nil, ErrAbsent
		}
		if int64(len(f.Delivery.Content)) > s.MaxBytes {
			return nil, fmt.Errorf("the delivery's body is %d bytes, more than max_bytes %d", len(f.Delivery.Content), s.MaxBytes)
		}
		out := *f.Delivery
		out.Name = s.Name
		return &out, nil
	}
	return nil, fmt.Errorf("unknown source kind %q", s.Kind)
}

func (f *Fetcher) file(s spec.Source) (*Fetched, error) {
	if f.Restricted {
		return nil, fmt.Errorf("a file source is only for an agent in the configuration files")
	}
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

func (f *Fetcher) http(ctx context.Context, s spec.Source, vars Vars) (*Fetched, error) {
	used := map[string]bool{}
	base, err := f.render(s.URL, vars, used)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(base)
	if err != nil {
		return nil, err
	}
	q := u.Query()
	for k, v := range s.QueryFor(vars.Trigger) {
		rv, err := f.render(v, vars, used)
		if err != nil {
			return nil, fmt.Errorf("query %s: %w", k, err)
		}
		q.Set(k, rv)
	}
	headers := http.Header{}
	for k, v := range s.Headers {
		rv, err := f.render(v, vars, used)
		if err != nil {
			return nil, fmt.Errorf("header %s: %w", k, err)
		}
		headers.Set(k, rv)
	}
	if s.Paginate == nil {
		u.RawQuery = q.Encode()
		b, ct, err := f.get(ctx, u.String(), headers, s.MaxBytes, used)
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
		b, _, err := f.get(ctx, u.String(), headers, s.MaxBytes-total, used)
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

func (f *Fetcher) prometheus(ctx context.Context, s spec.Source, vars Vars) (*Fetched, error) {
	used := map[string]bool{}
	base, err := f.render(s.URL, vars, used)
	if err != nil {
		return nil, err
	}
	expr, err := f.render(s.Expr, vars, used)
	if err != nil {
		return nil, err
	}
	u, err := url.Parse(strings.TrimRight(base, "/") + "/api/v1/query")
	if err != nil {
		return nil, err
	}
	u.RawQuery = url.Values{"query": {expr}}.Encode()
	b, _, err := f.get(ctx, u.String(), http.Header{}, s.MaxBytes, used)
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

// get fetches u with h, where used are the credentials rendered into either: u and every redirect must be under their prefixes.
func (f *Fetcher) get(ctx context.Context, u string, h http.Header, max int64, used map[string]bool) ([]byte, string, error) {
	if max <= 0 {
		return nil, "", fmt.Errorf("source exceeds max_bytes")
	}
	if err := f.allow(used, u); err != nil {
		return nil, "", err
	}
	client := f.Client
	if len(used) > 0 {
		// Go resends custom headers on a redirect to any host, so a redirect is held to the prefixes too.
		c := *f.Client
		next := c.CheckRedirect
		c.CheckRedirect = func(req *http.Request, via []*http.Request) error {
			if err := f.allow(used, req.URL.String()); err != nil {
				return fmt.Errorf("redirect refused: %w", err)
			}
			if next != nil {
				return next(req, via)
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		}
		client = &c
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
	resp, err := client.Do(req)
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
