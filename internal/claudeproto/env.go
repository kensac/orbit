package claudeproto

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// The CLI's credentials. A subscription token (claude setup-token) and an API key are the two orbit hands a model; the others are listed so that none can ride along with them.
const (
	OAuthToken = "CLAUDE_CODE_OAUTH_TOKEN"
	APIKey     = "ANTHROPIC_API_KEY"
)

// authNames are every variable the CLI authenticates with. A child gets at most one of them.
var authNames = []string{OAuthToken, APIKey, "ANTHROPIC_AUTH_TOKEN", "AWS_BEARER_TOKEN_BEDROCK",
	"CLAUDE_CODE_OAUTH_REFRESH_TOKEN"}

// stampNames mark a process as running inside a Claude Code session. A CLI that inherits them treats itself as that session's child (its hooks, its session id), which orbit run under a CLI (a developer's shell) would otherwise pass on.
var stampNames = []string{"CLAUDECODE", "CLAUDE_CODE_ENTRYPOINT", "CLAUDE_CODE_CHILD_SESSION",
	"CLAUDE_CODE_SESSION_ID", "CLAUDE_CODE_BRIDGE_SESSION_ID", "CLAUDE_CODE_SSE_PORT"}

// Credential is the one credential a child gets: the subscription token when set, else the API key, else "". An API key the CLI is handed takes precedence over a subscription login and is billed per token, so with a token present the key is withheld: a stray key in orbit's environment never turns subscription runs into API spend.
func Credential(getenv func(string) (string, bool)) string {
	for _, n := range []string{OAuthToken, APIKey} {
		if v, ok := getenv(n); ok && v != "" {
			return n
		}
	}
	return ""
}

// Env finishes a child's environment, built from an allowlist by the caller (runner.childEnv, sandbox.AllowedEnv): it keeps one credential, cred, and drops every other auth variable, an ANTHROPIC_CUSTOM_HEADERS that carries a credential, every session stamp, and an earlier copy of any repeated name (the last one is what exec passes). An allowlist that grows by mistake still cannot hand the model a second credential.
func Env(env []string, cred string) []string {
	drop := map[string]bool{}
	for _, n := range authNames {
		drop[n] = n != cred
	}
	for _, n := range stampNames {
		drop[n] = true
	}
	last := map[string]int{}
	for i, kv := range env {
		name, _, _ := strings.Cut(kv, "=")
		last[name] = i
	}
	out := make([]string, 0, len(env))
	for i, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		if last[name] != i || drop[name] {
			continue
		}
		if name == "ANTHROPIC_CUSTOM_HEADERS" && credentialHeader(val) {
			continue
		}
		out = append(out, kv)
	}
	return withoutRejectedKey(out)
}

func credentialHeader(v string) bool {
	v = strings.ToLower(v)
	for _, s := range []string{"authorization", "x-api-key", "api-key", "bearer"} {
		if strings.Contains(v, s) {
			return true
		}
	}
	return false
}

// withoutRejectedKey drops ANTHROPIC_API_KEY when the CLI's config under the child's HOME records the person declining that key at the CLI's "use this API key?" prompt. Interactively the CLI then ignores it, but a non-interactive one still uses and bills it. The config is .config.json in CLAUDE_CONFIG_DIR (default HOME/.claude) if present, else .claude.json in CLAUDE_CONFIG_DIR or HOME, and a key is recorded as its trimmed last 20 characters (Claude Code 2.1.316's customApiKeyResponses). A persistent session HOME is where such a config can exist.
func withoutRejectedKey(env []string) []string {
	var key, home, configDir string
	for _, kv := range env {
		name, val, _ := strings.Cut(kv, "=")
		switch name {
		case APIKey:
			key = strings.TrimSpace(val)
		case "HOME":
			home = val
		case "CLAUDE_CONFIG_DIR":
			configDir = val
		}
	}
	if key == "" || (home == "" && configDir == "") {
		return env
	}
	legacyDir, dir := configDir, configDir
	if configDir == "" {
		legacyDir, dir = filepath.Join(home, ".claude"), home
	}
	file := filepath.Join(dir, ".claude.json")
	if legacy := filepath.Join(legacyDir, ".config.json"); fileExists(legacy) {
		file = legacy
	}
	b, err := os.ReadFile(file)
	if err != nil {
		return env
	}
	var cfg struct {
		CustomAPIKeyResponses struct {
			Rejected []string `json:"rejected"`
		} `json:"customApiKeyResponses"`
	}
	if json.Unmarshal(b, &cfg) != nil {
		return env
	}
	id := key
	if len(id) > 20 {
		id = id[len(id)-20:]
	}
	for _, r := range cfg.CustomAPIKeyResponses.Rejected {
		if r == id {
			out := env[:0:0]
			for _, kv := range env {
				if !strings.HasPrefix(kv, APIKey+"=") {
					out = append(out, kv)
				}
			}
			return out
		}
	}
	return env
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}
