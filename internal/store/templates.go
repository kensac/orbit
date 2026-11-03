package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/templates"
)

// Template sources: who wrote a version.
const (
	TemplateByPerson = "person"
	TemplateByImport = "import"
)

// Template is one version of a template: the current one where a single template is returned.
type Template struct {
	Name         string    `json:"name"`
	Version      int       `json:"version"`
	Status       string    `json:"status"`
	Description  string    `json:"description"`
	SHA256       string    `json:"sha256"`
	Source       string    `json:"source"`
	Author       *string   `json:"author"`
	Note         string    `json:"note"`
	RestoredFrom *int      `json:"restored_from"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// Parameters are the version's parameters, in every listing: what a client shows as the form.
	Parameters []templates.Parameter `json:"parameters"`
	// Spec is the agent with its slots, where a version is read in full.
	Spec json.RawMessage `json:"spec,omitempty"`
}

// Doc is the version's document, as templates.Decode reads one.
func (t *Template) Doc() *templates.Template {
	return &templates.Template{Name: t.Name, Description: t.Description, Parameters: t.Parameters, Spec: t.Spec}
}

// TemplateProvenance is who wrote a version and why.
type TemplateProvenance struct {
	Source string
	Author string
	Note   string
}

// TemplateWrite is a version to write.
type TemplateWrite struct {
	Doc *templates.Template
	// Expect, above 0, is the version the writer read; another is ErrConflict.
	Expect int
	// Create refuses a template that exists (ErrConflict) rather than writing its next version.
	Create bool
}

func canonicalTemplate(doc *templates.Template) ([]byte, string, error) {
	if err := doc.Validate(); err != nil {
		return nil, "", fmt.Errorf("%w: template %s: %w", ErrInvalid, doc.Name, err)
	}
	b, err := doc.Canonical()
	if err != nil {
		return nil, "", fmt.Errorf("%w: template %s: %w", ErrInvalid, doc.Name, err)
	}
	sum := sha256.Sum256(b)
	return b, hex.EncodeToString(sum[:]), nil
}

func validTemplateProvenance(p TemplateProvenance) error {
	if !slices.Contains([]string{TemplateByPerson, TemplateByImport}, p.Source) {
		return fmt.Errorf("%w: unknown source %q", ErrInvalid, p.Source)
	}
	if len(p.Note) > entryNoteMax {
		return fmt.Errorf("%w: note is at most %d bytes", ErrInvalid, entryNoteMax)
	}
	return nil
}

const templateCols = `t.name, v.version, v.status, v.description, v.sha256, v.source, v.author, v.note,
	v.restored_from, t.created_at, v.created_at, COALESCE(v.document->'parameters', '[]'::jsonb)`

func scanTemplate(row pgx.Row) (*Template, error) {
	var k Template
	var params []byte
	err := row.Scan(&k.Name, &k.Version, &k.Status, &k.Description, &k.SHA256, &k.Source, &k.Author, &k.Note,
		&k.RestoredFrom, &k.CreatedAt, &k.UpdatedAt, &params)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &k, json.Unmarshal(params, &k.Parameters)
}

// templateAt reads a template at a version (0: its current one), with its spec when full is set.
func templateAt(ctx context.Context, q queryer, name string, version int, full bool) (*Template, error) {
	k, err := scanTemplate(q.QueryRow(ctx, `SELECT `+templateCols+`
		FROM templates t JOIN template_versions v ON v.template = t.name
		WHERE t.name = $1 AND v.version = CASE WHEN $2 > 0 THEN $2 ELSE t.current_version END`, name, version))
	if err != nil || !full {
		return k, err
	}
	err = q.QueryRow(ctx, `SELECT document->'spec' FROM template_versions WHERE template = $1 AND version = $2`, name, k.Version).Scan(&k.Spec)
	return k, err
}

// GetTemplate returns a template at a version (0: its current one), with its spec when full is set.
func (s *Store) GetTemplate(ctx context.Context, name string, version int, full bool) (*Template, error) {
	return templateAt(ctx, s.Pool, name, version, full)
}

// PutTemplate writes a template's next version, or its first. A version identical to the current active one (by digest) writes nothing and returns it with changed false.
func (s *Store) PutTemplate(ctx context.Context, w TemplateWrite, p TemplateProvenance) (*Template, bool, error) {
	var out *Template
	var changed bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, changed, err = putTemplate(ctx, tx, w, p)
		return err
	})
	return out, changed, err
}

func putTemplate(ctx context.Context, tx pgx.Tx, w TemplateWrite, p TemplateProvenance) (*Template, bool, error) {
	doc, digest, err := canonicalTemplate(w.Doc)
	if err != nil {
		return nil, false, err
	}
	if err := validTemplateProvenance(p); err != nil {
		return nil, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO templates (name) VALUES ($1) ON CONFLICT DO NOTHING`, w.Doc.Name)
	if err != nil {
		return nil, false, err
	}
	version := 1
	if tag.RowsAffected() == 0 {
		if w.Create {
			return nil, false, fmt.Errorf("a template named %q exists: %w", w.Doc.Name, ErrConflict)
		}
		cur, err := lockTemplate(ctx, tx, w.Doc.Name, w.Expect)
		if err != nil {
			return nil, false, err
		}
		if cur.SHA256 == digest && cur.Status == "active" {
			return cur, false, nil
		}
		version = cur.Version + 1
	}
	if err := insertTemplateVersion(ctx, tx, w.Doc.Name, version, "active", w.Doc.Description, doc, digest, nil, p); err != nil {
		return nil, false, err
	}
	k, err := templateAt(ctx, tx, w.Doc.Name, version, false)
	return k, true, err
}

func insertTemplateVersion(ctx context.Context, tx pgx.Tx, name string, version int, status, desc string, doc []byte, digest string, restoredFrom *int, p TemplateProvenance) error {
	if _, err := tx.Exec(ctx, `INSERT INTO template_versions
		    (template, version, status, description, document, sha256, source, author, note, restored_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, ''), $9, $10)`,
		name, version, status, desc, doc, digest, p.Source, p.Author, p.Note, restoredFrom); err != nil {
		return err
	}
	_, err := tx.Exec(ctx, `UPDATE templates SET current_version = $2 WHERE name = $1`, name, version)
	return err
}

func lockTemplate(ctx context.Context, tx pgx.Tx, name string, expect int) (*Template, error) {
	var current int
	err := tx.QueryRow(ctx, `SELECT current_version FROM templates WHERE name = $1 FOR UPDATE`, name).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expect > 0 && expect != current {
		return nil, fmt.Errorf("the template is at version %d, not %d: %w", current, expect, ErrConflict)
	}
	return templateAt(ctx, tx, name, current, false)
}

func versionDocument(ctx context.Context, tx pgx.Tx, name string, version int) ([]byte, error) {
	var doc []byte
	err := tx.QueryRow(ctx, `SELECT document FROM template_versions WHERE template = $1 AND version = $2`, name, version).Scan(&doc)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return doc, err
}

// TemplateVersions lists a template's versions, newest first, without their specs.
func (s *Store) TemplateVersions(ctx context.Context, name string) ([]Template, error) {
	if _, err := templateAt(ctx, s.Pool, name, 0, false); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+templateCols+` FROM templates t JOIN template_versions v ON v.template = t.name
		WHERE t.name = $1 ORDER BY v.version DESC`, name)
	if err != nil {
		return nil, err
	}
	return collectTemplates(rows)
}

func collectTemplates(rows pgx.Rows) ([]Template, error) {
	defer rows.Close()
	var out []Template
	for rows.Next() {
		k, err := scanTemplate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// ListTemplates returns every template's current version, by name, without specs; status is active (the default), retired or all.
func (s *Store) ListTemplates(ctx context.Context, status string) ([]Template, error) {
	switch status {
	case "":
		status = "active"
	case "active", "retired":
	case "all":
		status = ""
	default:
		return nil, fmt.Errorf("%w: status must be active, retired or all", ErrInvalid)
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+templateCols+` FROM templates t JOIN template_versions v ON v.template = t.name AND v.version = t.current_version
		WHERE $1 = '' OR v.status = $1 ORDER BY t.name`, status)
	if err != nil {
		return nil, err
	}
	return collectTemplates(rows)
}

// RetireTemplate writes a retired version: nothing is created from it any more, and every version stays readable.
func (s *Store) RetireTemplate(ctx context.Context, name string, expect int, p TemplateProvenance) (*Template, error) {
	var out *Template
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := validTemplateProvenance(p); err != nil {
			return err
		}
		cur, err := lockTemplate(ctx, tx, name, expect)
		if err != nil {
			return err
		}
		if cur.Status == "retired" {
			out = cur
			return nil
		}
		doc, err := versionDocument(ctx, tx, name, cur.Version)
		if err != nil {
			return err
		}
		if err := insertTemplateVersion(ctx, tx, name, cur.Version+1, "retired", cur.Description, doc, cur.SHA256, nil, p); err != nil {
			return err
		}
		out, err = templateAt(ctx, tx, name, cur.Version+1, false)
		return err
	})
	return out, err
}

// RestoreTemplate writes the next version of a template as an active copy of an earlier one.
func (s *Store) RestoreTemplate(ctx context.Context, name string, version, expect int, p TemplateProvenance) (*Template, error) {
	var out *Template
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := validTemplateProvenance(p); err != nil {
			return err
		}
		cur, err := lockTemplate(ctx, tx, name, expect)
		if err != nil {
			return err
		}
		if version <= 0 || version >= cur.Version {
			return fmt.Errorf("%w: restore names an earlier version, 1 to %d", ErrInvalid, cur.Version-1)
		}
		old, err := templateAt(ctx, tx, name, version, false)
		if err != nil {
			return err
		}
		doc, err := versionDocument(ctx, tx, name, version)
		if err != nil {
			return err
		}
		if p.Note == "" {
			p.Note = fmt.Sprintf("restored version %d", version)
		}
		if err := insertTemplateVersion(ctx, tx, name, cur.Version+1, "active", old.Description, doc, old.SHA256, &version, p); err != nil {
			return err
		}
		out, err = templateAt(ctx, tx, name, cur.Version+1, false)
		return err
	})
	return out, err
}
