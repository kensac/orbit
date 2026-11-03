package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"
)

// External sessions: sessions run outside orbit, imported line by line
// (the external_sessions migration). claudeimport reads and normalizes the
// lines; the store keeps them, each line's content once by digest.

// ExternalSources are the sources a session is imported from.
var ExternalSources = []string{"claude-code"}

// MaxImportSessions bounds the sessions one import carries, and
// MaxImportLines its lines: a client sends a large history in several
// imports, each one transaction.
const (
	MaxImportSessions = 200
	MaxImportLines    = 20000
)

// emptySHA256 is the SHA-256 of nothing: the content hash of a session with no lines, where the chain starts.
var emptySHA256 = func() string { s := sha256.Sum256(nil); return hex.EncodeToString(s[:]) }()

// ChainSHA256 extends a session's content hash by one line digest: sha256(prev || digest), both hex.
func ChainSHA256(prev, digest string) string {
	s := sha256.Sum256([]byte(prev + digest))
	return hex.EncodeToString(s[:])
}

// LineDigest is a stored line's digest: the hex SHA-256 of its content.
func LineDigest(content []byte) string {
	s := sha256.Sum256(content)
	return hex.EncodeToString(s[:])
}

// ExternalSession is an imported session.
type ExternalSession struct {
	ID            string     `json:"id"`
	Source        string     `json:"source"`
	SessionID     string     `json:"session_id"`
	Cwd           *string    `json:"cwd"`
	Branch        *string    `json:"branch"`
	Repo          *string    `json:"repo"`
	Title         *string    `json:"title"`
	FirstPrompt   *string    `json:"first_prompt"`
	Generation    int        `json:"generation"`
	Lines         int        `json:"lines"`
	OffsetBytes   int64      `json:"offset"`
	PrefixSHA256  string     `json:"prefix_sha256"`
	ContentSHA256 string     `json:"content_sha256"`
	FirstAt       *time.Time `json:"first_at"`
	LastAt        time.Time  `json:"last_at"`
	CreatedAt     time.Time  `json:"created_at"`
	UpdatedAt     time.Time  `json:"updated_at"`
}

const externalSessionCols = `id::text, source, session_id, cwd, branch, repo, title, first_prompt, generation, lines,
	offset_bytes, prefix_sha256, content_sha256, first_at, last_at, created_at, updated_at`

// SessionLine is one normalized line of an import, with what it says about its session.
type SessionLine struct {
	Content []byte
	Type    string
	Cwd     string
	Branch  string
	At      *time.Time
	Title   string
	Prompt  string
}

// SessionChunk is a stretch of one session's file: its lines from FromOffset (where the stored session ends, whose first bytes hash to FromPrefix) to ToOffset (whose first bytes hash to ToPrefix). Rewrite, with FromOffset 0, replaces a session whose stored prefix no longer matches the file with a new generation.
type SessionChunk struct {
	SessionID  string
	Repo       string
	FromOffset int64
	FromPrefix string
	Rewrite    bool
	ToOffset   int64
	ToPrefix   string
	Lines      []SessionLine
}

// Session import outcomes.
const (
	SessionCreated   = "created"
	SessionAppended  = "appended"
	SessionRewritten = "rewritten"
	SessionUnchanged = "unchanged"
	SessionConflict  = "conflict"
)

// SessionImportResult is what an import did with one chunk.
type SessionImportResult struct {
	SessionID string `json:"session_id"`
	// ID is the stored session's; absent for one a dry run would create.
	ID      string `json:"id,omitempty"`
	Outcome string `json:"outcome"`
	// Added counts the lines added to the session; New those whose content was not stored before (the rest are another session's, folded).
	Added      int    `json:"added"`
	New        int    `json:"new"`
	Lines      int    `json:"lines"`
	Offset     int64  `json:"offset"`
	Generation int    `json:"generation"`
	Error      string `json:"error,omitempty"`
}

// ImportSessions applies chunks in one transaction, in order; dryRun rolls it back, so it reports exactly what the import would do. A chunk that does not continue the stored session (another import moved it, or the client's view is stale) is a conflict, reported with where the session is, and writes nothing; the other chunks apply.
func (s *Store) ImportSessions(ctx context.Context, source string, chunks []SessionChunk, dryRun bool) ([]SessionImportResult, error) {
	if !slices.Contains(ExternalSources, source) {
		return nil, fmt.Errorf("%w: source %q is not one of %v", ErrInvalid, source, ExternalSources)
	}
	if len(chunks) > MaxImportSessions {
		return nil, fmt.Errorf("%w: at most %d sessions in one import", ErrInvalid, MaxImportSessions)
	}
	total := 0
	for i, c := range chunks {
		total += len(c.Lines)
		switch {
		case !IsUUID(c.SessionID):
			return nil, fmt.Errorf("%w: session %d: session_id is not a UUID", ErrInvalid, i)
		case c.FromOffset < 0 || c.ToOffset < c.FromOffset:
			return nil, fmt.Errorf("%w: session %d: to_offset must not be before from_offset", ErrInvalid, i)
		case len(c.Lines) > 0 && c.ToOffset == c.FromOffset:
			return nil, fmt.Errorf("%w: session %d: lines without bytes (to_offset equals from_offset)", ErrInvalid, i)
		case c.Rewrite && c.FromOffset != 0:
			return nil, fmt.Errorf("%w: session %d: a rewrite starts at offset 0", ErrInvalid, i)
		case len(c.ToPrefix) != 64:
			return nil, fmt.Errorf("%w: session %d: to_prefix_sha256 is not a SHA-256", ErrInvalid, i)
		}
	}
	if total > MaxImportLines {
		return nil, fmt.Errorf("%w: at most %d lines in one import", ErrInvalid, MaxImportLines)
	}
	out := make([]SessionImportResult, 0, len(chunks))
	tx, err := s.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(context.WithoutCancel(ctx)) }()
	for _, c := range chunks {
		r, err := importChunk(ctx, tx, source, c)
		if err != nil {
			return nil, fmt.Errorf("session %s: %w", c.SessionID, err)
		}
		if dryRun && r.Outcome == SessionCreated {
			r.ID = ""
		}
		out = append(out, r)
	}
	if dryRun {
		return out, nil
	}
	return out, tx.Commit(ctx)
}

func importChunk(ctx context.Context, tx pgx.Tx, source string, c SessionChunk) (SessionImportResult, error) {
	r := SessionImportResult{SessionID: c.SessionID}
	var cur struct {
		id, prefix, content string
		gen, lines          int
		offset              int64
	}
	err := tx.QueryRow(ctx, `SELECT id::text, generation, lines, offset_bytes, prefix_sha256, content_sha256
		FROM external_sessions WHERE source = $1 AND session_id = $2 FOR UPDATE`, source, c.SessionID).
		Scan(&cur.id, &cur.gen, &cur.lines, &cur.offset, &cur.prefix, &cur.content)
	exists := err == nil
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return r, err
	}
	gen, base, chain := 1, 0, emptySHA256
	switch {
	case !exists && c.FromOffset != 0:
		r.Outcome, r.Error = SessionConflict, fmt.Sprintf("orbit has no lines of this session, so an import of it starts at offset 0, not %d", c.FromOffset)
		return r, nil
	case !exists:
		r.Outcome = SessionCreated
	case c.Rewrite:
		r.Outcome, gen = SessionRewritten, cur.gen+1
	case c.FromOffset != cur.offset || c.FromPrefix != cur.prefix:
		r.ID, r.Lines, r.Offset, r.Generation = cur.id, cur.lines, cur.offset, cur.gen
		r.Outcome = SessionConflict
		r.Error = fmt.Sprintf("orbit holds this session to offset %d (prefix %s); read it again and import from there", cur.offset, short(cur.prefix))
		return r, nil
	case len(c.Lines) == 0 && c.ToOffset == cur.offset:
		r.ID, r.Lines, r.Offset, r.Generation, r.Outcome = cur.id, cur.lines, cur.offset, cur.gen, SessionUnchanged
		return r, nil
	default:
		r.Outcome, gen, base, chain = SessionAppended, cur.gen, cur.lines, cur.content
	}

	digests := make([]string, len(c.Lines))
	contents := make([]string, len(c.Lines))
	types := make([]*string, len(c.Lines))
	ats := make([]*time.Time, len(c.Lines))
	var title, prompt, cwd, branch string
	var first, last *time.Time
	for i, l := range c.Lines {
		digests[i], contents[i], ats[i] = LineDigest(l.Content), string(l.Content), l.At
		if l.Type != "" {
			types[i] = &l.Type
		}
		chain = ChainSHA256(chain, digests[i])
		if l.Title != "" {
			title = l.Title
		}
		if prompt == "" {
			prompt = l.Prompt
		}
		if l.Cwd != "" {
			cwd = l.Cwd
		}
		if l.Branch != "" {
			branch = l.Branch
		}
		if l.At != nil {
			if first == nil || l.At.Before(*first) {
				first = l.At
			}
			if last == nil || l.At.After(*last) {
				last = l.At
			}
		}
	}
	tag, err := tx.Exec(ctx, `INSERT INTO external_line_contents (digest, content, type, at)
		SELECT * FROM unnest($1::text[], $2::text[], $3::text[], $4::timestamptz[])
		ON CONFLICT (digest) DO NOTHING`, digests, contents, types, ats)
	if err != nil {
		return r, err
	}
	r.New, r.Added = int(tag.RowsAffected()), len(c.Lines)
	if exists {
		r.ID = cur.id
		_, err = tx.Exec(ctx, `UPDATE external_sessions SET
			generation = $2, lines = $3, offset_bytes = $4, prefix_sha256 = $5, content_sha256 = $6,
			cwd = COALESCE(NULLIF($7, ''), cwd), branch = COALESCE(NULLIF($8, ''), branch),
			repo = COALESCE(NULLIF($9, ''), repo), title = COALESCE(NULLIF($10, ''), title),
			first_prompt = CASE WHEN $11 THEN NULLIF($12, '') ELSE COALESCE(first_prompt, NULLIF($12, '')) END,
			first_at = CASE WHEN $11 THEN $13 ELSE LEAST(first_at, $13) END,
			last_at = GREATEST(last_at, COALESCE($14, now())), updated_at = now()
			WHERE id = $1`,
			cur.id, gen, base+len(c.Lines), c.ToOffset, c.ToPrefix, chain, cwd, branch, c.Repo, title,
			c.Rewrite, prompt, first, last)
	} else {
		err = tx.QueryRow(ctx, `INSERT INTO external_sessions (source, session_id, cwd, branch, repo, title, first_prompt,
			lines, offset_bytes, prefix_sha256, content_sha256, first_at, last_at)
			VALUES ($1, $2, NULLIF($3, ''), NULLIF($4, ''), NULLIF($5, ''), NULLIF($6, ''), NULLIF($7, ''),
			        $8, $9, $10, $11, $12, COALESCE($13, now()))
			RETURNING id::text`,
			source, c.SessionID, cwd, branch, c.Repo, title, prompt, len(c.Lines), c.ToOffset, c.ToPrefix, chain, first, last).
			Scan(&r.ID)
	}
	if err != nil {
		return r, err
	}
	if len(c.Lines) > 0 {
		if _, err := tx.Exec(ctx, `INSERT INTO external_session_lines (external_session_id, generation, n, digest)
			SELECT $1, $2, $3 + ord, d FROM unnest($4::text[]) WITH ORDINALITY AS t(d, ord)`,
			r.ID, gen, base, digests); err != nil {
			return r, err
		}
	}
	r.Lines, r.Offset, r.Generation = base+len(c.Lines), c.ToOffset, gen
	return r, nil
}

func short(h string) string {
	if len(h) > 12 {
		return h[:12]
	}
	return h
}

// SessionFilter narrows a list of imported sessions.
type SessionFilter struct {
	Source string
	Repo   string
	Before *ConversationCursor
	Limit  int
}

// MaxSessionList bounds a page of sessions: a client reads every session's import state, a page at a time.
const MaxSessionList = 1000

// SessionPage is a page of sessions, most recent first.
type SessionPage struct {
	Sessions   []ExternalSession
	HasMore    bool
	NextBefore string
}

// ListExternalSessions lists imported sessions by their newest line, newest first.
func (s *Store) ListExternalSessions(ctx context.Context, f SessionFilter) (*SessionPage, error) {
	limit := clampLimit(f.Limit, 100, MaxSessionList)
	var at any
	id := "ffffffff-ffff-ffff-ffff-ffffffffffff"
	if f.Before != nil {
		at, id = f.Before.At, f.Before.ID
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+externalSessionCols+` FROM external_sessions
		WHERE ($1 = '' OR source = $1) AND ($2 = '' OR repo = $2)
		  AND (last_at, id) < (COALESCE($3::timestamptz, 'infinity'), $4::uuid)
		ORDER BY last_at DESC, id DESC LIMIT $5`, f.Source, f.Repo, at, id, limit+1)
	if err != nil {
		return nil, err
	}
	list, err := pgx.CollectRows(rows, pgx.RowToStructByPos[ExternalSession])
	if err != nil {
		return nil, err
	}
	page := &SessionPage{Sessions: list}
	if len(list) > limit {
		page.Sessions, page.HasMore = list[:limit], true
		last := list[limit-1]
		page.NextBefore = ConversationCursor{At: last.LastAt, ID: last.ID}.String()
	}
	return page, nil
}

// GetExternalSession returns one imported session by its id, or ErrNotFound.
func (s *Store) GetExternalSession(ctx context.Context, id string) (*ExternalSession, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+externalSessionCols+` FROM external_sessions WHERE id = $1`, id)
	if err != nil {
		return nil, err
	}
	e, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[ExternalSession])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

// FindExternalSession returns an imported session by orbit's id or by its source's session id, or ErrNotFound.
func (s *Store) FindExternalSession(ctx context.Context, ref string) (*ExternalSession, error) {
	if !IsUUID(ref) {
		return nil, ErrNotFound
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+externalSessionCols+` FROM external_sessions
		WHERE id = $1::text::uuid OR session_id = $1::text ORDER BY id = $1::text::uuid DESC LIMIT 1`, ref)
	if err != nil {
		return nil, err
	}
	e, err := pgx.CollectExactlyOneRow(rows, pgx.RowToStructByPos[ExternalSession])
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &e, err
}

// StoredLine is one line of an imported session, in its stored form.
type StoredLine struct {
	N       int    `json:"n"`
	Digest  string `json:"digest"`
	Content string `json:"content"`
}

// MaxSessionLines bounds a page of lines.
const MaxSessionLines = 1000

// ExternalSessionLines returns the current generation's lines after line n (from 1), at most limit (0: every line, for building a transcript).
func (s *Store) ExternalSessionLines(ctx context.Context, id string, after, limit int) ([]StoredLine, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	var lim any
	if limit > 0 {
		lim = clampLimit(limit, 200, MaxSessionLines)
	}
	rows, err := s.Pool.Query(ctx, `SELECT l.n, l.digest, c.content
		FROM external_sessions e
		JOIN external_session_lines l ON l.external_session_id = e.id AND l.generation = e.generation
		JOIN external_line_contents c ON c.digest = l.digest
		WHERE e.id = $1 AND l.n > $2
		ORDER BY l.n LIMIT $3`, id, after, lim)
	if err != nil {
		return nil, err
	}
	lines, err := pgx.CollectRows(rows, pgx.RowToStructByPos[StoredLine])
	if err != nil {
		return nil, err
	}
	if len(lines) == 0 {
		if _, err := s.GetExternalSession(ctx, id); err != nil {
			return nil, err
		}
	}
	return lines, nil
}
