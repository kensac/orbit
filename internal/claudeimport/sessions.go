package claudeimport

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/store"
)

// A Claude Code session transcript is one JSON object per line in <projects>/<cwd key>/<session id>.jsonl, appended to as the session goes on. orbit stores each line in a canonical form (NormalizeLine): its keys sorted, its session id taken out, credentials scrubbed, an oversized line cut to its skeleton. A session resumed or forked from another starts with its parent's lines under a new session id, so with the id taken out those lines are byte for byte the parent's, and are stored once.

// SourceClaudeCode is the source of a Claude Code session.
const SourceClaudeCode = "claude-code"

// MaxSessionLine bounds one stored line. Claude Code writes a tool's whole result into its line (a file read, an image as base64), so a line can run to megabytes; past this it is kept as its skeleton (cutLine), which keeps the session's structure (every message, every tool call and result, in order) so it still resumes.
const MaxSessionLine = 1 << 20

// SessionFile is one session transcript on disk.
type SessionFile struct {
	Path      string
	SessionID string
}

// FindSessions lists the session transcripts under dir: dir is Claude Code's projects directory (~/.claude/projects, a directory per working directory) or one project's directory. Only a top-level <uuid>.jsonl is a session; subagent transcripts under <id>/ belong to theirs. Sorted by path.
func FindSessions(dir string) ([]SessionFile, error) {
	var out []SessionFile
	add := func(d string) error {
		ents, err := os.ReadDir(d)
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := e.Name()
			id, ok := strings.CutSuffix(name, ".jsonl")
			if !ok || !e.Type().IsRegular() || !store.IsUUID(id) {
				continue
			}
			out = append(out, SessionFile{Path: filepath.Join(d, name), SessionID: id})
		}
		return nil
	}
	if err := add(dir); err != nil {
		return nil, err
	}
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, e := range ents {
		if e.IsDir() && !store.IsUUID(e.Name()) {
			if err := add(filepath.Join(dir, e.Name())); err != nil {
				return nil, err
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, nil
}

// CompleteLines splits a transcript into its complete lines, each with the offset just after its newline. A last line without a newline is being written and is left for the next import.
func CompleteLines(b []byte) (lines [][]byte, ends []int64) {
	var off int64
	for {
		i := bytes.IndexByte(b, '\n')
		if i < 0 {
			return lines, ends
		}
		off += int64(i) + 1
		if line := bytes.TrimSpace(b[:i]); len(line) > 0 {
			lines = append(lines, line)
			ends = append(ends, off)
		}
		b = b[i+1:]
	}
}

// PrefixSHA256 is the hex SHA-256 of a transcript's first n bytes: how an import knows the file it continues is the one it read, before it reads only what was appended.
func PrefixSHA256(b []byte, n int64) string {
	sum := sha256.Sum256(b[:n])
	return hex.EncodeToString(sum[:])
}

// ErrNotObject is a line that is not a JSON object.
var ErrNotObject = errors.New("not a JSON object")

// NormalizeLine is a line's stored form: a JSON object with its keys sorted, without its sessionId, credentials scrubbed (claudeproto.ScrubCredentials), and cut to its skeleton past MaxSessionLine. It is idempotent, so a client and the server can both apply it.
func NormalizeLine(raw []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil || obj == nil {
		return nil, ErrNotObject
	}
	delete(obj, "sessionId")
	b, err := marshal(obj)
	if err != nil {
		return nil, err
	}
	if len(b) > MaxSessionLine {
		if b, err = cutLine(obj, len(b)); err != nil {
			return nil, err
		}
	}
	scrubbed := []byte(claudeproto.ScrubCredentials(string(b)))
	if !json.Valid(scrubbed) {
		// A pattern that broke the JSON would leave a line no reader can use; its skeleton holds no text to scrub.
		if scrubbed, err = cutLine(obj, len(b)); err != nil {
			return nil, err
		}
	}
	return scrubbed, nil
}

// skeletonKeys are the fields of a line its skeleton keeps: what places it in the session and what the CLI needs to rebuild the conversation.
var skeletonKeys = []string{"type", "uuid", "parentUuid", "logicalParentUuid", "isSidechain", "isMeta", "timestamp", "cwd", "gitBranch", "version", "userType", "requestId", "subtype", "leafUuid", "summary"}

// cutLine is an oversized line's skeleton: its placing fields, and its message with each content block replaced by a note of what was left out, except that a tool call keeps its id and name and a tool result its call's id, so every call still has its result.
func cutLine(obj map[string]json.RawMessage, size int) ([]byte, error) {
	out := map[string]any{"orbitOmittedBytes": size}
	for _, k := range skeletonKeys {
		if v, ok := obj[k]; ok && len(v) <= 64<<10 {
			out[k] = v
		}
	}
	note := fmt.Sprintf("[orbit omitted this part of a %d-byte line]", size)
	var msg struct {
		Role    string          `json:"role"`
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
	}
	if raw, ok := obj["message"]; ok && json.Unmarshal(raw, &msg) == nil {
		m := map[string]any{"role": msg.Role}
		if msg.ID != "" {
			m["id"] = msg.ID
		}
		if msg.Model != "" {
			m["model"] = msg.Model
		}
		var blocks []map[string]json.RawMessage
		if json.Unmarshal(msg.Content, &blocks) == nil {
			cut := make([]map[string]any, 0, len(blocks))
			for _, bl := range blocks {
				var typ string
				_ = json.Unmarshal(bl["type"], &typ)
				switch typ {
				case "tool_use":
					cut = append(cut, map[string]any{"type": "tool_use", "id": bl["id"], "name": bl["name"], "input": map[string]any{}})
				case "tool_result":
					c := map[string]any{"type": "tool_result", "tool_use_id": bl["tool_use_id"], "content": note}
					if e, ok := bl["is_error"]; ok {
						c["is_error"] = e
					}
					cut = append(cut, c)
				default:
					cut = append(cut, map[string]any{"type": "text", "text": note})
				}
			}
			m["content"] = cut
		} else {
			m["content"] = note
		}
		out["message"] = m
	}
	return marshal(out)
}

// marshal encodes v without escaping <, > and &, as Claude Code writes them, so a stored line is no longer than the one read.
func marshal(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// RestoreLine is a stored line as Claude Code writes it in a session file: with sessionId set to id.
func RestoreLine(stored []byte, id string) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(stored, &obj); err != nil || obj == nil {
		return nil, ErrNotObject
	}
	b, _ := json.Marshal(id)
	obj["sessionId"] = b
	return marshal(obj)
}

// LineMeta is what a line says about its session.
type LineMeta struct {
	Type   string
	Cwd    string
	Branch string
	// At is the line's timestamp, nil when it has none.
	At *time.Time
	// Title is a title the session was given: a custom title (custom-title) or the CLI's summary (summary).
	Title string
	// Prompt is the person's own text in a user line: not a tool result, not a meta line, not a command or caveat the CLI wrapped in tags.
	Prompt string
}

// wrappedRE is a user line's text the CLI generated rather than the person typed: <command-name>, <local-command-stdout>, a system reminder.
var wrappedRE = regexp.MustCompile(`^\s*(<[a-z][a-z-]*>|Caveat: )`)

// ParseLineMeta reads a (stored or raw) line's meta. A line it cannot read gives none.
func ParseLineMeta(line []byte) LineMeta {
	var l struct {
		Type        string          `json:"type"`
		Cwd         string          `json:"cwd"`
		GitBranch   string          `json:"gitBranch"`
		Timestamp   string          `json:"timestamp"`
		Summary     string          `json:"summary"`
		CustomTitle string          `json:"customTitle"`
		IsMeta      bool            `json:"isMeta"`
		Message     json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil {
		return LineMeta{}
	}
	m := LineMeta{Type: l.Type, Cwd: l.Cwd, Branch: l.GitBranch}
	if t, err := time.Parse(time.RFC3339Nano, l.Timestamp); err == nil {
		m.At = &t
	}
	switch l.Type {
	case "summary":
		m.Title = strings.TrimSpace(l.Summary)
	case "custom-title":
		m.Title = strings.TrimSpace(l.CustomTitle)
	case "user":
		if !l.IsMeta {
			if p := userText(l.Message); p != "" && !wrappedRE.MatchString(p) {
				m.Prompt = p
			}
		}
	}
	return m
}

// userText is a user message's typed text: its content when that is a string, else its text blocks joined.
func userText(raw json.RawMessage) string {
	var msg struct {
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return ""
	}
	var s string
	if json.Unmarshal(msg.Content, &s) == nil {
		return strings.TrimSpace(s)
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(msg.Content, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		if b.Type != "text" {
			// A tool result is the CLI's, not the person's.
			return ""
		}
		if t := strings.TrimSpace(b.Text); t != "" {
			parts = append(parts, t)
		}
	}
	return strings.Join(parts, "\n\n")
}

// RepoOf names the git repository a working directory is a checkout of, from its origin remote: "owner/name" for a GitHub remote, the remote's URL without credentials otherwise, "" when the directory is gone or not a checkout. It reads files only (a worktree's .git file leads to the main checkout's config).
func RepoOf(cwd string) string {
	if cwd == "" {
		return ""
	}
	gitPath := filepath.Join(cwd, ".git")
	fi, err := os.Stat(gitPath)
	if err != nil {
		return ""
	}
	config := filepath.Join(gitPath, "config")
	if !fi.IsDir() {
		b, err := os.ReadFile(gitPath) //nolint:gosec // a checkout the person's session named
		if err != nil {
			return ""
		}
		dir, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
		if !ok {
			return ""
		}
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(cwd, dir)
		}
		common := dir
		if c, err := os.ReadFile(filepath.Join(dir, "commondir")); err == nil { //nolint:gosec // under that checkout's git directory
			common = filepath.Join(dir, strings.TrimSpace(string(c)))
		}
		config = filepath.Join(common, "config")
	}
	f, err := os.Open(config) //nolint:gosec // that checkout's git config
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	inOrigin := false
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if k, v, ok := strings.Cut(line, "="); inOrigin && ok && strings.TrimSpace(k) == "url" {
			return repoName(strings.TrimSpace(v))
		}
	}
	return ""
}

var (
	githubRE   = regexp.MustCompile(`github\.com[:/]([^/\s]+)/([^/\s]+?)(?:\.git)?/?$`)
	userinfoRE = regexp.MustCompile(`://[^/@]*@`)
)

func repoName(url string) string {
	if m := githubRE.FindStringSubmatch(url); m != nil {
		return m[1] + "/" + m[2]
	}
	return userinfoRE.ReplaceAllString(url, "://")
}

// ImportState is how much of a session orbit holds: GET /sessions's offset and prefix_sha256.
type ImportState struct {
	Offset       int64
	PrefixSHA256 string
}

// Planned outcomes beside store's: a file with no complete line yet is left alone.
const PlanEmpty = "empty"

// SessionPlan is what an import of one file would do: send its lines after From (Rewrite: all of them, as a new generation, because the file no longer starts with what orbit holds).
type SessionPlan struct {
	File    SessionFile
	Outcome string
	From    int64
	Rewrite bool
	// Lines are the complete lines after From, each ending at Ends[i].
	Lines [][]byte
	Ends  []int64
	// Bytes is the length of what would be sent, before normalization.
	Bytes int64
	Cwd   string
}

// PlanSession decides what an import of one file sends, given what orbit holds of it (nil: nothing). Only complete lines are sent; a prefix that still hashes to what orbit holds is skipped.
func PlanSession(f SessionFile, b []byte, st *ImportState) SessionPlan {
	lines, ends := CompleteLines(b)
	p := SessionPlan{File: f}
	for _, l := range lines {
		if m := ParseLineMeta(l); m.Cwd != "" {
			p.Cwd = m.Cwd
			break
		}
	}
	var end int64
	if len(ends) > 0 {
		end = ends[len(ends)-1]
	}
	switch {
	case st == nil && end == 0:
		p.Outcome = PlanEmpty
		return p
	case st == nil:
		p.Outcome = store.SessionCreated
	case st.Offset <= int64(len(b)) && PrefixSHA256(b, st.Offset) == st.PrefixSHA256:
		p.From = st.Offset
		if end <= st.Offset {
			p.Outcome = store.SessionUnchanged
			return p
		}
		p.Outcome = store.SessionAppended
	default:
		p.Outcome, p.Rewrite = store.SessionRewritten, true
	}
	for i, e := range ends {
		if e > p.From {
			p.Lines, p.Ends = append(p.Lines, lines[i]), append(p.Ends, e)
		}
	}
	p.Bytes = end - p.From
	return p
}

// Speakers of a rendered line.
const (
	SpeakerPerson    = "person"
	SpeakerAssistant = "assistant"
	SpeakerSummary   = "summary"
)

// toolInputMax bounds a tool call's input in a rendered line: enough to show the command or the file, not its content.
const toolInputMax = 300

// RenderLine is a stored line as a reader of the session needs it: who spoke and what, "" for a line with nothing to read (a tool result, a meta line, the CLI's bookkeeping). The person's typed text, the assistant's text with each tool call as "[tool <name>: <input>]", a summary's text.
func RenderLine(line []byte) (speaker, text string) {
	var l struct {
		Type    string          `json:"type"`
		IsMeta  bool            `json:"isMeta"`
		Summary string          `json:"summary"`
		Message json.RawMessage `json:"message"`
	}
	if json.Unmarshal(line, &l) != nil || l.IsMeta {
		return "", ""
	}
	switch l.Type {
	case "summary":
		return SpeakerSummary, strings.TrimSpace(l.Summary)
	case "user":
		return SpeakerPerson, userText(l.Message)
	case "assistant":
	default:
		return "", ""
	}
	var msg struct {
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if json.Unmarshal(l.Message, &msg) != nil {
		return "", ""
	}
	var parts []string
	for _, b := range msg.Content {
		switch b.Type {
		case "text":
			if t := strings.TrimSpace(b.Text); t != "" {
				parts = append(parts, t)
			}
		case "tool_use":
			parts = append(parts, "[tool "+b.Name+": "+toolInput(b.Input)+"]")
		}
	}
	return SpeakerAssistant, strings.Join(parts, "\n")
}

// toolInput is a tool call's input in a line: the field that says what it did (a command, a path, a pattern, a URL) when it has one, else its JSON, cut to toolInputMax.
func toolInput(raw json.RawMessage) string {
	var m map[string]json.RawMessage
	s := string(raw)
	if json.Unmarshal(raw, &m) == nil {
		for _, k := range []string{"command", "file_path", "path", "pattern", "url", "query", "description", "prompt"} {
			var v string
			if json.Unmarshal(m[k], &v) == nil && v != "" {
				s = v
				break
			}
		}
	}
	return oneLine(s, toolInputMax)
}
