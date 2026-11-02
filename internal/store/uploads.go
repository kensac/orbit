package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	"unicode"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// MaxUploadBytes bounds one upload. A run writes each attachment into its
// workdir, and a sandbox's workspace crosses sandboxd as one tar of at most
// sandbox.MaxArchiveBytes (256 MiB), so a file a quarter of that leaves room
// for the rest of the workspace.
const MaxUploadBytes = 64 << 20

// MaxAttachments bounds the files one message carries.
const MaxAttachments = 20

// maxUploadName bounds a file name, well inside every filesystem's 255 bytes
// once a " (n)" suffix is added.
const maxUploadName = 200

type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
}

// Upload is a file a person uploaded into a conversation, without its
// content.
type Upload struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	Bytes     int64     `json:"bytes"`
	SHA256    string    `json:"sha256"`
	CreatedAt time.Time `json:"created_at"`
}

// NewUpload is a file to store.
type NewUpload struct {
	ConversationID string
	Name           string
	MediaType      string
	Content        []byte
	Author         string
	// Nonce, when set, is a signed URL's: a second upload with it is
	// ErrConflict.
	Nonce string
}

// UploadName is name made safe as one path element of uploads/: its base
// name, control characters and slashes removed, at most maxUploadName
// bytes. An empty or dot-only name becomes "file".
func UploadName(name string) string {
	name = strings.ReplaceAll(name, "\\", "/")
	name = path.Base(strings.TrimSpace(name))
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || r == '/' {
			return -1
		}
		return r
	}, name)
	name = strings.TrimSpace(name)
	if strings.Trim(name, ".") == "" {
		name = "file"
	}
	for len(name) > maxUploadName {
		r := []rune(name)
		name = string(r[:len(r)-1])
	}
	return name
}

// numbered is name with " (n)" before its extension.
func numbered(name string, n int) string {
	ext := path.Ext(name)
	if ext == name {
		ext = ""
	}
	return fmt.Sprintf("%s (%d)%s", strings.TrimSuffix(name, ext), n, ext)
}

// CreateUpload stores a file in a conversation. A name already taken there
// gets " (2)", " (3)", ... so uploads/<name> names one file; the stored name
// is in the result.
func (s *Store) CreateUpload(ctx context.Context, n NewUpload) (Upload, error) {
	if len(n.Content) > MaxUploadBytes {
		return Upload{}, fmt.Errorf("%w: a file is at most %d bytes", ErrInvalid, MaxUploadBytes)
	}
	mt := strings.TrimSpace(n.MediaType)
	if mt == "" {
		mt = "application/octet-stream"
	}
	sum := sha256.Sum256(n.Content)
	base := UploadName(n.Name)
	u := Upload{MediaType: mt, Bytes: int64(len(n.Content)), SHA256: hex.EncodeToString(sum[:])}
	for i := 1; i <= 1000; i++ {
		name := base
		if i > 1 {
			name = numbered(base, i)
		}
		err := s.Pool.QueryRow(ctx, `INSERT INTO uploads (conversation_id, name, media_type, bytes, sha256, content, author, nonce)
			VALUES ($1, $2, $3, $4, $5, $6, NULLIF($7, ''), NULLIF($8, ''))
			ON CONFLICT (conversation_id, name) DO NOTHING
			RETURNING id::text, name, created_at`,
			n.ConversationID, name, mt, u.Bytes, u.SHA256, n.Content, n.Author, n.Nonce).Scan(&u.ID, &u.Name, &u.CreatedAt)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "23505" {
			return Upload{}, fmt.Errorf("%w: this upload link was already used", ErrConflict)
		}
		if isForeignKeyViolation(err) {
			return Upload{}, ErrNotFound
		}
		return u, err
	}
	return Upload{}, fmt.Errorf("%w: too many files named %q in this conversation", ErrInvalid, base)
}

// GetUpload returns one of a conversation's uploads and its content.
func (s *Store) GetUpload(ctx context.Context, conversationID, id string) (Upload, []byte, error) {
	var u Upload
	var content []byte
	err := s.Pool.QueryRow(ctx, `SELECT id::text, name, media_type, bytes, sha256, created_at, content
		FROM uploads WHERE id = $1 AND conversation_id = $2`, id, conversationID).
		Scan(&u.ID, &u.Name, &u.MediaType, &u.Bytes, &u.SHA256, &u.CreatedAt, &content)
	if errors.Is(err, pgx.ErrNoRows) {
		return u, nil, ErrNotFound
	}
	return u, content, err
}

// Attachments returns the uploads each message carries, in their order,
// keyed by message id; a message with none is absent.
func (s *Store) Attachments(ctx context.Context, messageIDs []string) (map[string][]Upload, error) {
	return messageUploads(ctx, s.Pool, messageIDs)
}

func messageUploads(ctx context.Context, db querier, messageIDs []string) (map[string][]Upload, error) {
	out := map[string][]Upload{}
	if len(messageIDs) == 0 {
		return out, nil
	}
	rows, err := db.Query(ctx, `SELECT a.message_id::text, u.id::text, u.name, u.media_type, u.bytes, u.sha256, u.created_at
		FROM message_attachments a JOIN uploads u ON u.id = a.upload_id
		WHERE a.message_id = ANY($1::uuid[]) ORDER BY a.message_id, a.position`, messageIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var mid string
		var u Upload
		if err := rows.Scan(&mid, &u.ID, &u.Name, &u.MediaType, &u.Bytes, &u.SHA256, &u.CreatedAt); err != nil {
			return nil, err
		}
		out[mid] = append(out[mid], u)
	}
	return out, rows.Err()
}

// UploadContents returns the content of uploads by id.
func (s *Store) UploadContents(ctx context.Context, ids []string) (map[string][]byte, error) {
	out := map[string][]byte{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT id::text, content FROM uploads WHERE id = ANY($1::uuid[])`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id string
		var b []byte
		if err := rows.Scan(&id, &b); err != nil {
			return nil, err
		}
		out[id] = b
	}
	return out, rows.Err()
}

// attach records a message's attachments, each an upload of the message's
// own conversation.
func attach(ctx context.Context, tx pgx.Tx, messageID, conversationID string, uploads []string) error {
	if len(uploads) > MaxAttachments {
		return fmt.Errorf("%w: a message carries at most %d files", ErrInvalid, MaxAttachments)
	}
	for i, id := range uploads {
		var conv string
		err := tx.QueryRow(ctx, `SELECT conversation_id::text FROM uploads WHERE id = $1`, id).Scan(&conv)
		if errors.Is(err, pgx.ErrNoRows) || (err == nil && conv != conversationID) {
			return fmt.Errorf("%w: attachment %s is not an upload of this conversation", ErrInvalid, id)
		}
		if err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `INSERT INTO message_attachments (message_id, upload_id, position) VALUES ($1, $2, $3)
			ON CONFLICT DO NOTHING`, messageID, id, i); err != nil {
			return err
		}
	}
	return nil
}
