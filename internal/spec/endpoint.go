package spec

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"net/url"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// An endpoint's URL is configuration; with a path template, the action fills
// its {name} placeholders from params the model supplies, each checked
// against its declared type, pattern or enum before the action is stored and
// again before it is sent, and escaped as one path segment. So a model can
// say which transaction to change, never which host or route.

// EndpointParam is one placeholder of an endpoint's path.
type EndpointParam struct {
	// Type is string or integer.
	Type string `yaml:"type" json:"type"`
	// Pattern is a regular expression (RE2) the whole value must match.
	Pattern string `yaml:"pattern,omitempty" json:"pattern,omitempty"`
	// Enum lists the only values allowed.
	Enum []string `yaml:"enum,omitempty" json:"enum,omitempty"`
	// MaxLength bounds a string's length.
	MaxLength int `yaml:"max_length,omitempty" json:"max_length,omitempty"`
	// Description tells the model what the value is.
	Description string `yaml:"description,omitempty" json:"description,omitempty"`
}

// MaxEndpointParamLength is a string param's length when its max_length is unset, and the most it may be.
const MaxEndpointParamLength = 200

var pathParamRE = regexp.MustCompile(`\{([a-z_][a-z0-9_]*)\}`)

// PathParams lists the placeholders of the endpoint's path, in order and without repeats.
func (ep Endpoint) PathParams() []string {
	var out []string
	for _, m := range pathParamRE.FindAllStringSubmatch(ep.Path, -1) {
		if !slices.Contains(out, m[1]) {
			out = append(out, m[1])
		}
	}
	return out
}

func (p EndpointParam) maxLength() int {
	if p.MaxLength == 0 {
		return MaxEndpointParamLength
	}
	return p.MaxLength
}

// JSONSchema is the param as the model is shown it.
func (p EndpointParam) JSONSchema() map[string]any {
	s := map[string]any{"type": p.Type}
	if p.Description != "" {
		s["description"] = p.Description
	}
	if p.Type == "string" {
		if p.Pattern != "" {
			s["pattern"] = "^(?:" + p.Pattern + ")$"
		}
		if len(p.Enum) > 0 {
			s["enum"] = p.Enum
		}
		s["minLength"], s["maxLength"] = 1, p.maxLength()
	} else {
		s["minimum"] = 0
	}
	return s
}

// check returns the param's value as it goes into the path, unescaped.
func (p EndpointParam) check(name string, raw json.RawMessage) (string, error) {
	var v string
	switch p.Type {
	case "integer":
		t := bytes.TrimSpace(raw)
		if len(t) == 0 || (t[0] != '-' && (t[0] < '0' || t[0] > '9')) {
			return "", fmt.Errorf("path.%s: must be an integer", name)
		}
		i, err := strconv.ParseInt(string(t), 10, 64)
		if err != nil || i < 0 {
			return "", fmt.Errorf("path.%s: must be a non-negative integer", name)
		}
		v = strconv.FormatInt(i, 10)
	default:
		if err := json.Unmarshal(raw, &v); err != nil {
			return "", fmt.Errorf("path.%s: must be a string", name)
		}
		if v == "" || len(v) > p.maxLength() {
			return "", fmt.Errorf("path.%s: must be 1 to %d characters", name, p.maxLength())
		}
		if v == "." || v == ".." {
			return "", fmt.Errorf("path.%s: may not be a dot segment", name)
		}
		if len(p.Enum) > 0 && !slices.Contains(p.Enum, v) {
			return "", fmt.Errorf("path.%s: must be one of %s", name, strings.Join(p.Enum, ", "))
		}
		if p.Pattern != "" && !regexp.MustCompile("^(?:"+p.Pattern+")$").MatchString(v) {
			return "", fmt.Errorf("path.%s: does not match %s", name, p.Pattern)
		}
	}
	return v, nil
}

// RenderURL is the URL an action with these path params is sent to: url, then path with each placeholder replaced by its checked, escaped value. An endpoint without a path takes no params.
func (ep Endpoint) RenderURL(params map[string]json.RawMessage) (string, error) {
	names := ep.PathParams()
	for k := range params {
		if !slices.Contains(names, k) {
			return "", fmt.Errorf("path.%s: not a parameter of this endpoint", k)
		}
	}
	values := map[string]string{}
	for _, n := range names {
		raw, ok := params[n]
		if !ok {
			return "", fmt.Errorf("path.%s is required", n)
		}
		v, err := ep.Params[n].check(n, raw)
		if err != nil {
			return "", err
		}
		values[n] = url.PathEscape(v)
	}
	p := pathParamRE.ReplaceAllStringFunc(ep.Path, func(m string) string { return values[m[1:len(m)-1]] })
	if ep.Path == "" {
		return ep.URL, nil
	}
	return strings.TrimRight(ep.URL, "/") + p, nil
}

// CompileBodySchema compiles the endpoint's body_schema; nil when it has none.
func (ep Endpoint) CompileBodySchema() (*jsonschema.Schema, error) {
	if ep.BodySchema == nil {
		return nil, nil
	}
	b, err := json.Marshal(ep.BodySchema)
	if err != nil {
		return nil, err
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(b))
	if err != nil {
		return nil, err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("body.json", doc); err != nil {
		return nil, err
	}
	return c.Compile("body.json")
}

// EnvRefs lists the variables the endpoint's headers render, sorted and without repeats.
func (ep Endpoint) EnvRefs() []string {
	var out []string
	for _, v := range ep.Headers {
		for _, r := range EnvRefs(v) {
			if !slices.Contains(out, r) {
				out = append(out, r)
			}
		}
	}
	sort.Strings(out)
	return out
}

func (c *Config) validateEndpoint(name string, ep Endpoint) []error {
	var errs []error
	at := func(format string, args ...any) {
		errs = append(errs, fmt.Errorf("endpoint %q: "+format, append([]any{name}, args...)...))
	}
	if !nameRE.MatchString(name) {
		at("invalid name")
	}
	u, err := url.Parse(ep.URL)
	if ep.URL == "" {
		at("url is required")
	} else if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		at("url %q must be an absolute http(s) URL", ep.URL)
	}
	if ep.Method != "" && !slices.Contains([]string{"POST", "PUT", "PATCH"}, ep.Method) {
		at("method must be POST, PUT or PATCH")
	}
	names := ep.PathParams()
	if ep.Path != "" {
		if err == nil && (u.RawQuery != "" || u.Fragment != "") {
			at("url may have no query or fragment when path is set")
		}
		if !strings.HasPrefix(ep.Path, "/") || strings.ContainsAny(ep.Path, "?#%\\ ") {
			at("path %q must start with / and have no query, fragment, escape, backslash or space", ep.Path)
		}
		rest := pathParamRE.ReplaceAllString(ep.Path, "x")
		if strings.ContainsAny(rest, "{}") {
			at("path %q: a placeholder is {name}, lower case letters, digits and _", ep.Path)
		}
		for _, seg := range strings.Split(ep.Path, "/") {
			if seg == "." || seg == ".." {
				at("path %q has a dot segment", ep.Path)
			}
		}
	}
	for _, n := range names {
		if _, ok := ep.Params[n]; !ok {
			at("path placeholder {%s} has no entry in params", n)
		}
	}
	for _, n := range slices.Sorted(maps.Keys(ep.Params)) {
		p := ep.Params[n]
		if !slices.Contains(names, n) {
			at("params.%s is not a placeholder of path", n)
		}
		switch p.Type {
		case "integer":
			if p.Pattern != "" || len(p.Enum) > 0 || p.MaxLength != 0 {
				at("params.%s: an integer takes no pattern, enum or max_length", n)
			}
		case "string":
			if p.Pattern == "" && len(p.Enum) == 0 {
				// An unchecked string could be any value the model chose; the pattern or enum is what makes the path the configuration's.
				at("params.%s: a string needs a pattern or an enum", n)
			}
			if p.Pattern != "" {
				if _, err := regexp.Compile("^(?:" + p.Pattern + ")$"); err != nil {
					at("params.%s: pattern: %v", n, err)
				}
			}
			if p.MaxLength < 0 || p.MaxLength > MaxEndpointParamLength {
				at("params.%s: max_length is 1 to %d", n, MaxEndpointParamLength)
			}
		default:
			at("params.%s: type must be string or integer", n)
		}
	}
	if _, err := ep.CompileBodySchema(); err != nil {
		at("body_schema: %v", err)
	}
	if len(names) == 0 && err == nil && ep.URL != "" {
		// The whole URL is known now, so a credential bound elsewhere fails the load; with placeholders it is checked on every send.
		full, _ := ep.RenderURL(nil)
		for _, r := range ep.EnvRefs() {
			if err := c.Deployment.Credentials.Check(r, full); err != nil {
				at("%v", err)
			}
		}
	}
	return errs
}
