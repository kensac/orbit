package spec

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
	"time"
)

// SelfRepo is orbit's own repository. A merge to it replaces the process
// that made it, so code, not configuration, holds its rules (docs/tickets.md,
// Self-hosting): its merges always wait for a person.
const SelfRepo = "kensac/orbit"

// MaxBuildTimeout bounds a build agent's timeout. A sandbox's processes end
// at sandboxd.Lifetime (one hour) and its workdir is read back afterwards,
// so a build must finish well inside it: ten minutes are left for the clone,
// the readback and the delivery.
const MaxBuildTimeout = 50 * time.Minute

// DefaultMaxDiffLines bounds a build's diff when a repository names no
// limit: a plan a person reviews by reading the PR, not a rewrite.
const DefaultMaxDiffLines = 2000

// maxDiffLinesCap is the most a repository may allow.
const maxDiffLinesCap = 20000

// Build is orbit.yaml's build: section (docs/tickets.md, The build run).
// Without it no ticket is built: approved tickets wait.
type Build struct {
	// Sandboxd is the build sandbox service: "host:port" on the tailnet, or
	// "unix:/path".
	Sandboxd string `yaml:"sandboxd" json:"sandboxd"`
	// TokenEnv names the variable holding sandboxd's bearer token.
	TokenEnv string `yaml:"token_env" json:"token_env"`
	// Agent is the agent whose spec a build runs under: its runner (a
	// claude-code runner: model, max_turns), its timeout, and its rule for
	// github:merge.
	Agent string `yaml:"agent" json:"agent"`
	// Author is the commit identity, "Name <email>": the App's bot user.
	Author string `yaml:"author" json:"author"`
	// Repositories are the repositories orbit builds, each with its tool
	// policy. Each must also be in github.repositories.
	Repositories map[string]BuildRepo `yaml:"repositories" json:"repositories"`
}

var sha256RE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// BuildRepo is one repository's build policy.
type BuildRepo struct {
	// Commands are the argv prefixes a build's Bash calls may start with,
	// one per entry, words separated by spaces ("go test", "make check").
	Commands []string `yaml:"commands" json:"commands"`
	// Protected are paths a build may not change (globs, ** for any
	// depth). .github/** is always protected.
	Protected []string `yaml:"protected,omitempty" json:"protected,omitempty"`
	// MaxDiffLines bounds added plus deleted lines; DefaultMaxDiffLines
	// when zero.
	MaxDiffLines int `yaml:"max_diff_lines,omitempty" json:"max_diff_lines,omitempty"`
	// TrustedScripts are the sha256 (hex) of every setup and cleanup
	// script in the repository's .orbit/build.yaml a person reviewed. A
	// script whose text hashes to anything else is never run, and fails
	// the build (docs/tickets.md, Setup and cleanup scripts).
	TrustedScripts []string `yaml:"trusted_scripts,omitempty" json:"trusted_scripts,omitempty"`
}

// Argv returns the repository's command prefixes as argv slices.
func (r BuildRepo) Argv() [][]string {
	out := make([][]string, 0, len(r.Commands))
	for _, c := range r.Commands {
		out = append(out, strings.Fields(c))
	}
	return out
}

// Builds reports the build policy for repo, if orbit builds it.
func (d Deployment) Builds(repo string) (BuildRepo, bool) {
	if d.Build == nil {
		return BuildRepo{}, false
	}
	r, ok := d.Build.Repositories[repo]
	return r, ok
}

// commandWord is what a command prefix's words may contain: no shell
// syntax, so a configured prefix means exactly the argv it spells.
func commandWord(w string) bool {
	for _, r := range w {
		ok := r == '-' || r == '_' || r == '.' || r == '/' || r == '=' || r == ':' ||
			(r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9')
		if !ok {
			return false
		}
	}
	return w != ""
}

// ValidGlob reports whether p is a path glob BuildRepo.Protected accepts:
// relative, slash-separated, each segment ** or a path.Match pattern.
func ValidGlob(p string) bool {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") {
		return false
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if seg == "**" {
			continue
		}
		if _, err := path.Match(seg, "x"); err != nil {
			return false
		}
	}
	return true
}

func (c *Config) validateBuild() []error {
	b := c.Deployment.Build
	if b == nil {
		return nil
	}
	var errs []error
	at := func(format string, args ...any) { errs = append(errs, fmt.Errorf("build: "+format, args...)) }
	if c.Deployment.GitHub == nil {
		at("needs a github block in orbit.yaml: orbit clones, pushes and opens PRs as the App")
	}
	if b.Sandboxd == "" {
		at("sandboxd is required (host:port on the tailnet, or unix:/path)")
	} else if !strings.HasPrefix(b.Sandboxd, "unix:/") && !strings.Contains(b.Sandboxd, ":") {
		at("sandboxd %q is neither host:port nor unix:/path", b.Sandboxd)
	}
	if b.TokenEnv == "" {
		at("token_env is required")
	}
	if _, _, err := ParseAuthor(b.Author); err != nil {
		at("author %q must be \"Name <email>\" (the App's bot user)", b.Author)
	}
	a, ok := c.Agents[b.Agent]
	switch {
	case b.Agent == "":
		at("agent is required")
	case !ok:
		at("unknown agent %q", b.Agent)
	default:
		if rs := c.Deployment.Runners[a.Runner]; rs.Kind != "claude-code" {
			at("agent %q must use a claude-code runner (runner %q is %q): the sandbox runs Claude Code", b.Agent, a.Runner, rs.Kind)
		}
		if time.Duration(a.Timeout) > MaxBuildTimeout {
			at("agent %q's timeout %s exceeds %s, inside the sandbox's one-hour lifetime", b.Agent, time.Duration(a.Timeout), MaxBuildTimeout)
		}
	}
	if len(b.Repositories) == 0 {
		at("repositories must name at least one repository")
	}
	repos := make([]string, 0, len(b.Repositories))
	for r := range b.Repositories {
		repos = append(repos, r)
	}
	sort.Strings(repos)
	for _, r := range repos {
		br := b.Repositories[r]
		if !c.Deployment.AllowsRepo(r) {
			at("repository %q is not in github.repositories", r)
		}
		// The backend credential is readable inside the sandbox until the
		// credential proxy keeps it out (docs/tickets.md, Open decisions):
		// until then only orbit's own repository, whose every merge a
		// person approves, is built.
		if r != SelfRepo {
			at("repository %q: builds target only %s until the backend credential is kept out of the sandbox", r, SelfRepo)
		}
		if len(br.Commands) == 0 {
			at("repository %q: commands must list at least one command prefix", r)
		}
		for _, cmd := range br.Commands {
			words := strings.Fields(cmd)
			if len(words) == 0 {
				at("repository %q: empty command", r)
				continue
			}
			for _, w := range words {
				if !commandWord(w) {
					at("repository %q: command %q: %q is not a plain word", r, cmd, w)
				}
			}
		}
		for _, p := range br.Protected {
			if !ValidGlob(p) {
				at("repository %q: protected path %q is not a relative glob", r, p)
			}
		}
		for _, h := range br.TrustedScripts {
			if !sha256RE.MatchString(h) {
				at("repository %q: trusted script %q is not a sha256 in lower-case hex", r, h)
			}
		}
		if br.MaxDiffLines < 0 || br.MaxDiffLines > maxDiffLinesCap {
			at("repository %q: max_diff_lines must be between 1 and %d", r, maxDiffLinesCap)
		}
	}
	return errs
}

func (c *Config) applyBuildDefaults() {
	b := c.Deployment.Build
	if b == nil {
		return
	}
	if b.TokenEnv == "" {
		b.TokenEnv = "ORBIT_SANDBOX_TOKEN"
	}
	for name, r := range b.Repositories {
		if r.MaxDiffLines == 0 {
			r.MaxDiffLines = DefaultMaxDiffLines
			b.Repositories[name] = r
		}
	}
}

// authorRE is "Name <email>". net/mail refuses a bot's name ("[bot]" is
// special in RFC 5322), so the commit identity has its own small grammar.
var authorRE = regexp.MustCompile(`^([^<>\n]*[^<>\s])\s*<([^<>\s@]+@[^<>\s@]+)>$`)

// ParseAuthor reads a commit identity, "Name <email>".
func ParseAuthor(s string) (name, email string, err error) {
	m := authorRE.FindStringSubmatch(strings.TrimSpace(s))
	if m == nil {
		return "", "", fmt.Errorf("author %q is not \"Name <email>\"", s)
	}
	return m[1], m[2], nil
}
