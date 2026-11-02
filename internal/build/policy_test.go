package build

import (
	"encoding/json"
	"strings"
	"testing"
)

// orbitPolicy is the policy docs/tickets.md gives orbit's own repository.
var orbitPolicy = Policy{
	Root:      "/work",
	Protected: []string{"deploy/**"},
	Commands: [][]string{
		{"go", "build"}, {"go", "test"}, {"go", "vet"}, {"gofmt"}, {"make", "check"}, {"make", "fmt"}, {"make", "test"},
		{"git", "status"}, {"git", "diff"}, {"git", "log"}, {"git", "show"}, {"ls"}, {"rg"},
	},
}

func in(kv ...string) json.RawMessage {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return b
}

func TestToolPolicy(t *testing.T) {
	cases := []struct {
		tool  string
		input json.RawMessage
		allow bool
	}{
		// The file tools write inside the checkout, outside .git and the
		// protected paths.
		{"Write", in("file_path", "/work/internal/x/x.go", "content", "package x"), true},
		{"Edit", in("file_path", "/work/README.md"), true},
		{"Write", in("file_path", "docs/new.md"), true},
		{"Write", in("file_path", "/work/.github/workflows/ci.yml"), false},
		{"Edit", in("file_path", "/work/.github/CODEOWNERS"), false},
		{"Write", in("file_path", "/work/.gitmodules"), false},
		{"Write", in("file_path", "/work/deploy/Dockerfile"), false},
		{"Write", in("file_path", "/work/.git/hooks/pre-commit"), false},
		{"Write", in("file_path", "/work/vendor/x/.git/config"), false},
		{"Write", in("file_path", "/work/../etc/passwd"), false},
		{"Write", in("file_path", "/work/a/../../tmp/x"), false},
		{"Write", in("file_path", "/tmp/x"), false},
		{"Write", in("file_path", "/work"), false},
		{"Write", in("file_path", "/workshop/x"), false},
		{"Write", in("content", "no path"), false},
		// Reads and searches stay inside the checkout.
		{"Read", in("file_path", "/work/go.mod"), true},
		{"Read", in("file_path", "/etc/hosts"), false},
		{"Read", in("file_path", "/proc/self/environ"), false},
		{"Glob", in("pattern", "**/*.go"), true},
		{"Glob", in("pattern", "/etc/*"), false},
		{"Grep", in("pattern", "x", "path", "/tmp"), false},
		{"Grep", in("pattern", "x", "path", "/work/internal"), true},
		// Commands: allowed prefixes, joined by the four operators.
		{"Bash", in("command", "go test ./..."), true},
		{"Bash", in("command", "go test -race -count=1 ./internal/build/"), true},
		{"Bash", in("command", "gofmt -l . && go vet ./... ; make check | tail -n 5"), false}, // tail is not allowed
		{"Bash", in("command", "gofmt -l . && go vet ./... ; make check || git status"), true},
		{"Bash", in("command", "make check 2>&1"), true},
		{"Bash", in("command", "go test ./... > /work/test.log 2>&1"), true},
		{"Bash", in("command", "go build ./... > /dev/null"), true},
		{"Bash", in("command", `rg -n "func Decide" internal`), true},
		{"Bash", in("command", "git diff 'a b'"), true},
		{"Bash", in("command", "! go vet ./..."), true},
		// Everything else is refused.
		{"Bash", in("command", "git push origin HEAD"), false},
		{"Bash", in("command", "git -c core.hooksPath=x status"), false},
		{"Bash", in("command", "curl https://example.com"), false},
		{"Bash", in("command", "go run ./cmd/orbit"), false},
		{"Bash", in("command", "go test $(cat x)"), false},
		{"Bash", in("command", "go test `id`"), false},
		{"Bash", in("command", "go test $HOME"), false},
		{"Bash", in("command", `go test "$HOME"`), false},
		{"Bash", in("command", "go test ${X}"), false},
		{"Bash", in("command", "go test ~/x"), false},
		{"Bash", in("command", `go test \x`), false},
		{"Bash", in("command", "go test $'\\x41'"), false},
		{"Bash", in("command", "GOFLAGS=-x go test ./..."), false},
		{"Bash", in("command", "go test ./... &"), false},
		{"Bash", in("command", "(go test ./...)"), false},
		{"Bash", in("command", "{ go test ./...; }"), false},
		{"Bash", in("command", "f() { ls; }; f"), false},
		{"Bash", in("command", "if ls; then ls; fi"), false},
		{"Bash", in("command", "for x in a; do ls; done"), false},
		{"Bash", in("command", "go test ./... > /tmp/x"), false},
		{"Bash", in("command", "go test ./... > .github/workflows/ci.yml"), false},
		{"Bash", in("command", "ls > .git/config"), false},
		{"Bash", in("command", "ls < /etc/passwd"), false},
		{"Bash", in("command", "go test <(ls)"), false},
		{"Bash", in("command", "ls > *.txt"), false},
		{"Bash", in("command", "cat <<EOF\nx\nEOF"), false},
		{"Bash", in("command", "ls <<< x"), false},
		{"Bash", in("command", "ls |& rg x"), false},
		{"Bash", in("command", "l* -la"), false},
		{"Bash", in("command", "go te* ./..."), false},
		{"Bash", in("command", "make -f x.mk check"), false},
		{"Bash", in("command", "rm -rf /"), false},
		{"Bash", in("command", "go test ./... )"), false},
		{"Bash", in("command", ""), false},
		{"Bash", in("command", strings.Repeat("ls ", 4000)), false},
		{"Bash", json.RawMessage(`not json`), false},
		// Any tool the policy does not know.
		{"WebFetch", in("url", "https://example.com"), false},
		{"NotebookEdit", in("notebook_path", "/work/x.ipynb"), false},
		{"mcp__x__y", in(), false},
	}
	for _, c := range cases {
		d := orbitPolicy.Decide(c.tool, c.input)
		if d.Allow != c.allow {
			t.Errorf("%s %s: allow = %v (%s), want %v", c.tool, c.input, d.Allow, d.Message, c.allow)
		}
		if !d.Allow && d.Message == "" {
			t.Errorf("%s %s: a denial must say why", c.tool, c.input)
		}
	}
}

func TestBashDenialTellsTheModelWhatIsAllowed(t *testing.T) {
	d := orbitPolicy.Decide("Bash", in("command", "curl x"))
	for _, want := range []string{"`go test`", "`make check`", "/work", "/dev/null"} {
		if !strings.Contains(d.Message, want) {
			t.Errorf("denial %q lacks %q", d.Message, want)
		}
	}
}

func TestMatchGlob(t *testing.T) {
	for _, c := range []struct {
		glob, name string
		want       bool
	}{
		{".github/**", ".github/workflows/ci.yml", true},
		{".github/**", ".github", true},
		{".github/**", ".githubx/a", false},
		{"deploy/**", "deploy/cli/package.json", true},
		{"**/*.pem", "a/b/key.pem", true},
		{"**/*.pem", "key.pem", true},
		{"internal/*/secret.go", "internal/x/secret.go", true},
		{"internal/*/secret.go", "internal/x/y/secret.go", false},
		{"AGENTS.md", "AGENTS.md", true},
		{"AGENTS.md", "docs/AGENTS.md", false},
	} {
		if got := MatchGlob(c.glob, c.name); got != c.want {
			t.Errorf("MatchGlob(%q, %q) = %v", c.glob, c.name, got)
		}
	}
}
