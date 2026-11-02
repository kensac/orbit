package store

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
)

// Memory entry scopes: who an entry is about and which agents read it.
const (
	// ScopeProfile is the person: every agent reads it.
	ScopeProfile = "profile"
	// ScopeTopic is a domain (finance, homelab): agents that read the topic.
	ScopeTopic = "topic"
	// ScopeAgent is one agent's own.
	ScopeAgent = "agent"
)

// EntryTypes are Claude Code's memory types, kept so an imported memory keeps its meaning: user (who the person is), feedback (how they want work done: corrections and preferences), project (facts about their world and work), reference (where to find things).
var EntryTypes = []string{"user", "feedback", "project", "reference"}

// Entry sources: who wrote a version.
const (
	SourcePerson  = "person"
	SourceLearned = "learned"
	SourceImport  = "import"
)

// Bounds on one entry, in bytes. An entry is one fact with its reason, not a document: the largest of the owner's Claude Code memories is 8 KB, so a body of twice that holds any of them, and the line fields stay short because the index shows them on every run.
const (
	entryTitleMax       = 200
	entryDescriptionMax = 400
	entryBodyMax        = 16 << 10
	entryNoteMax        = 2 << 10
	// entryReasonMax bounds why and how_to_apply each.
	entryReasonMax = 4 << 10
	// MaxImportEntries bounds one import request: the owner's largest memory directory has under 50 entries.
	MaxImportEntries = 500
)

// EntryKeyRE is an entry's key: a slug, unique within its scope.
var EntryKeyRE = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,79}$`)

// ownerRE is a topic or agent name, as spec names are.
var ownerRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// EntryFields are what a version of an entry says.
type EntryFields struct {
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Why         string `json:"why"`
	HowToApply  string `json:"how_to_apply"`
}

// Provenance is who wrote a version and why.
type Provenance struct {
	Source         string
	RunID          string
	ConversationID string
	Author         string
	Note           string
}

// Entry is one version of a memory entry: the current one where a single entry is returned, any of them in a history.
type Entry struct {
	ID      string `json:"id"`
	Scope   string `json:"scope"`
	Topic   string `json:"topic,omitempty"`
	Agent   string `json:"agent,omitempty"`
	Key     string `json:"key"`
	Version int    `json:"version"`
	Status  string `json:"status"`
	EntryFields
	SupersededBy         *string   `json:"superseded_by"`
	Source               string    `json:"source"`
	SourceRunID          *string   `json:"source_run_id"`
	SourceConversationID *string   `json:"source_conversation_id"`
	Author               *string   `json:"author"`
	Note                 string    `json:"note"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// EntryRef names an entry's place: its scope and, for a topic or an agent, the owner's name.
type EntryRef struct {
	Scope string
	Owner string
}

// NewEntry is an entry to create (or, in an import, to match by key).
type NewEntry struct {
	EntryRef
	// Key is derived from the title when empty.
	Key string
	EntryFields
}

const entryCols = `e.id::text, e.scope, e.owner, e.key, v.version, v.status, v.type, v.title, v.description,
	v.body, v.why, v.how_to_apply, v.superseded_by::text, v.source, v.source_run::text,
	v.source_conversation::text, v.author, v.note, e.created_at, v.created_at`

func scanEntry(row pgx.Row) (*Entry, error) {
	var e Entry
	var owner string
	err := row.Scan(&e.ID, &e.Scope, &owner, &e.Key, &e.Version, &e.Status, &e.Type, &e.Title, &e.Description,
		&e.Body, &e.Why, &e.HowToApply, &e.SupersededBy, &e.Source, &e.SourceRunID,
		&e.SourceConversationID, &e.Author, &e.Note, &e.CreatedAt, &e.UpdatedAt)
	if err != nil {
		return nil, err
	}
	switch e.Scope {
	case ScopeTopic:
		e.Topic = owner
	case ScopeAgent:
		e.Agent = owner
	}
	return &e, nil
}

func collectEntries(rows pgx.Rows) ([]Entry, error) {
	defer rows.Close()
	var out []Entry
	for rows.Next() {
		e, err := scanEntry(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *e)
	}
	return out, rows.Err()
}

// validRef checks a scope and its owner.
func validRef(r EntryRef) error {
	switch r.Scope {
	case ScopeProfile:
		if r.Owner != "" {
			return fmt.Errorf("%w: the profile scope has no topic or agent", ErrInvalid)
		}
	case ScopeTopic, ScopeAgent:
		if !ownerRE.MatchString(r.Owner) {
			return fmt.Errorf("%w: %s name %q must match %s", ErrInvalid, r.Scope, r.Owner, ownerRE)
		}
	default:
		return fmt.Errorf("%w: scope must be profile, topic or agent", ErrInvalid)
	}
	return nil
}

// validFields checks a version's content and trims it.
func validFields(f *EntryFields) error {
	f.Title = strings.TrimSpace(f.Title)
	f.Description = strings.TrimSpace(f.Description)
	f.Body = strings.TrimSpace(f.Body)
	f.Why = strings.TrimSpace(f.Why)
	f.HowToApply = strings.TrimSpace(f.HowToApply)
	switch {
	case !slices.Contains(EntryTypes, f.Type):
		return fmt.Errorf("%w: type must be one of %s", ErrInvalid, strings.Join(EntryTypes, ", "))
	case f.Title == "" || len(f.Title) > entryTitleMax || strings.ContainsAny(f.Title, "\r\n"):
		return fmt.Errorf("%w: title is required, one line of at most %d bytes", ErrInvalid, entryTitleMax)
	case len(f.Description) > entryDescriptionMax || strings.ContainsAny(f.Description, "\r\n"):
		return fmt.Errorf("%w: description is one line of at most %d bytes", ErrInvalid, entryDescriptionMax)
	case f.Body == "" || len(f.Body) > entryBodyMax:
		return fmt.Errorf("%w: body is required, at most %d bytes", ErrInvalid, entryBodyMax)
	case len(f.Why) > entryReasonMax || len(f.HowToApply) > entryReasonMax:
		return fmt.Errorf("%w: why and how_to_apply are at most %d bytes each", ErrInvalid, entryReasonMax)
	}
	for _, s := range []string{f.Title, f.Description, f.Body, f.Why, f.HowToApply} {
		if !utf8.ValidString(s) {
			return fmt.Errorf("%w: text must be UTF-8", ErrInvalid)
		}
	}
	return nil
}

func validProvenance(p Provenance) error {
	if !slices.Contains([]string{SourcePerson, SourceLearned, SourceImport}, p.Source) {
		return fmt.Errorf("%w: unknown source %q", ErrInvalid, p.Source)
	}
	if len(p.Note) > entryNoteMax {
		return fmt.Errorf("%w: note is at most %d bytes", ErrInvalid, entryNoteMax)
	}
	return nil
}

// EntryKey is the key a title gives: lower case, every run of other characters one dash, cut to fit EntryKeyRE.
func EntryKey(title string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(title) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			dash = false
		} else if !dash && b.Len() > 0 {
			b.WriteByte('-')
			dash = true
		}
	}
	k := strings.Trim(b.String(), "-")
	if len(k) > 80 {
		k = strings.TrimRight(k[:80], "-")
	}
	if k == "" {
		k = "entry"
	}
	return k
}

// CreateEntry adds an entry at version 1. A key already used in the scope (by any entry, retired ones too) is ErrConflict; with no key given, the title's is used, numbered when taken.
func (s *Store) CreateEntry(ctx context.Context, n NewEntry, p Provenance) (*Entry, error) {
	var e *Entry
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		e, err = createEntry(ctx, tx, n, p)
		return err
	})
	return e, err
}

func createEntry(ctx context.Context, tx pgx.Tx, n NewEntry, p Provenance) (*Entry, error) {
	if err := validRef(n.EntryRef); err != nil {
		return nil, err
	}
	if err := validFields(&n.EntryFields); err != nil {
		return nil, err
	}
	if err := validProvenance(p); err != nil {
		return nil, err
	}
	derived := n.Key == ""
	if derived {
		n.Key = EntryKey(n.Title)
	}
	if !EntryKeyRE.MatchString(n.Key) {
		return nil, fmt.Errorf("%w: key %q must match %s", ErrInvalid, n.Key, EntryKeyRE)
	}
	key := n.Key
	var id string
	for i := 2; ; i++ {
		tag, err := tx.Exec(ctx, `INSERT INTO memory_entries (scope, owner, key) VALUES ($1, $2, $3)
			ON CONFLICT (scope, owner, key) DO NOTHING`, n.Scope, n.Owner, key)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() == 1 {
			break
		}
		if !derived {
			return nil, fmt.Errorf("an entry with key %q exists in this scope: %w", key, ErrConflict)
		}
		suffix := fmt.Sprintf("-%d", i)
		key = strings.TrimRight(n.Key[:min(len(n.Key), 80-len(suffix))], "-") + suffix
	}
	if err := tx.QueryRow(ctx, `SELECT id::text FROM memory_entries WHERE scope = $1 AND owner = $2 AND key = $3`,
		n.Scope, n.Owner, key).Scan(&id); err != nil {
		return nil, err
	}
	if err := insertVersion(ctx, tx, id, 1, "active", n.EntryFields, nil, p); err != nil {
		return nil, err
	}
	return entryAt(ctx, tx, id, 0)
}

func insertVersion(ctx context.Context, tx pgx.Tx, id string, version int, status string, f EntryFields, supersededBy *string, p Provenance) error {
	_, err := tx.Exec(ctx, `INSERT INTO memory_entry_versions
		    (entry_id, version, status, type, title, description, body, why, how_to_apply, superseded_by,
		     source, source_run, source_conversation, author, note)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, NULLIF($12, '')::uuid, NULLIF($13, '')::uuid, NULLIF($14, ''), $15)`,
		id, version, status, f.Type, f.Title, f.Description, f.Body, f.Why, f.HowToApply, supersededBy,
		p.Source, p.RunID, p.ConversationID, p.Author, p.Note)
	if isForeignKeyViolation(err) {
		return fmt.Errorf("%w: superseded_by, the run or the conversation does not exist", ErrInvalid)
	}
	return err
}

type queryer interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

// entryAt reads an entry at a version, 0 meaning its current one.
func entryAt(ctx context.Context, q queryer, id string, version int) (*Entry, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	e, err := scanEntry(q.QueryRow(ctx, `SELECT `+entryCols+`
		FROM memory_entries e JOIN memory_entry_versions v ON v.entry_id = e.id
		WHERE e.id = $1 AND v.version = CASE WHEN $2 > 0 THEN $2 ELSE e.current_version END`, id, version))
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return e, err
}

// GetEntry returns an entry's current version and every version, newest first.
func (s *Store) GetEntry(ctx context.Context, id string) (*Entry, []Entry, error) {
	e, err := entryAt(ctx, s.Pool, id, 0)
	if err != nil {
		return nil, nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+entryCols+`
		FROM memory_entries e JOIN memory_entry_versions v ON v.entry_id = e.id
		WHERE e.id = $1 ORDER BY v.version DESC`, id)
	if err != nil {
		return nil, nil, err
	}
	versions, err := collectEntries(rows)
	return e, versions, err
}

// EntryFilter selects entries by their current version.
type EntryFilter struct {
	// Scope and Owner narrow to one place; Scope alone to every owner.
	Scope string
	Owner string
	Type  string
	// Status is active (the default), retired or all.
	Status string
	// Query matches the key, title, description, body, why and how_to_apply, case-insensitively, as a substring.
	Query string
	// RunID selects entries a run wrote a version of.
	RunID string
	Limit int
}

// MaxEntryLimit bounds one listing; DefaultEntryLimit is a listing's size when none is asked for.
const (
	DefaultEntryLimit = 200
	MaxEntryLimit     = 1000
)

// ListEntries returns the entries that match f, by scope (profile, topics, agents), owner, then most recently changed.
func (s *Store) ListEntries(ctx context.Context, f EntryFilter) ([]Entry, error) {
	f.Limit = clampLimit(f.Limit, DefaultEntryLimit, MaxEntryLimit)
	status := f.Status
	switch status {
	case "":
		status = "active"
	case "active", "retired":
	case "all":
		status = ""
	default:
		return nil, fmt.Errorf("%w: status must be active, retired or all", ErrInvalid)
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+entryCols+`
		FROM memory_entries e JOIN memory_entry_versions v ON v.entry_id = e.id AND v.version = e.current_version
		WHERE ($1 = '' OR e.scope = $1) AND ($2 = '' OR e.owner = $2) AND ($3 = '' OR v.type = $3)
		  AND ($4 = '' OR v.status = $4)
		  AND ($7 = '' OR EXISTS (SELECT 1 FROM memory_entry_versions w WHERE w.entry_id = e.id AND w.source_run = NULLIF($7, '')::uuid))
		  AND ($5 = '' OR strpos(lower(e.key || ' ' || v.title || ' ' || v.description || ' ' || v.body || ' ' || v.why || ' ' || v.how_to_apply), lower($5)) > 0)
		ORDER BY array_position(ARRAY['profile', 'topic', 'agent'], e.scope), e.owner, v.created_at DESC, e.key
		LIMIT $6`, f.Scope, f.Owner, f.Type, status, f.Query, f.Limit, f.RunID)
	if err != nil {
		return nil, err
	}
	return collectEntries(rows)
}

// EntriesFor returns the active entries an agent reads: the profile, the topics named, and its own, in that order.
func (s *Store) EntriesFor(ctx context.Context, agent string, topics []string) ([]Entry, error) {
	if topics == nil {
		topics = []string{}
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+entryCols+`
		FROM memory_entries e JOIN memory_entry_versions v ON v.entry_id = e.id AND v.version = e.current_version
		WHERE v.status = 'active'
		  AND (e.scope = 'profile' OR (e.scope = 'topic' AND e.owner = ANY($2)) OR (e.scope = 'agent' AND e.owner = $1))
		ORDER BY array_position(ARRAY['profile', 'topic', 'agent'], e.scope), e.owner, v.created_at DESC, e.key`, agent, topics)
	if err != nil {
		return nil, err
	}
	return collectEntries(rows)
}

// EntryEdit changes an entry; a nil field keeps its value. Status active restores a retired entry (clearing superseded_by).
type EntryEdit struct {
	Type        *string `json:"type"`
	Title       *string `json:"title"`
	Description *string `json:"description"`
	Body        *string `json:"body"`
	Why         *string `json:"why"`
	HowToApply  *string `json:"how_to_apply"`
	Status      *string `json:"status"`
}

// UpdateEntry writes the next version of an entry. expect, when above 0, is the version the caller read; another current version is ErrConflict, so two edits never silently overwrite each other. An edit that changes nothing writes nothing and returns the current version.
func (s *Store) UpdateEntry(ctx context.Context, id string, expect int, edit EntryEdit, p Provenance) (*Entry, error) {
	var out *Entry
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = updateEntry(ctx, tx, id, expect, edit, p)
		return err
	})
	return out, err
}

func updateEntry(ctx context.Context, tx pgx.Tx, id string, expect int, edit EntryEdit, p Provenance) (*Entry, error) {
	cur, err := lockEntry(ctx, tx, id, expect)
	if err != nil {
		return nil, err
	}
	f := cur.EntryFields
	set := func(dst *string, v *string) {
		if v != nil {
			*dst = *v
		}
	}
	set(&f.Type, edit.Type)
	set(&f.Title, edit.Title)
	set(&f.Description, edit.Description)
	set(&f.Body, edit.Body)
	set(&f.Why, edit.Why)
	set(&f.HowToApply, edit.HowToApply)
	if err := validFields(&f); err != nil {
		return nil, err
	}
	if err := validProvenance(p); err != nil {
		return nil, err
	}
	status, superseded := cur.Status, cur.SupersededBy
	if edit.Status != nil {
		switch *edit.Status {
		case "active":
			status, superseded = "active", nil
		case "retired":
			status = "retired"
		default:
			return nil, fmt.Errorf("%w: status must be active or retired", ErrInvalid)
		}
	}
	if f == cur.EntryFields && status == cur.Status {
		return cur, nil
	}
	return writeNext(ctx, tx, cur, status, f, superseded, p)
}

// RetireEntry ends an entry: the next version is retired, superseded by another entry when supersededBy names one (it must be active, and not the entry itself). The entry and every version stay readable.
func (s *Store) RetireEntry(ctx context.Context, id string, expect int, supersededBy *string, p Provenance) (*Entry, error) {
	var out *Entry
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = retireEntry(ctx, tx, id, expect, supersededBy, p)
		return err
	})
	return out, err
}

func retireEntry(ctx context.Context, tx pgx.Tx, id string, expect int, supersededBy *string, p Provenance) (*Entry, error) {
	if err := validProvenance(p); err != nil {
		return nil, err
	}
	cur, err := lockEntry(ctx, tx, id, expect)
	if err != nil {
		return nil, err
	}
	if supersededBy != nil {
		if *supersededBy == id {
			return nil, fmt.Errorf("%w: an entry cannot supersede itself", ErrInvalid)
		}
		by, err := entryAt(ctx, tx, *supersededBy, 0)
		if errors.Is(err, ErrNotFound) {
			return nil, fmt.Errorf("%w: superseded_by %s does not exist", ErrInvalid, *supersededBy)
		}
		if err != nil {
			return nil, err
		}
		if by.Status != "active" {
			return nil, fmt.Errorf("%w: superseded_by %s is retired", ErrInvalid, *supersededBy)
		}
	}
	if cur.Status == "retired" && equalPtr(cur.SupersededBy, supersededBy) {
		return cur, nil
	}
	return writeNext(ctx, tx, cur, "retired", cur.EntryFields, supersededBy, p)
}

func equalPtr(a, b *string) bool { return (a == nil) == (b == nil) && (a == nil || *a == *b) }

// lockEntry reads an entry's current version for update and checks the version the caller expects.
func lockEntry(ctx context.Context, tx pgx.Tx, id string, expect int) (*Entry, error) {
	if !IsUUID(id) {
		return nil, ErrNotFound
	}
	var current int
	err := tx.QueryRow(ctx, `SELECT current_version FROM memory_entries WHERE id = $1 FOR UPDATE`, id).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expect > 0 && expect != current {
		return nil, fmt.Errorf("the entry is at version %d, not %d: %w", current, expect, ErrConflict)
	}
	return entryAt(ctx, tx, id, current)
}

func writeNext(ctx context.Context, tx pgx.Tx, cur *Entry, status string, f EntryFields, supersededBy *string, p Provenance) (*Entry, error) {
	next := cur.Version + 1
	if err := insertVersion(ctx, tx, cur.ID, next, status, f, supersededBy, p); err != nil {
		return nil, err
	}
	if _, err := tx.Exec(ctx, `UPDATE memory_entries SET current_version = $2 WHERE id = $1`, cur.ID, next); err != nil {
		return nil, err
	}
	return entryAt(ctx, tx, cur.ID, next)
}

// ImportResult is what an import did, or would do, with one entry.
type ImportResult struct {
	Scope string `json:"scope"`
	Topic string `json:"topic,omitempty"`
	Agent string `json:"agent,omitempty"`
	Key   string `json:"key"`
	// Outcome is created, updated (a new version) or unchanged.
	Outcome string `json:"outcome"`
	ID      string `json:"id,omitempty"`
	Version int    `json:"version,omitempty"`
}

var errDryRun = errors.New("dry run")

// ImportEntries matches each entry by scope and key: a new key creates an entry, a known one gets a new version when its content differs or it was retired, and an identical one is left alone, so an import can be run again. All of it is one transaction; dryRun reports the outcomes and rolls back.
func (s *Store) ImportEntries(ctx context.Context, entries []NewEntry, p Provenance, dryRun bool) ([]ImportResult, error) {
	if len(entries) > MaxImportEntries {
		return nil, fmt.Errorf("%w: at most %d entries per import", ErrInvalid, MaxImportEntries)
	}
	seen := map[string]bool{}
	for i := range entries {
		n := &entries[i]
		if n.Key == "" {
			n.Key = EntryKey(n.Title)
		}
		id := n.Scope + "\x00" + n.Owner + "\x00" + n.Key
		if seen[id] {
			return nil, fmt.Errorf("%w: key %q appears twice in scope %s %s", ErrInvalid, n.Key, n.Scope, n.Owner)
		}
		seen[id] = true
	}
	var results []ImportResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for i, n := range entries {
			r := ImportResult{Scope: n.Scope, Key: n.Key}
			switch n.Scope {
			case ScopeTopic:
				r.Topic = n.Owner
			case ScopeAgent:
				r.Agent = n.Owner
			}
			var id string
			err := tx.QueryRow(ctx, `SELECT id::text FROM memory_entries WHERE scope = $1 AND owner = $2 AND key = $3`,
				n.Scope, n.Owner, n.Key).Scan(&id)
			var e *Entry
			switch {
			case errors.Is(err, pgx.ErrNoRows):
				r.Outcome = "created"
				e, err = createEntry(ctx, tx, n, p)
			case err != nil:
				return err
			default:
				var before *Entry
				if before, err = entryAt(ctx, tx, id, 0); err != nil {
					return err
				}
				active := "active"
				e, err = updateEntry(ctx, tx, id, 0, EntryEdit{Type: &n.Type, Title: &n.Title, Description: &n.Description,
					Body: &n.Body, Why: &n.Why, HowToApply: &n.HowToApply, Status: &active}, p)
				r.Outcome = "unchanged"
				if e != nil && e.Version != before.Version {
					r.Outcome = "updated"
				}
			}
			if err != nil {
				return fmt.Errorf("entry %d (%s): %w", i, n.Key, err)
			}
			r.ID, r.Version = e.ID, e.Version
			if dryRun && r.Outcome == "created" {
				// Rolled back: the id would not exist.
				r.ID = ""
			}
			results = append(results, r)
		}
		if dryRun {
			return errDryRun
		}
		return nil
	})
	if errors.Is(err, errDryRun) {
		err = nil
	}
	return results, err
}

// IsUUID reports whether s is a UUID in its canonical text form, so an id that cannot name a row is caught before Postgres's uuid cast fails on it.
func IsUUID(s string) bool {
	if len(s) != 36 {
		return false
	}
	for i, c := range s {
		switch i {
		case 8, 13, 18, 23:
			if c != '-' {
				return false
			}
		default:
			if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
				return false
			}
		}
	}
	return true
}

// RestoreEntry writes the next version of an entry as a copy of an earlier one (its content and status; superseded_by too when that entry is still active), so reverting an edit is a version like any other. expect is as for UpdateEntry.
func (s *Store) RestoreEntry(ctx context.Context, id string, version, expect int, p Provenance) (*Entry, error) {
	var out *Entry
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := validProvenance(p); err != nil {
			return err
		}
		cur, err := lockEntry(ctx, tx, id, expect)
		if err != nil {
			return err
		}
		if version <= 0 || version >= cur.Version {
			return fmt.Errorf("%w: restore names an earlier version, 1 to %d", ErrInvalid, cur.Version-1)
		}
		old, err := entryAt(ctx, tx, id, version)
		if err != nil {
			return err
		}
		by := old.SupersededBy
		if by != nil {
			if e, err := entryAt(ctx, tx, *by, 0); err != nil || e.Status != "active" {
				by = nil
			}
		}
		if p.Note == "" {
			p.Note = fmt.Sprintf("restored version %d", version)
		}
		out, err = writeNext(ctx, tx, cur, old.Status, old.EntryFields, by, p)
		return err
	})
	return out, err
}
