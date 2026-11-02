// Package build is the build runner: it turns a ticket's approved plan into
// a pull request (docs/tickets.md, The build run). orbit clones the
// repository into its own work area, hands a copy to a sandbox (sandboxd),
// runs Claude Code inside it and answers every tool permission request
// from here, outside the container, by a fixed policy; then it reads the
// tree back, checks the diff, and commits, pushes and opens the PR itself.
// The model writes code; everything else is this package.
package build

import (
	"encoding/json"
	"fmt"
	"path"
	"strings"

	"github.com/kensac/orbit/internal/sandbox"
)

// AlwaysProtected are paths no build may change in any repository. A
// workflow change would alter what CI runs with the repository's secrets
// (orbit's tokens lack the workflows permission, so GitHub refuses the push
// too); .gitmodules would add a submodule, a tree orbit never reviewed.
var AlwaysProtected = []string{".github/**", ".gitmodules"}

// Policy decides a build CLI's permission requests. It is deterministic
// and runs in orbit's process, which nothing in the sandbox can reach.
type Policy struct {
	// Root is the checkout inside the sandbox (sandbox.WorkDir).
	Root string
	// Protected are globs (relative to Root) that may not be written, on
	// top of AlwaysProtected.
	Protected []string
	// Commands are the argv prefixes a Bash command may start with.
	Commands [][]string
}

// Decision is the policy's answer to one request.
type Decision struct {
	Allow bool
	// Message tells the model why, and what is allowed instead, so it
	// adapts rather than retrying.
	Message string
}

func deny(format string, args ...any) Decision {
	return Decision{Message: fmt.Sprintf(format, args...)}
}

var allow = Decision{Allow: true}

// Decide answers one can_use_tool request. Anything it does not recognize,
// or cannot parse, is denied.
func (p Policy) Decide(tool string, input json.RawMessage) Decision {
	var in struct {
		FilePath string `json:"file_path"`
		Path     string `json:"path"`
		Pattern  string `json:"pattern"`
		Command  string `json:"command"`
	}
	if err := json.Unmarshal(input, &in); err != nil {
		return deny("orbit could not read this request's input")
	}
	switch tool {
	case "Edit", "Write":
		if in.FilePath == "" {
			return deny("%s needs a file_path", tool)
		}
		return p.writable(in.FilePath)
	case "Read":
		if in.FilePath == "" || !p.inside(in.FilePath) {
			return deny("read only files inside %s", p.root())
		}
		return allow
	case "Glob", "Grep":
		if in.Path != "" && !p.inside(in.Path) {
			return deny("search only inside %s", p.root())
		}
		if tool == "Glob" && strings.HasPrefix(in.Pattern, "/") && !p.inside(in.Pattern) {
			return deny("search only inside %s", p.root())
		}
		return allow
	case "Bash":
		return p.bash(in.Command)
	}
	return deny("orbit does not allow the %s tool in a build", tool)
}

func (p Policy) root() string {
	if p.Root == "" {
		return sandbox.WorkDir
	}
	return path.Clean(p.Root)
}

// rel resolves name against the root, lexically: a symlink inside the
// checkout can still point elsewhere, which is why the diff policy checks
// the tree that comes back, not the requests that made it.
func (p Policy) rel(name string) (string, bool) {
	root := p.root()
	if !strings.HasPrefix(name, "/") {
		name = root + "/" + name
	}
	name = path.Clean(name)
	if name == root {
		return ".", true
	}
	r, ok := strings.CutPrefix(name, root+"/")
	return r, ok
}

func (p Policy) inside(name string) bool {
	_, ok := p.rel(name)
	return ok
}

// writable is the rule for every write, by the file tools or a shell
// redirection: inside the root, outside any .git directory, outside the
// protected paths.
func (p Policy) writable(name string) Decision {
	r, ok := p.rel(name)
	if !ok || r == "." {
		return deny("write only files inside %s", p.root())
	}
	for _, seg := range strings.Split(r, "/") {
		if seg == ".git" {
			return deny("%s is inside a .git directory; orbit commits for you, so never write there", r)
		}
	}
	for _, g := range append(append([]string{}, AlwaysProtected...), p.Protected...) {
		if MatchGlob(g, r) {
			return deny("%s is a protected path (%s); this build may not change it", r, g)
		}
	}
	return allow
}

// MatchGlob matches a slash-separated relative path against a glob whose
// segments are path.Match patterns or "**" (any number of segments).
func MatchGlob(glob, name string) bool {
	return matchSegs(strings.Split(glob, "/"), strings.Split(name, "/"))
}

func matchSegs(g, n []string) bool {
	for len(g) > 0 {
		if g[0] == "**" {
			for i := 0; i <= len(n); i++ {
				if matchSegs(g[1:], n[i:]) {
					return true
				}
			}
			return false
		}
		if len(n) == 0 {
			return false
		}
		if ok, err := path.Match(g[0], n[0]); err != nil || !ok {
			return false
		}
		g, n = g[1:], n[1:]
	}
	return len(n) == 0
}

// commandList renders the allowed prefixes for a denial message.
func (p Policy) commandList() string {
	var out []string
	for _, c := range p.Commands {
		out = append(out, "`"+strings.Join(c, " ")+"`")
	}
	return strings.Join(out, ", ")
}
