package harness

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"time"
)

// The repository tools: read-only history and GitHub state of the
// repositories an agent's spec names (repos:), run by orbit, never by the
// model: git over orbit's mirror, GitHub as the App with a read token
// minted per call. The tool server holds neither; it asks the engine over a
// unix socket the engine serves for that agent alone (engine/repos.go), so
// a run reaches only its own agent's repositories.
// RepoToolNames are the repository tools as the model sees them.
func RepoToolNames() []string {
	names := make([]string, len(RepoTools))
	for i, t := range RepoTools {
		names[i] = "mcp__" + MCPServer + "__" + t.Name
	}
	return names
}

// RepoTool is one repository tool as an MCP server lists it.
type RepoTool struct {
	Name        string
	Description string
	Schema      map[string]any
}

func repoProp() map[string]any {
	return map[string]any{"type": "string", "description": "owner/name, one of the repositories in your prompt; may be omitted when you have one"}
}

func str(desc string) map[string]any { return map[string]any{"type": "string", "description": desc} }

func obj(required []string, props map[string]any) map[string]any {
	props["repo"] = repoProp()
	if required == nil {
		required = []string{}
	}
	return map[string]any{"type": "object", "additionalProperties": false, "required": required, "properties": props}
}

// RepoTools are the repository tools, in the order they are listed.
var RepoTools = []RepoTool{
	{"git_log", "Commits of one of your repositories, newest first: sha, author, date, subject. Exact, from orbit's mirror of the repository.",
		obj(nil, map[string]any{
			"rev":   str("branch, tag, sha or refs/pull/<n>/head; default the ref your prompt names"),
			"path":  str("only commits touching this path"),
			"since": str("only commits on or after YYYY-MM-DD"),
			"max":   map[string]any{"type": "integer", "minimum": 1, "maximum": 200, "description": "at most this many (default 20)"},
		})},
	{"git_show", "One commit (message, files changed, patch), or with path one file's content at that revision.",
		obj([]string{"rev"}, map[string]any{
			"rev":  str("the commit: a sha, a branch, a tag, main~3, refs/pull/<n>/head"),
			"path": str("a file: its content at rev instead of the commit"),
		})},
	{"git_diff", "The patch between two revisions (base to head), optionally only one path, or with stat only the files and line counts. For a pull request, base is its base branch and head refs/pull/<n>/head.",
		obj([]string{"base", "head"}, map[string]any{
			"base": str("the older revision"),
			"head": str("the newer revision"),
			"path": str("only this path"),
			"stat": map[string]any{"type": "boolean", "description": "files and line counts only"},
		})},
	{"git_blame", "Who last changed each line of a file: commit, author, date per line.",
		obj([]string{"path"}, map[string]any{
			"path":  str("the file"),
			"rev":   str("the revision; default the ref your prompt names"),
			"start": map[string]any{"type": "integer", "minimum": 1, "description": "first line (1-based)"},
			"end":   map[string]any{"type": "integer", "minimum": 1, "description": "last line, inclusive"},
		})},
	{"pr_list", "Pull requests of one of your repositories from GitHub, most recently updated first, every page up to limit: number, title, state, draft, merged_at (set only when merged), author, head and base.",
		obj(nil, map[string]any{
			"state": map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}, "description": "default open"},
			"limit": map[string]any{"type": "integer", "minimum": 1, "maximum": 300, "description": "at most this many (default 30)"},
		})},
	{"pr_view", "One pull request from GitHub in full (body, merged, mergeable, size) with the check runs and commit statuses of its head commit.",
		obj([]string{"number"}, map[string]any{
			"number": map[string]any{"type": "integer", "minimum": 1},
		})},
}

// IsRepoTool reports whether name (unprefixed) is a repository tool.
func IsRepoTool(name string) bool {
	for _, t := range RepoTools {
		if t.Name == name {
			return true
		}
	}
	return false
}

// RepoRequest is one repository tool call as the model made it; the engine
// adds the agent.
type RepoRequest struct {
	Tool string          `json:"tool"`
	Args json.RawMessage `json:"args"`
}

// RepoResponse is the engine's answer: the text the model reads, or why
// not.
type RepoResponse struct {
	Text  string `json:"text,omitempty"`
	Error string `json:"error,omitempty"`
}

// repoTimeout bounds one call: a git read answers in well under a second
// on a mirror, a GitHub read in a few, and the engine bounds each command
// itself (internal/repos).
const repoTimeout = 60 * time.Second

// maxRepoResponse is above the engine's own bound on an answer
// (repos.MaxOutput) plus its JSON escaping.
const maxRepoResponse = 1 << 20

// Repo asks the engine listening on socket to run one repository tool.
func Repo(socket string, req RepoRequest) (string, error) {
	conn, err := net.DialTimeout("unix", socket, repoTimeout)
	if err != nil {
		return "", fmt.Errorf("the repository tools are unavailable: %w", err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(repoTimeout))
	b, _ := json.Marshal(req)
	if _, err := conn.Write(append(b, '\n')); err != nil {
		return "", fmt.Errorf("%s: %w", req.Tool, err)
	}
	line, err := bufio.NewReader(io.LimitReader(conn, maxRepoResponse)).ReadBytes('\n')
	if err != nil {
		return "", fmt.Errorf("%s: %w", req.Tool, err)
	}
	var resp RepoResponse
	if err := json.Unmarshal(bytes.TrimSpace(line), &resp); err != nil {
		return "", fmt.Errorf("%s: %w", req.Tool, err)
	}
	if resp.Error != "" {
		return "", errors.New(resp.Error)
	}
	return resp.Text, nil
}

// ReadRepoRequest reads one request line, for the engine's side.
func ReadRepoRequest(conn net.Conn) (RepoRequest, error) {
	var req RepoRequest
	line, err := bufio.NewReader(io.LimitReader(conn, 64<<10)).ReadBytes('\n')
	if err != nil {
		return req, err
	}
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		return req, fmt.Errorf("repository tool: %w", err)
	}
	if !IsRepoTool(req.Tool) {
		return req, fmt.Errorf("unknown repository tool %q", req.Tool)
	}
	return req, nil
}
