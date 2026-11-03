package claudeproto

import (
	"fmt"
	"strings"
)

// Transport is how the host and the CLI talk; this package owns the flags that set it, so a caller cannot start a CLI whose streams the package would misread.
type Transport struct {
	// Stream takes the prompt as stream-json user messages on stdin (--input-format stream-json), for a process that takes several turns or answers control requests. Without it the prompt is plain text on stdin and the CLI exits after one turn.
	Stream bool
	// PermissionPrompt sends every tool call the CLI's rules leave as "ask" to the host as can_use_tool (--permission-prompt-tool stdio). Needs Stream: the answer travels on stdin.
	PermissionPrompt bool
	// Resume continues this session as a fork: a new session id, the parent's transcript copied, so a turn that fails or is interrupted never edits the session the next attempt continues.
	Resume string
	// ResumeAt, with Resume, continues the session only up to this message (an assistant message's uuid), dropping what came after (--resume-session-at; on 2.1.316, with --fork-session, the fork answered from the session as it stood at that message).
	ResumeAt string
	// Ephemeral keeps no transcript (--no-session-persistence).
	Ephemeral bool
}

// ownedFlags are the flags Transport sets, and others that would change what the stream carries or which session it writes; a caller's own args may name none of them. Short spellings are the CLI's aliases.
var ownedFlags = map[string]bool{
	"print": true, "p": true, "output-format": true, "input-format": true, "verbose": true,
	"permission-prompt-tool": true, "resume": true, "r": true, "continue": true, "c": true,
	"fork-session": true, "session-id": true, "resume-session-at": true,
	"no-session-persistence": true, "replay-user-messages": true,
	"include-partial-messages": true, "await-initialize": true, "session-mirror": true,
}

// Args is the full argument list: the transport's flags, then extra (the caller's model, tools, settings and prompts). It refuses an extra flag the transport owns.
func Args(t Transport, extra []string) ([]string, error) {
	for _, a := range extra {
		if a == "--" {
			break
		}
		if name, ok := flagName(a); ok && ownedFlags[name] {
			return nil, fmt.Errorf("claude flag %s is set by the transport, not by a caller", a)
		}
	}
	if t.PermissionPrompt && !t.Stream {
		return nil, fmt.Errorf("a permission prompt needs a stream-json transport")
	}
	args := []string{"-p", "--output-format", "stream-json", "--verbose"}
	if t.Stream {
		args = append(args, "--input-format", "stream-json")
	}
	if t.PermissionPrompt {
		args = append(args, "--permission-prompt-tool", "stdio")
	}
	if t.Ephemeral {
		if t.Resume != "" {
			return nil, fmt.Errorf("an ephemeral session cannot resume one")
		}
		args = append(args, "--no-session-persistence")
	}
	args = append(args, ResumeArgs(t.Resume)...)
	if t.ResumeAt != "" {
		if t.Resume == "" {
			return nil, fmt.Errorf("resuming at a message needs a session to resume")
		}
		args = append(args, "--resume-session-at", t.ResumeAt)
	}
	return append(args, extra...), nil
}

// ResumeArgs continue session id as a fork; none for an empty id.
func ResumeArgs(id string) []string {
	if id == "" {
		return nil
	}
	return []string{"--resume", id, "--fork-session"}
}

// flagName is the name of a flag argument (--name, --name=value, -n), and false for a value. A value that starts with "-" is mistaken for a flag only when it also spells one this package owns, which a caller's value never needs to.
func flagName(a string) (string, bool) {
	switch {
	case strings.HasPrefix(a, "--"):
		name, _, _ := strings.Cut(a[2:], "=")
		return name, name != ""
	case len(a) == 2 && a[0] == '-' && a[1] != '-':
		return a[1:], true
	}
	return "", false
}
