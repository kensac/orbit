package store

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Board states, one per conversation, derived by BoardState from facts the
// store reads (docs/api.md, The board). The order is precedence: the first
// that holds is the state.
const (
	BoardNeedsYou   = "needs_you"   // an open approval, question, plan or task decision
	BoardWorking    = "working"     // a run is running, or queued and due
	BoardFailed     = "failed"      // the newest run failed, and that is news
	BoardDoneUnread = "done_unread" // a run finished after the person last read it
	BoardRead       = "read"        // nothing waiting, read, active within the idle window
	BoardIdle       = "idle"        // as read, quiet past the idle window
)

// DefaultBoardIdle is how long a conversation with nothing waiting stays on
// the board after its last activity, unless orbit.yaml's board.idle says.
const DefaultBoardIdle = 30 * time.Minute

// BoardCounts is what a conversation holds open.
type BoardCounts struct {
	Approvals int `json:"approvals"` // pending actions of its runs (not task decisions)
	Questions int `json:"questions"` // its open questions to the person
	Decisions int `json:"decisions"` // pending task:decision actions and a ticket plan to approve
	Tasks     int `json:"tasks"`     // tasks it delegated that are not over
	Unread    int `json:"unread"`    // runs finished after read_through
}

// BoardTask is the task a delegated task's conversation works on.
type BoardTask struct {
	ID                   string `json:"id"`
	Status               string `json:"status"`
	ParentConversationID string `json:"parent_conversation_id"`
}

// BoardPreview is the newest prose in a conversation: a message, or a
// finished run's summary (its report when it has none), cut to one line.
type BoardPreview struct {
	From string    `json:"from"` // person or agent
	Text string    `json:"text"`
	At   time.Time `json:"at"`
}

// BoardFacts is what BoardState decides from.
type BoardFacts struct {
	Working        bool
	Counts         BoardCounts
	Task           *BoardTask
	LastRun        *RunSummary
	ReadThrough    *time.Time
	LastActivityAt time.Time
}

// BoardState derives a conversation's state from its facts. Code, so the
// board, the sidebar and a notification can never disagree about it.
func BoardState(f BoardFacts, now time.Time, idle time.Duration) string {
	task := ""
	if f.Task != nil {
		task = f.Task.Status
	}
	recent := now.Sub(f.LastActivityAt) < idle
	unread := func(t *time.Time) bool { return t != nil && (f.ReadThrough == nil || t.After(*f.ReadThrough)) }
	switch {
	case f.Counts.Approvals+f.Counts.Questions+f.Counts.Decisions > 0, task == "blocked":
		return BoardNeedsYou
	case f.Working, task == "working", task == "needs_input":
		return BoardWorking
	case f.LastRun != nil && f.LastRun.Status == "failed" && (recent || unread(f.LastRun.FinishedAt)):
		return BoardFailed
	case f.Counts.Unread > 0:
		return BoardDoneUnread
	case recent:
		return BoardRead
	}
	return BoardIdle
}

// BoardRow is one open conversation on the board.
type BoardRow struct {
	ConversationID string        `json:"conversation_id"`
	Agent          string        `json:"agent"`
	Title          string        `json:"title"`
	IsDefault      bool          `json:"is_default"`
	TicketID       *string       `json:"ticket_id"`
	Task           *BoardTask    `json:"task"`
	State          string        `json:"state"`
	Preview        *BoardPreview `json:"preview"`
	LastActivityAt time.Time     `json:"last_activity_at"`
	AgeSeconds     int64         `json:"age_seconds"`
	ReadThrough    *time.Time    `json:"read_through"`
	LastRun        *RunSummary   `json:"last_run"`
	Counts         BoardCounts   `json:"counts"`
}

// Board is every open conversation's state at AsOf, newest activity first.
type Board struct {
	Rows []BoardRow
	AsOf time.Time
}

// previewMax is a preview's length in characters: a line under a sidebar
// title.
const previewMax = 140

// boardQuery reads every open conversation's facts in one statement. A
// task conversation's questions are to the delegating agent, so they are
// not the person's.
const boardQuery = `
SELECT c.id::text, c.agent, c.title, c.is_default, c.last_activity_at, c.read_through,
       (SELECT t.id::text FROM tickets t WHERE t.conversation_id = c.id),
       k.id::text, k.status, k.parent_conversation_id::text,
       (SELECT json_build_object('id', r.id, 'status', r.status, 'trigger', r.trigger, 'created_at', r.created_at,
                                 'finished_at', r.finished_at, 'summary', r.summary)
        FROM runs r WHERE r.conversation_id = c.id ORDER BY r.created_at DESC, r.id DESC LIMIT 1),
       EXISTS (SELECT 1 FROM runs w WHERE w.conversation_id = c.id
               AND (w.status = 'running' OR (w.status = 'queued' AND (w.run_after IS NULL OR w.run_after <= now())))),
       (SELECT count(*) FROM actions x JOIN runs xr ON xr.id = x.run_id
        WHERE xr.conversation_id = c.id AND x.status = 'pending' AND x.type <> 'task:decision'),
       CASE WHEN k.id IS NULL THEN (SELECT count(*) FROM messages m WHERE m.conversation_id = c.id AND ` + openQuestion + `) ELSE 0 END,
       (SELECT count(*) FROM actions x JOIN runs xr ON xr.id = x.run_id
        WHERE xr.conversation_id = c.id AND x.status = 'pending' AND x.type = 'task:decision')
       + (SELECT count(*) FROM tickets t WHERE t.conversation_id = c.id AND t.status = 'planned'),
       (SELECT count(*) FROM tasks d WHERE d.parent_conversation_id = c.id AND d.status NOT IN ('done', 'failed', 'cancelled')),
       (SELECT count(*) FROM runs u WHERE u.conversation_id = c.id AND u.status IN ('succeeded', 'skipped', 'failed')
        AND u.finished_at > COALESCE(c.read_through, '-infinity')),
       (SELECT json_build_object('from', p.src, 'text', p.body, 'at', p.at) FROM (
            SELECT m.created_at AS at, CASE WHEN m.direction = 'to_agent' AND m.author NOT LIKE 'agent:%' THEN 'person' ELSE 'agent' END AS src,
                   left(m.body, 600) AS body
            FROM messages m WHERE m.conversation_id = c.id AND btrim(m.body) <> ''
            UNION ALL
            SELECT r.finished_at, 'agent', left(COALESCE(NULLIF(btrim(r.summary), ''), r.report), 600)
            FROM runs r WHERE r.conversation_id = c.id AND r.finished_at IS NOT NULL
              AND btrim(COALESCE(NULLIF(btrim(r.summary), ''), r.report, '')) <> ''
            ORDER BY at DESC LIMIT 1) p),
       now()
FROM conversations c
LEFT JOIN tasks k ON k.conversation_id = c.id
WHERE NOT c.archived AND c.purpose IS NULL
ORDER BY c.last_activity_at DESC, c.id DESC`

// ReadBoard returns every open conversation (not archived, not a learning
// conversation) with its derived state, newest activity first, at most
// limit rows. Rows whose state is idle are left out unless withIdle.
func (s *Store) ReadBoard(ctx context.Context, idle time.Duration, withIdle bool, limit int) (*Board, error) {
	if idle <= 0 {
		idle = DefaultBoardIdle
	}
	limit = clampLimit(limit, 200, 500)
	rows, err := s.Pool.Query(ctx, boardQuery)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	b := &Board{Rows: []BoardRow{}}
	for rows.Next() {
		var r BoardRow
		var f BoardFacts
		var taskID, taskStatus, taskParent *string
		var lastRun, preview []byte
		if err := rows.Scan(&r.ConversationID, &r.Agent, &r.Title, &r.IsDefault, &r.LastActivityAt, &r.ReadThrough,
			&r.TicketID, &taskID, &taskStatus, &taskParent, &lastRun, &f.Working,
			&r.Counts.Approvals, &r.Counts.Questions, &r.Counts.Decisions, &r.Counts.Tasks, &r.Counts.Unread,
			&preview, &b.AsOf); err != nil {
			return nil, err
		}
		if taskID != nil {
			r.Task = &BoardTask{ID: *taskID, Status: *taskStatus, ParentConversationID: *taskParent}
		}
		if lastRun != nil {
			if err := json.Unmarshal(lastRun, &r.LastRun); err != nil {
				return nil, fmt.Errorf("board: last run: %w", err)
			}
		}
		if preview != nil {
			if err := json.Unmarshal(preview, &r.Preview); err != nil {
				return nil, fmt.Errorf("board: preview: %w", err)
			}
			r.Preview.Text = PreviewText(r.Preview.Text)
		}
		f.Counts, f.Task, f.LastRun, f.ReadThrough, f.LastActivityAt = r.Counts, r.Task, r.LastRun, r.ReadThrough, r.LastActivityAt
		r.State = BoardState(f, b.AsOf, idle)
		r.AgeSeconds = int64(b.AsOf.Sub(r.LastActivityAt) / time.Second)
		if (r.State != BoardIdle || withIdle) && len(b.Rows) < limit {
			b.Rows = append(b.Rows, r)
		}
	}
	return b, rows.Err()
}

// PreviewText is prose as one line of at most previewMax characters:
// whitespace runs folded, leading Markdown markers of each line dropped, cut
// at a word.
func PreviewText(s string) string {
	var lines []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimLeft(strings.TrimSpace(l), "#>*-+` \t"); l != "" {
			lines = append(lines, l)
		}
	}
	line := strings.Join(strings.FieldsFunc(strings.Join(lines, " "), unicode.IsSpace), " ")
	if utf8.RuneCountInString(line) <= previewMax {
		return line
	}
	cut := string([]rune(line)[:previewMax-3])
	if i := strings.LastIndexByte(cut, ' '); i > previewMax/2 {
		cut = cut[:i]
	}
	return strings.TrimRight(cut, " .,;:") + "..."
}

// ---------------------------------------------------------------- read points

// ReadPoint returns how far the person has read a conversation (nil before
// they first did) and the database's clock, which is what a mark made from
// this read should carry: everything that finished before it was on the
// page the caller is about to read.
func (s *Store) ReadPoint(ctx context.Context, conversationID string) (*time.Time, time.Time, error) {
	var through *time.Time
	var now time.Time
	err := s.Pool.QueryRow(ctx, `SELECT read_through, now() FROM conversations WHERE id = $1`, conversationID).Scan(&through, &now)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, now, ErrNotFound
	}
	return through, now, err
}

// MarkRead moves a conversation's read point to through (now when nil),
// never past now and never back, and returns where it is.
func (s *Store) MarkRead(ctx context.Context, conversationID string, through *time.Time) (time.Time, error) {
	var at time.Time
	err := s.Pool.QueryRow(ctx, `UPDATE conversations
		SET read_through = GREATEST(read_through, LEAST(COALESCE($2::timestamptz, now()), now()))
		WHERE id = $1 RETURNING read_through`, conversationID, through).Scan(&at)
	if errors.Is(err, pgx.ErrNoRows) {
		return at, ErrNotFound
	}
	return at, err
}

// ---------------------------------------------------------------- changes

// Change is one record of the change feed (GET /events): what kind of thing
// changed, in which conversation, for which run.
type Change struct {
	Kind           string  `json:"kind"`
	ConversationID *string `json:"conversation"`
	RunID          *string `json:"run"`
	Cursor         string  `json:"cursor"`
}

// ChangeCursor is a position in the feed: (xid, id) of the last record
// read. The zero cursor is before every record.
type ChangeCursor struct {
	XID uint64
	ID  int64
}

// String encodes the cursor for a client, which treats it as opaque.
func (c ChangeCursor) String() string {
	return strconv.FormatUint(c.XID, 10) + "-" + strconv.FormatInt(c.ID, 10)
}

// ParseChangeCursor decodes String's output.
func ParseChangeCursor(s string) (ChangeCursor, error) {
	x, i, ok := strings.Cut(s, "-")
	xid, err1 := strconv.ParseUint(x, 10, 64)
	id, err2 := strconv.ParseInt(i, 10, 64)
	if !ok || err1 != nil || err2 != nil || id < 0 {
		return ChangeCursor{}, errors.New("not a change cursor")
	}
	return ChangeCursor{XID: xid, ID: id}, nil
}

// settled is true of a changes row (alias ch) whose transaction is older
// than every transaction still open, so no row will ever appear before it
// in (xid, id) order.
const settled = `ch.xid < pg_snapshot_xmin(pg_current_snapshot())`

// ChangesHead returns the cursor of the newest settled record: a client
// starting there misses nothing written after it.
func (s *Store) ChangesHead(ctx context.Context) (ChangeCursor, error) {
	var c ChangeCursor
	var xid *string
	var id *int64
	err := s.Pool.QueryRow(ctx, `SELECT ch.xid::text, ch.id FROM changes ch WHERE `+settled+`
		ORDER BY ch.xid DESC, ch.id DESC LIMIT 1`).Scan(&xid, &id)
	if errors.Is(err, pgx.ErrNoRows) {
		return c, nil
	}
	if err != nil {
		return c, err
	}
	c.XID, _ = strconv.ParseUint(*xid, 10, 64)
	c.ID = *id
	return c, nil
}

// ChangesAfter returns up to limit settled records after the cursor, in
// order.
func (s *Store) ChangesAfter(ctx context.Context, after ChangeCursor, limit int) ([]Change, error) {
	rows, err := s.Pool.Query(ctx, `SELECT ch.xid::text, ch.id, ch.kind, ch.conversation_id::text, ch.run_id::text
		FROM changes ch
		WHERE (ch.xid, ch.id) > ($1::text::xid8, $2::bigint) AND `+settled+`
		ORDER BY ch.xid, ch.id LIMIT $3`, strconv.FormatUint(after.XID, 10), after.ID, limit)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(row pgx.CollectableRow) (Change, error) {
		var c Change
		var xid string
		var id int64
		if err := row.Scan(&xid, &id, &c.Kind, &c.ConversationID, &c.RunID); err != nil {
			return c, err
		}
		x, err := strconv.ParseUint(xid, 10, 64)
		c.Cursor = ChangeCursor{XID: x, ID: id}.String()
		return c, err
	})
}

// ChangesBehind counts the settled records after the cursor, up to most+1,
// so a stream can tell a client too far behind to replay from one it can
// catch up.
func (s *Store) ChangesBehind(ctx context.Context, after ChangeCursor, most int) (int, error) {
	var n int
	err := s.Pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM changes ch
		WHERE (ch.xid, ch.id) > ($1::text::xid8, $2::bigint) AND `+settled+` LIMIT $3) x`,
		strconv.FormatUint(after.XID, 10), after.ID, most+1).Scan(&n)
	return n, err
}
