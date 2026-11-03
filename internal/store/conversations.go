package store

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/status"
)

// ErrInvalid is a request the store refuses on its content (a 400).
var ErrInvalid = errors.New("invalid")

// titleMax is a conversation title's length in characters: one line of a
// sidebar.
const titleMax = 60

// DefaultTitle names a conversation after its first message: the first
// non-empty line, Markdown markers and runs of whitespace removed, cut at a
// word boundary to titleMax characters. Code, not a model: the same message
// always gives the same title.
func DefaultTitle(body string) string {
	var line string
	for _, l := range strings.Split(body, "\n") {
		l = strings.TrimLeft(strings.TrimSpace(l), "#>*-+` \t")
		if l != "" {
			line = l
			break
		}
	}
	line = strings.Join(strings.FieldsFunc(line, unicode.IsSpace), " ")
	if utf8.RuneCountInString(line) <= titleMax {
		return line
	}
	r := []rune(line)[:titleMax-3]
	cut := string(r)
	if i := strings.LastIndexByte(cut, ' '); i > titleMax/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .,;:") + "..."
}

// Conversation is one of an agent's conversations as the API reports it.
type Conversation struct {
	ID             string    `json:"id"`
	Agent          string    `json:"agent"`
	Title          string    `json:"title"`
	IsDefault      bool      `json:"is_default"`
	Archived       bool      `json:"archived"`
	CreatedAt      time.Time `json:"created_at"`
	LastActivityAt time.Time `json:"last_activity_at"`
	// NeedsInput is true while one of its questions is open or one of its
	// runs' actions awaits approval.
	NeedsInput bool `json:"needs_input" db:"-"`
	// TicketID is set when the conversation is a ticket's.
	TicketID *string `json:"ticket_id"`
	// Purpose is learning for the agent's learning conversation, where its learning runs are (archived); null for a person's.
	Purpose *string `json:"purpose"`
	// TaskID is set when the conversation is a delegated task's own. Its
	// questions are to the delegating agent, so they do not count as
	// waiting on the person.
	TaskID *string `json:"task_id"`
	// WorkspaceID is its workspace's id (docs/api.md, Workspace), null
	// until it has one.
	WorkspaceID *string `json:"workspace_id"`
	// AgentStatus is the agent's status in this conversation
	// (status.Aggregate over its current run).
	AgentStatus status.Status `json:"agent_status" db:"-"`

	OpenQuestion  bool      `json:"-"`
	PendingAction bool      `json:"-"`
	CurrentRun    *RunFacts `json:"-"`
	// LastRun is the conversation's newest run, null before its first.
	LastRun *RunSummary `json:"last_run"`
	// ReadThrough is how far the person has read it (MarkRead); null
	// before they first did.
	ReadThrough *time.Time `json:"read_through"`
	// FromSession is the imported session it was started from (POST /conversations with from_session).
	FromSession *string `json:"from_session"`
	// Model and Effort are what the conversation's runs use instead of the
	// agent's runner's own; null is the runner's.
	Model  *string `json:"model"`
	Effort *string `json:"effort"`
	// ForkedFrom names the conversation and message a fork was started at
	// (ForkConversation); null for any other.
	ForkedFrom *ForkPoint `json:"forked_from"`
}

// ForkPoint is where a fork was started.
type ForkPoint struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id"`
}

// derive fills the fields computed from the scanned ones.
func (c *Conversation) derive() {
	c.NeedsInput = c.OpenQuestion || c.PendingAction
	c.AgentStatus = StatusFacts{Run: c.CurrentRun, OpenQuestion: c.OpenQuestion, PendingAction: c.PendingAction}.Status()
}

var conversationCols = `c.id::text, c.agent, c.title, c.is_default, c.archived, c.created_at, c.last_activity_at,
	(SELECT t.id::text FROM tickets t WHERE t.conversation_id = c.id), c.purpose,
	(SELECT k.id::text FROM tasks k WHERE k.conversation_id = c.id),
	(SELECT wt.id::text FROM workspace_trees wt WHERE wt.conversation_id = c.id),
	EXISTS (SELECT 1 FROM messages m WHERE m.conversation_id = c.id AND ` + personQuestion + `),
	EXISTS (SELECT 1 FROM actions x JOIN runs xr ON xr.id = x.run_id
	        WHERE xr.conversation_id = c.id AND x.status = 'pending'),
	` + currentRun("c.agent", "sr.conversation_id = c.id") + `,
	(SELECT json_build_object('id', r.id, 'status', r.status, 'trigger', r.trigger, 'created_at', r.created_at,
	                          'finished_at', r.finished_at, 'summary', r.summary)
	 FROM runs r WHERE r.conversation_id = c.id ORDER BY r.created_at DESC, r.id DESC LIMIT 1),
	c.read_through, c.from_session::text, c.model, c.effort,
	CASE WHEN c.forked_from IS NOT NULL
	     THEN json_build_object('conversation_id', c.forked_from, 'message_id', c.forked_at_message) END`

// GetConversation returns one conversation.
func (s *Store) GetConversation(ctx context.Context, id string) (*Conversation, error) {
	rows, err := s.Pool.Query(ctx, `SELECT `+conversationCols+` FROM conversations c WHERE c.id = $1`, id)
	if err != nil {
		return nil, err
	}
	c, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[Conversation])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	c.derive()
	return &c, err
}

// ConversationCursor is a position in an agent's conversation list, which
// is ordered by (last_activity_at, id), newest first.
type ConversationCursor struct {
	At time.Time
	ID string
}

// String encodes the cursor for a client, which treats it as opaque.
func (c ConversationCursor) String() string {
	return base64.RawURLEncoding.EncodeToString([]byte(c.At.UTC().Format(time.RFC3339Nano) + "|" + c.ID))
}

// ParseConversationCursor decodes String's output.
func ParseConversationCursor(s string) (*ConversationCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	at, id, ok := strings.Cut(string(b), "|")
	if err != nil || !ok || id == "" {
		return nil, errors.New("not a cursor from next_before")
	}
	t, err := time.Parse(time.RFC3339Nano, at)
	if err != nil {
		return nil, fmt.Errorf("cursor time: %w", err)
	}
	return &ConversationCursor{At: t, ID: id}, nil
}

// ConversationPage is a page of an agent's conversations, newest activity
// first.
type ConversationPage struct {
	Conversations []Conversation
	HasMore       bool
	NextBefore    string
}

// ListConversations returns an agent's conversations by latest activity,
// newest first, the archived ones only when archived is set. A
// conversation's activity moving while a client pages can move it to an
// earlier page; the list never repeats one within a page walk started from
// a fixed cursor.
func (s *Store) ListConversations(ctx context.Context, agent string, archived bool, before *ConversationCursor, limit int) (*ConversationPage, error) {
	limit = clampLimit(limit, DefaultThreadLimit, MaxThreadLimit)
	var at any
	id := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if before != nil {
		at, id = before.At, before.ID
	}
	var exists bool
	if err := s.Pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM agents WHERE name = $1)`, agent).Scan(&exists); err != nil {
		return nil, err
	}
	if !exists {
		return nil, ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+conversationCols+` FROM conversations c
		WHERE c.agent = $1 AND c.archived = $2
		  AND (c.last_activity_at, c.id) < (COALESCE($3::timestamptz, 'infinity'), $4::uuid)
		ORDER BY c.last_activity_at DESC, c.id DESC LIMIT $5`, agent, archived, at, id, limit+1)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[Conversation])
	if err != nil {
		return nil, err
	}
	for i := range list {
		list[i].derive()
	}
	page := &ConversationPage{Conversations: list}
	if len(list) > limit {
		page.Conversations, page.HasMore = list[:limit], true
		last := list[limit-1]
		page.NextBefore = ConversationCursor{At: last.LastActivityAt, ID: last.ID}.String()
	}
	return page, nil
}

// NewConversation opens a conversation, with its first message when Body is
// set (which names it, unless Title does, and wakes a run when Wake is).
type NewConversation struct {
	Agent    string
	SpecSHA  string
	Title    string
	Body     string
	Author   string
	Wake     bool
	ClientID string
	// Seed starts the conversation from an imported session.
	Seed *SessionSeed
}

// SessionSeed is an imported session a conversation starts from: its transcript, built under SessionID (a session id of orbit's, so the person's own session file is never the one written to), stored in session_transcripts with the conversation.
type SessionSeed struct {
	ExternalID string
	SessionID  string
	Title      string
	Archive    []byte
	Bytes      int64
}

// CreateResult is what opening a conversation did.
type CreateResult struct {
	ConversationID string `json:"conversation_id"`
	MessageID      string `json:"message_id,omitempty"`
	RunID          string `json:"run_id,omitempty"`
}

// CreateConversation opens a conversation, its first message and the run it
// wakes in one transaction, idempotent by ClientID (receipts).
func (s *Store) CreateConversation(ctx context.Context, n NewConversation) (res CreateResult, replayed bool, err error) {
	title := strings.TrimSpace(n.Title)
	if utf8.RuneCountInString(title) > titleMax*4 {
		return res, false, fmt.Errorf("%w: title is longer than %d characters", ErrInvalid, titleMax*4)
	}
	req := map[string]any{"op": "conversation", "title": title, "body": n.Body, "author": n.Author, "wake": n.Wake}
	var fromSession, seedSession *string
	if n.Seed != nil {
		req["from_session"] = n.Seed.SessionID
		fromSession, seedSession = &n.Seed.ExternalID, &n.Seed.SessionID
		if title == "" && strings.TrimSpace(n.Body) == "" {
			title = DefaultTitle(n.Seed.Title)
		}
	}
	raw, replayed, err := s.withReceipt(ctx, n.Agent, n.ClientID, req, func(tx pgx.Tx) (json.RawMessage, error) {
		var r CreateResult
		if n.Seed != nil {
			// The same snapshot of a session is the same seed id, so a second conversation from it shares the row (every resume forks).
			if _, err := tx.Exec(ctx, `INSERT INTO session_transcripts (session_id, agent, external_session_id, archive, bytes)
				VALUES ($1, $2, $3, $4, $5) ON CONFLICT (session_id) DO NOTHING`,
				n.Seed.SessionID, n.Agent, n.Seed.ExternalID, n.Seed.Archive, n.Seed.Bytes); err != nil {
				if isForeignKeyViolation(err) {
					return nil, ErrNotFound
				}
				return nil, err
			}
		}
		err := tx.QueryRow(ctx, `INSERT INTO conversations (agent, title, from_session, seed_session) VALUES ($1, $2, $3, $4) RETURNING id::text`,
			n.Agent, title, fromSession, seedSession).Scan(&r.ConversationID)
		if isForeignKeyViolation(err) {
			return nil, ErrNotFound
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimSpace(n.Body) != "" {
			p, err := postMessage(ctx, tx, PostMessage{Agent: n.Agent, SpecSHA: n.SpecSHA, Body: n.Body, Author: n.Author,
				Wake: n.Wake, ConversationID: r.ConversationID})
			if err != nil {
				return nil, err
			}
			r.MessageID, r.RunID = p.MessageID, p.RunID
		}
		return json.Marshal(r)
	})
	if err != nil {
		return res, false, err
	}
	return res, replayed, json.Unmarshal(raw, &res)
}

// ConversationUpdate changes a conversation's title, archived flag, model or
// effort; nil leaves a field as it is, and an empty Model or Effort returns
// it to the runner's own. The caller checks a model and effort against the
// agent's runner (spec.CheckModel).
type ConversationUpdate struct {
	Title    *string
	Archived *bool
	Model    *string
	Effort   *string
}

// UpdateConversation applies u. The default conversation cannot be
// archived: schedules speak there (ErrConflict).
func (s *Store) UpdateConversation(ctx context.Context, id string, u ConversationUpdate) error {
	if u.Title != nil {
		t := strings.TrimSpace(*u.Title)
		if utf8.RuneCountInString(t) > titleMax*4 {
			return fmt.Errorf("%w: title is longer than %d characters", ErrInvalid, titleMax*4)
		}
		u.Title = &t
	}
	tag, err := s.Pool.Exec(ctx, `UPDATE conversations SET
		    title = COALESCE($2, title), archived = COALESCE($3, archived),
		    model = CASE WHEN $4::text IS NULL THEN model ELSE NULLIF($4, '') END,
		    effort = CASE WHEN $5::text IS NULL THEN effort ELSE NULLIF($5, '') END
		WHERE id = $1 AND NOT (is_default AND COALESCE($3, false))`, id, u.Title, u.Archived, u.Model, u.Effort)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		if _, err := s.GetConversation(ctx, id); err != nil {
			return err
		}
		return fmt.Errorf("the default conversation cannot be archived: %w", ErrConflict)
	}
	return nil
}
