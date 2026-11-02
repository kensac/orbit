// Package journal is a run's typed record: the prose, thinking, tool calls
// and their results, diffs, plans, approvals, questions, status changes and
// turns of every attempt, as items a client renders without knowing any
// backend's event format.
//
// The journal is derived from the stored transcript (run_events) by code
// (Project), deterministically, so the live write and a backfill of old
// runs produce the same rows. It is append-only: an item that changes (a
// tool call that finishes, a plan that is updated, a turn that ends) gets a
// new revision with its whole body, and earlier revisions are never
// rewritten. A reader folds revisions by item id, the latest winning.
//
// Rows are addressed by (run, epoch, seq). An epoch is one timeline: the
// attempts of a run that continue each other (an attempt that resumed the
// interrupted one's session) share the epoch of the first of them, and an
// attempt that started over opens a new epoch named by its own number.
// seq counts rows within an epoch from 1, so a cursor (epoch, seq) names
// exactly what a client has seen.
package journal

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// Version is the body schema version this build writes. A body's shape
// changes only with a new version and an upcaster for the old one
// (Upcast); stored rows keep the version they were written with.
const Version = 1

// Item kinds.
const (
	KindMessage  = "message"
	KindThinking = "thinking"
	KindToolCall = "tool_call"
	KindDiff     = "diff"
	KindPlan     = "plan"
	KindApproval = "approval"
	KindQuestion = "question"
	KindStatus   = "status"
	KindTurn     = "turn"
	// KindPermissions is a chat run's permission level being applied: at its start, or changed during the turn.
	KindPermissions = "permissions"
)

// Item is one stored revision of a journal item.
type Item struct {
	RunID    string    `json:"run_id"`
	Epoch    int       `json:"epoch"`
	Seq      int       `json:"seq"`
	Cursor   string    `json:"cursor"`
	Attempt  int       `json:"attempt"`
	ID       string    `json:"item_id"`
	Revision int       `json:"revision"`
	Kind     string    `json:"kind"`
	Turn     *string   `json:"turn_id"`
	Parent   *string   `json:"parent_id"`
	At       time.Time `json:"at"`
	V        int       `json:"v"`
	// Body is the kind's typed body (Message, ToolCall, ...), upcast to
	// Version when the row is older and this build knows how.
	Body json.RawMessage `json:"body"`
}

// Bounded is a body too large to send whole on every read: a tool's
// output, a diff, a long thought. Head is its start, cut at a character
// boundary to at most HeadMax bytes; Bytes and SHA256 are the full value's,
// which is fetched by digest when a client wants it.
type Bounded struct {
	Head      string `json:"head"`
	Bytes     int    `json:"bytes"`
	SHA256    string `json:"sha256"`
	Truncated bool   `json:"truncated"`
}

// HeadMax bounds a Bounded head: a screen of output on a phone, and small
// enough that a page of items stays a few hundred kilobytes.
const HeadMax = 4096

// Blob is the full value behind a Bounded.
type Blob struct {
	SHA256  string
	Content []byte
}

// Bound makes a Bounded of content, and the blob holding it whole.
func Bound(content []byte) (Bounded, Blob) {
	sum := sha256.Sum256(content)
	digest := hex.EncodeToString(sum[:])
	head := content
	if len(head) > HeadMax {
		head = head[:HeadMax]
		for len(head) > 0 && !utf8.Valid(head) {
			head = head[:len(head)-1]
		}
	}
	return Bounded{Head: strings.ToValidUTF8(string(head), "�"), Bytes: len(content), SHA256: digest,
		Truncated: len(head) < len(content)}, Blob{SHA256: digest, Content: content}
}

// ---------------------------------------------------------------- bodies

// Message is prose: the agent's (role assistant), or a user turn the
// backend recorded (role user, such as the prompt a subagent was given).
type Message struct {
	Role string `json:"role"`
	Text string `json:"text"`
}

// Thinking is the model's reasoning, where the backend exposes it.
type Thinking struct {
	Text Bounded `json:"text"`
	// TokensEstimate is the thought's length in tokens, estimated from its
	// bytes (bytesPerToken); backends report output tokens per turn, not
	// per block.
	TokensEstimate int `json:"tokens_estimate"`
	// Redacted is a thought the backend sent encrypted: there is no text,
	// and the estimate is 0.
	Redacted bool `json:"redacted,omitempty"`
}

// bytesPerToken is the usual ratio for English text and code under
// Claude's and OpenAI's tokenizers; the estimate is a size, not a bill.
const bytesPerToken = 4

func estimateTokens(s string) int { return (len(s) + bytesPerToken - 1) / bytesPerToken }

// Tool call states.
const (
	ToolRunning   = "running"
	ToolCompleted = "completed"
	ToolFailed    = "failed"
)

// ToolCall is one tool use. Revision 1 is the call (state running); the
// revision written when its result arrives carries the result.
type ToolCall struct {
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	// Input is the call's input, whole, as the backend sent it.
	Input json.RawMessage `json:"input"`
	// Summary is one line saying what the call was about (a path relative
	// to the run's directory, a pattern, a command, a URL), at most
	// SummaryMax characters, possibly empty.
	Summary string   `json:"summary"`
	State   string   `json:"state"`
	Result  *Bounded `json:"result,omitempty"`
}

// Diff is a change to one file a tool made. Patch is a unified diff when
// the change's content is known, absent when the backend reported only
// that the file changed.
type Diff struct {
	CallID string   `json:"call_id"`
	Path   string   `json:"path"`
	Change string   `json:"change"`
	Patch  *Bounded `json:"patch,omitempty"`
}

// Plan is the agent's working plan: its todo list, or a plan it presented.
// There is one plan item per epoch (id "plan"); each update is a revision.
type Plan struct {
	Entries []PlanEntry `json:"entries"`
	Text    string      `json:"text,omitempty"`
}

// PlanEntry is one step of a plan; Status is pending, in_progress or
// completed.
type PlanEntry struct {
	Text   string `json:"text"`
	Status string `json:"status"`
}

// Approval states.
const (
	ApprovalPending = "pending"
	ApprovalAllowed = "allowed"
	ApprovalDenied  = "denied"
)

// Approval is a backend asking orbit's policy whether a tool call may run
// (a build's permission protocol). Revision 1 is the request; the decision
// is the next.
type Approval struct {
	RequestID string          `json:"request_id"`
	Tool      string          `json:"tool"`
	Input     json.RawMessage `json:"input"`
	State     string          `json:"state"`
	Message   string          `json:"message,omitempty"`
}

// Permissions is a permission level applied to a chat run's process (orbit.permissions): the level asked for, the one the process is at (auto that the CLI refused is ask), its Claude Code mode, when (start, turn or live) and what set it.
type Permissions struct {
	Level     string     `json:"level"`
	Applied   string     `json:"applied"`
	Mode      string     `json:"mode"`
	When      string     `json:"when"`
	Source    string     `json:"source"`
	Version   int        `json:"version,omitempty"`
	SetBy     string     `json:"set_by,omitempty"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"`
	Reason    string     `json:"reason,omitempty"`
}

// Question is something the agent asked its person: one of its output's
// questions, or a question tool's.
type Question struct {
	Text    string           `json:"text"`
	Header  string           `json:"header,omitempty"`
	Options []QuestionOption `json:"options"`
	Multi   bool             `json:"multi_select,omitempty"`
}

// QuestionOption is one offered answer.
type QuestionOption struct {
	Label       string `json:"label"`
	Description string `json:"description,omitempty"`
}

// Status is something that happened to the run that is not the model's
// own output: orbit's steps (source fetches, the session plan, a gate),
// the backend's session start, a rate limit report. Code is the
// transcript event's kind; Detail its payload.
type Status struct {
	Code   string  `json:"code"`
	Detail Bounded `json:"detail"`
}

// Turn states. A turn's state says whether it is still going and how it
// ended as orbit observed it; Outcome is the backend's own verdict, kept
// apart, because a completed turn can be a failed one (an API error ends a
// turn too) and an interrupted one has no verdict at all.
const (
	TurnRunning   = "running"
	TurnCompleted = "completed"
	// TurnInterrupted is a turn whose process orbit saw end without a
	// result: a timeout, a crash, a drain's hand-back.
	TurnInterrupted = "interrupted"
	// TurnUnverifiable is a turn whose attempt was lost (reaped without a
	// heartbeat): nothing observed how it ended.
	TurnUnverifiable = "unverifiable"
)

// Turn is one exchange with the backend: from the request to its result.
type Turn struct {
	State string `json:"state"`
	// Outcome is the backend's verdict: success, error, max_turns,
	// max_tokens, refused, or another word a backend reported. Absent when
	// none was reported, which is never success.
	Outcome     string          `json:"outcome,omitempty"`
	StartedAt   time.Time       `json:"started_at"`
	CompletedAt *time.Time      `json:"completed_at,omitempty"`
	DurationMS  *int64          `json:"duration_ms,omitempty"`
	CostUSD     *float64        `json:"cost_usd,omitempty"`
	Usage       json.RawMessage `json:"usage,omitempty"`
	Error       string          `json:"error,omitempty"`
}

// ---------------------------------------------------------------- cursors

// Cursor names a row: its epoch and its seq there. Sent as "epoch.seq".
type Cursor struct{ Epoch, Seq int }

func (c Cursor) String() string { return strconv.Itoa(c.Epoch) + "." + strconv.Itoa(c.Seq) }

// ParseCursor reads "epoch.seq". Anything else is not a cursor.
func ParseCursor(s string) (Cursor, bool) {
	e, q, ok := strings.Cut(s, ".")
	if !ok {
		return Cursor{}, false
	}
	epoch, err1 := strconv.Atoi(e)
	seq, err2 := strconv.Atoi(q)
	if err1 != nil || err2 != nil || epoch < 1 || seq < 0 || strconv.Itoa(epoch) != e || strconv.Itoa(seq) != q {
		return Cursor{}, false
	}
	return Cursor{epoch, seq}, true
}

// Reset reasons: why a cursor cannot continue the timeline a reader is on.
const (
	// ResetEpoch is a cursor into an earlier epoch: the run started over,
	// and what the client has belongs to an abandoned timeline.
	ResetEpoch = "epoch_changed"
	// ResetAhead is a cursor past the epoch's last row, which no reader
	// was ever sent.
	ResetAhead = "cursor_ahead"
)

// Resume says where a reader holding c continues in the timeline whose
// epoch is epoch and whose last row is last: after c.Seq, or from the
// start with the reason the client's copy is void. A partial replay across
// epochs would mix two timelines, so there is none.
func Resume(c Cursor, epoch, last int) (after int, reset string) {
	switch {
	case c.Epoch != epoch:
		return 0, ResetEpoch
	case c.Seq > last:
		return 0, ResetAhead
	}
	return c.Seq, ""
}

// ---------------------------------------------------------------- upcast

// Upcast returns a stored body in this build's shape. Version 1 is the
// only version so far, so every known row passes through. A row from a
// newer version (written by a newer instance during a deploy) is returned
// as stored, never rewritten, and the reader sees its v.
func Upcast(v int, kind string, body json.RawMessage) (json.RawMessage, error) {
	if v < 1 {
		return nil, fmt.Errorf("journal %s row has version %d", kind, v)
	}
	return body, nil
}

// Fold reduces revisions to the latest of each item, ordered by where each
// item first appeared.
func Fold(revisions []Item) []Item {
	index := map[string]int{}
	var out []Item
	for _, it := range revisions {
		if i, ok := index[it.ID]; ok {
			if it.Revision > out[i].Revision {
				out[i] = it
			}
			continue
		}
		index[it.ID] = len(out)
		out = append(out, it)
	}
	return out
}
