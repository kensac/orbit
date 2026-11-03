package store

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
)

const (
	MaxConversationArchives     = 100
	MaxConversationArchiveBytes = 8 << 20
)

// ValidateConversationArchiveSnapshot checks that a snapshot is exported
// conversation metadata plus well-shaped thread pages, without interpreting
// any content as an operational message or action.
func ValidateConversationArchiveSnapshot(sourceID string, raw json.RawMessage) error {
	var snapshot struct {
		Conversation json.RawMessage   `json:"conversation"`
		Pages        []json.RawMessage `json:"pages"`
	}
	if err := json.Unmarshal(raw, &snapshot); err != nil || len(snapshot.Conversation) == 0 || len(snapshot.Pages) == 0 {
		return fmt.Errorf("%w: snapshot must contain source conversation metadata and thread pages", ErrInvalid)
	}
	var conversation struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(snapshot.Conversation, &conversation); err != nil || conversation.ID != sourceID {
		return fmt.Errorf("%w: snapshot conversation id does not match source identity", ErrInvalid)
	}
	for i, rawPage := range snapshot.Pages {
		var page struct {
			Items      []json.RawMessage `json:"items"`
			HasMore    *bool             `json:"has_more"`
			NextBefore json.RawMessage   `json:"next_before"`
		}
		if err := json.Unmarshal(rawPage, &page); err != nil || page.Items == nil || page.HasMore == nil || len(page.NextBefore) == 0 {
			return fmt.Errorf("%w: thread page %d has an invalid page envelope", ErrInvalid, i)
		}
		var next *string
		if string(page.NextBefore) != "null" {
			if err := json.Unmarshal(page.NextBefore, &next); err != nil || next == nil {
				return fmt.Errorf("%w: thread page %d has an invalid cursor", ErrInvalid, i)
			}
		}
		if (*page.HasMore && (next == nil || *next == "")) || (!*page.HasMore && next != nil) {
			return fmt.Errorf("%w: thread page %d has inconsistent pagination", ErrInvalid, i)
		}
		for j, item := range page.Items {
			var kind struct {
				Kind string `json:"kind"`
			}
			if err := json.Unmarshal(item, &kind); err != nil || (kind.Kind != "message" && kind.Kind != "run") {
				return fmt.Errorf("%w: thread page %d item %d has an unsupported kind", ErrInvalid, i, j)
			}
		}
	}
	return nil
}

// NewConversationArchive is an exported transcript snapshot. Snapshot is the
// exact conversation metadata and thread pages from the source bundle.
type NewConversationArchive struct {
	TargetAgent          string
	SourceAgent          string
	SourceConversationID string
	SourceBundleSHA256   string
	Snapshot             json.RawMessage
	ImportedBy           string
}

// ConversationArchive is an immutable, read-only transcript snapshot. It has
// no live conversation, message, run, question or action behavior.
type ConversationArchive struct {
	ID                   int64           `json:"id"`
	TargetAgent          string          `json:"target_agent"`
	SourceAgent          string          `json:"source_agent"`
	SourceConversationID string          `json:"source_conversation_id"`
	Version              int             `json:"version"`
	SourceBundleSHA256   string          `json:"source_bundle_sha256"`
	ContentSHA256        string          `json:"content_sha256"`
	Snapshot             json.RawMessage `json:"snapshot"`
	ImportedBy           string          `json:"imported_by"`
	ImportedAt           string          `json:"imported_at"`
	Kind                 string          `json:"kind"`
	ReadOnly             bool            `json:"read_only"`
	Coverage             string          `json:"coverage"`
	CoverageDetails      string          `json:"coverage_details"`
}

// ConversationArchiveImport reports the append-only outcome for one source
// conversation. Repeating the same content is unchanged; changed content adds
// a version without modifying earlier rows.
type ConversationArchiveImport struct {
	SourceAgent          string `json:"source_agent"`
	SourceConversationID string `json:"source_conversation_id"`
	Outcome              string `json:"outcome"`
	Version              int    `json:"version"`
	ID                   int64  `json:"id,omitempty"`
}

// ImportConversationArchives appends bounded exported transcript snapshots in
// one transaction. It never creates or mutates operational conversation data.
func (s *Store) ImportConversationArchives(ctx context.Context, archives []NewConversationArchive, dryRun bool) ([]ConversationArchiveImport, error) {
	if len(archives) > MaxConversationArchives {
		return nil, fmt.Errorf("%w: at most %d conversation archives per import", ErrInvalid, MaxConversationArchives)
	}
	seen := make(map[string]bool, len(archives))
	for i := range archives {
		a := &archives[i]
		if !IsUUID(a.SourceConversationID) {
			return nil, fmt.Errorf("%w: archive %d has an invalid source conversation id", ErrInvalid, i)
		}
		if strings.TrimSpace(a.SourceAgent) == "" || len(a.SourceAgent) > 128 || strings.ContainsAny(a.SourceAgent, "\x00\r\n") {
			return nil, fmt.Errorf("%w: archive %d has an invalid source agent", ErrInvalid, i)
		}
		if len(a.SourceBundleSHA256) != 64 || !isLowerHex(a.SourceBundleSHA256) {
			return nil, fmt.Errorf("%w: archive %d has an invalid bundle hash", ErrInvalid, i)
		}
		if len(a.Snapshot) == 0 || len(a.Snapshot) > MaxConversationArchiveBytes || !json.Valid(a.Snapshot) {
			return nil, fmt.Errorf("%w: archive %d snapshot is empty, invalid or too large", ErrInvalid, i)
		}
		if err := ValidateConversationArchiveSnapshot(a.SourceConversationID, a.Snapshot); err != nil {
			return nil, fmt.Errorf("archive %d: %w", i, err)
		}
		key := a.TargetAgent + "\x00" + a.SourceAgent + "\x00" + a.SourceConversationID
		if seen[key] {
			return nil, fmt.Errorf("%w: duplicate source conversation in import", ErrInvalid)
		}
		seen[key] = true
	}
	results := make([]ConversationArchiveImport, len(archives))
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		order := make([]int, len(archives))
		for i := range order {
			order[i] = i
		}
		sort.Slice(order, func(i, j int) bool {
			a, b := archives[order[i]], archives[order[j]]
			return a.TargetAgent+"\x00"+a.SourceAgent+"\x00"+a.SourceConversationID < b.TargetAgent+"\x00"+b.SourceAgent+"\x00"+b.SourceConversationID
		})
		for _, index := range order {
			a := &archives[index]
			canonical, err := canonicalArchiveJSON(a.Snapshot)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(canonical)
			contentHash := hex.EncodeToString(sum[:])
			lockKey := fmt.Sprintf("%d:%s%d:%s%d:%s", len(a.TargetAgent), a.TargetAgent,
				len(a.SourceAgent), a.SourceAgent, len(a.SourceConversationID), a.SourceConversationID)
			if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock(hashtextextended($1, 0))`, lockKey); err != nil {
				return err
			}
			var existingID int64
			var existingVersion int
			err = tx.QueryRow(ctx, `SELECT id, version FROM conversation_archives
				WHERE target_agent=$1 AND source_agent=$2 AND source_conversation_id=$3 AND content_sha256=$4`,
				a.TargetAgent, a.SourceAgent, a.SourceConversationID, contentHash).Scan(&existingID, &existingVersion)
			if err == nil {
				results[index] = ConversationArchiveImport{SourceAgent: a.SourceAgent,
					SourceConversationID: a.SourceConversationID, Outcome: "unchanged", Version: existingVersion, ID: existingID}
				continue
			}
			if !errors.Is(err, pgx.ErrNoRows) {
				return err
			}
			var version int
			if err := tx.QueryRow(ctx, `SELECT COALESCE(max(version), 0) + 1 FROM conversation_archives
				WHERE target_agent=$1 AND source_agent=$2 AND source_conversation_id=$3`,
				a.TargetAgent, a.SourceAgent, a.SourceConversationID).Scan(&version); err != nil {
				return err
			}
			outcome := "created"
			if version > 1 {
				outcome = "versioned"
			}
			result := ConversationArchiveImport{SourceAgent: a.SourceAgent,
				SourceConversationID: a.SourceConversationID, Outcome: outcome, Version: version}
			if dryRun {
				results[index] = result
				continue
			}
			if err := tx.QueryRow(ctx, `INSERT INTO conversation_archives
				(target_agent, source_agent, source_conversation_id, version, source_bundle_sha256, content_sha256, snapshot, imported_by)
				VALUES ($1,$2,$3,$4,$5,$6,$7,$8) RETURNING id`, a.TargetAgent, a.SourceAgent,
				a.SourceConversationID, version, a.SourceBundleSHA256, contentHash, a.Snapshot, a.ImportedBy).Scan(&result.ID); err != nil {
				return err
			}
			results[index] = result
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return results, nil
}

func canonicalArchiveJSON(raw []byte) ([]byte, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	return json.Marshal(value)
}

// ListConversationArchives returns immutable snapshot versions for one target
// agent, newest first.
func (s *Store) ListConversationArchives(ctx context.Context, targetAgent string, limit int, beforeID int64) ([]ConversationArchive, bool, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	rows, err := s.Pool.Query(ctx, `SELECT id, target_agent, source_agent, source_conversation_id::text, version, source_bundle_sha256,
		content_sha256, snapshot, imported_by, imported_at::text
	FROM conversation_archives WHERE target_agent=$1 AND ($2 = 0 OR id < $2)
	ORDER BY id DESC LIMIT $3`, targetAgent, beforeID, limit+1)
	if err != nil {
		return nil, false, err
	}
	defer rows.Close()
	var out []ConversationArchive
	for rows.Next() {
		var a ConversationArchive
		if err := rows.Scan(&a.ID, &a.TargetAgent, &a.SourceAgent, &a.SourceConversationID, &a.Version,
			&a.SourceBundleSHA256, &a.ContentSHA256, &a.Snapshot, &a.ImportedBy, &a.ImportedAt); err != nil {
			return nil, false, err
		}
		setArchiveContract(&a)
		out = append(out, a)
	}
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	more := len(out) > limit
	if more {
		out = out[:limit]
	}
	return out, more, nil
}

// GetConversationArchive returns one immutable archived snapshot for its
// target agent.
func (s *Store) GetConversationArchive(ctx context.Context, targetAgent string, id int64) (*ConversationArchive, error) {
	var a ConversationArchive
	err := s.Pool.QueryRow(ctx, `SELECT id, target_agent, source_agent, source_conversation_id::text, version,
		source_bundle_sha256, content_sha256, snapshot, imported_by, imported_at::text
		FROM conversation_archives WHERE target_agent=$1 AND id=$2`, targetAgent, id).Scan(
		&a.ID, &a.TargetAgent, &a.SourceAgent, &a.SourceConversationID, &a.Version,
		&a.SourceBundleSHA256, &a.ContentSHA256, &a.Snapshot, &a.ImportedBy, &a.ImportedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	setArchiveContract(&a)
	return &a, nil
}

func setArchiveContract(a *ConversationArchive) {
	a.Kind = "exported_thread_snapshot"
	a.ReadOnly = true
	a.Coverage = "partial"
	a.CoverageDetails = "source conversation metadata and exported thread pages only"
}

func isLowerHex(s string) bool {
	for _, r := range s {
		if r >= '0' && r <= '9' {
			continue
		}
		if r >= 'a' && r <= 'f' {
			continue
		}
		return false
	}
	return true
}
