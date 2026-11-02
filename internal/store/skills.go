package store

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"

	"github.com/kensac/orbit/internal/frontmatter"
)

// Skill sources: who wrote a version.
const (
	SkillByPerson = "person"
	SkillByAgent  = "agent"
	SkillByImport = "import"
)

// Bounds on a skill. The owner's largest skill directory is under 1 MB with its scripts and an image; these leave room without letting one skill fill a run's directory.
const (
	skillContentMax = 256 << 10
	skillFileMax    = 4 << 20
	skillFilesMax   = 200
	skillTotalMax   = 16 << 20
	// MaxImportSkills bounds one import request.
	MaxImportSkills = 100
)

// SkillNameRE is a skill's name: a spec name, as Claude Code's skill directory names are.
var SkillNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// SkillFile is one file of a skill beside its SKILL.md.
type SkillFile struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
}

// Skill is one version of a skill: the current one where a single skill is returned.
type Skill struct {
	Name         string    `json:"name"`
	Version      int       `json:"version"`
	Status       string    `json:"status"`
	Description  string    `json:"description"`
	SHA256       string    `json:"sha256"`
	Source       string    `json:"source"`
	SourceRunID  *string   `json:"source_run_id"`
	Author       *string   `json:"author"`
	Note         string    `json:"note"`
	RestoredFrom *int      `json:"restored_from"`
	CreatedAt    time.Time `json:"created_at"`
	UpdatedAt    time.Time `json:"updated_at"`
	// FilePaths are the version's files beside SKILL.md, by path.
	FilePaths []string `json:"file_paths"`
	// Content and Files are filled where a version is read in full.
	Content string      `json:"content,omitempty"`
	Files   []SkillFile `json:"files,omitempty"`
}

// SkillProvenance is who wrote a version and why.
type SkillProvenance struct {
	Source string
	RunID  string
	Author string
	Note   string
}

// SkillDescription reads a SKILL.md's description from its frontmatter, and checks a name there matches the skill's.
func SkillDescription(name, content string) (string, error) {
	fm, _, err := frontmatter.Split([]byte(content))
	if err != nil {
		return "", fmt.Errorf("%w: SKILL.md must open with frontmatter naming its description (%s)", ErrInvalid, err.Error())
	}
	f := frontmatter.Fields(fm)
	if n := f["name"]; n != "" && n != name {
		return "", fmt.Errorf("%w: SKILL.md names itself %q, not %q", ErrInvalid, n, name)
	}
	d := strings.Join(strings.Fields(f["description"]), " ")
	if d == "" {
		return "", fmt.Errorf("%w: SKILL.md's frontmatter has no description, which is how a skill is found", ErrInvalid)
	}
	return d, nil
}

// validSkill checks a version and returns its description and digest.
func validSkill(name, content string, files []SkillFile) (string, string, error) {
	if !SkillNameRE.MatchString(name) {
		return "", "", fmt.Errorf("%w: skill name %q must match %s", ErrInvalid, name, SkillNameRE)
	}
	if len(content) > skillContentMax || !utf8.ValidString(content) {
		return "", "", fmt.Errorf("%w: SKILL.md is UTF-8 of at most %d bytes", ErrInvalid, skillContentMax)
	}
	desc, err := SkillDescription(name, content)
	if err != nil {
		return "", "", err
	}
	if len(files) > skillFilesMax {
		return "", "", fmt.Errorf("%w: at most %d files", ErrInvalid, skillFilesMax)
	}
	total := len(content)
	seen := map[string]bool{}
	for _, f := range files {
		if err := ValidSkillPath(f.Path); err != nil {
			return "", "", err
		}
		if seen[f.Path] {
			return "", "", fmt.Errorf("%w: file %q appears twice", ErrInvalid, f.Path)
		}
		seen[f.Path] = true
		if len(f.Content) > skillFileMax {
			return "", "", fmt.Errorf("%w: file %q is over %d bytes", ErrInvalid, f.Path, skillFileMax)
		}
		total += len(f.Content)
	}
	if total > skillTotalMax {
		return "", "", fmt.Errorf("%w: a skill is at most %d bytes in all", ErrInvalid, skillTotalMax)
	}
	return desc, skillDigest(content, files), nil
}

// ValidSkillPath checks a file's path: relative, clean, inside the skill's directory, and not SKILL.md itself.
func ValidSkillPath(p string) error {
	if p == "" || strings.HasPrefix(p, "/") || strings.Contains(p, "\\") || path.Clean(p) != p || p == "." ||
		p == ".." || strings.HasPrefix(p, "../") || p == "SKILL.md" || strings.ContainsRune(p, 0) {
		return fmt.Errorf("%w: file path %q must be relative, clean and inside the skill, and not SKILL.md", ErrInvalid, p)
	}
	return nil
}

// skillDigest is over SKILL.md and each file by path, so it names exactly a version's content.
// SkillDigest is a skill version's sha256: over SKILL.md and every file by path, so a copy of a skill on disk can be matched to a version.
func SkillDigest(content string, files []SkillFile) string { return skillDigest(content, files) }

func skillDigest(content string, files []SkillFile) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "SKILL.md\x00%d\x00%s", len(content), content)
	sorted := slices.Clone(files)
	slices.SortFunc(sorted, func(a, b SkillFile) int { return strings.Compare(a.Path, b.Path) })
	for _, f := range sorted {
		_, _ = fmt.Fprintf(h, "\x00%s\x00%d\x00", f.Path, len(f.Content))
		h.Write(f.Content)
	}
	return hex.EncodeToString(h.Sum(nil))
}

const skillCols = `s.name, v.version, v.status, v.description, v.sha256, v.source, v.source_run::text, v.author, v.note,
	v.restored_from, s.created_at, v.created_at,
	COALESCE((SELECT array_agg(f.path ORDER BY f.path) FROM skill_files f WHERE f.skill = v.skill AND f.version = v.version), '{}')`

func scanSkill(row pgx.Row) (*Skill, error) {
	var k Skill
	err := row.Scan(&k.Name, &k.Version, &k.Status, &k.Description, &k.SHA256, &k.Source, &k.SourceRunID, &k.Author,
		&k.Note, &k.RestoredFrom, &k.CreatedAt, &k.UpdatedAt, &k.FilePaths)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	return &k, err
}

// SkillWrite is a version to write.
type SkillWrite struct {
	Name    string
	Content string
	Files   []SkillFile
	// Expect, above 0, is the version the writer read; another is ErrConflict.
	Expect int
	// Create refuses a skill that exists (ErrConflict) rather than writing its next version.
	Create bool
}

// PutSkill writes a skill's next version, or its first. A version identical to the current active one (by digest) writes nothing and returns it with changed false.
func (s *Store) PutSkill(ctx context.Context, w SkillWrite, p SkillProvenance) (*Skill, bool, error) {
	var out *Skill
	var changed bool
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, changed, err = putSkill(ctx, tx, w, p)
		return err
	})
	return out, changed, err
}

func validSkillProvenance(p SkillProvenance) error {
	if !slices.Contains([]string{SkillByPerson, SkillByAgent, SkillByImport}, p.Source) {
		return fmt.Errorf("%w: unknown source %q", ErrInvalid, p.Source)
	}
	if len(p.Note) > entryNoteMax {
		return fmt.Errorf("%w: note is at most %d bytes", ErrInvalid, entryNoteMax)
	}
	return nil
}

func putSkill(ctx context.Context, tx pgx.Tx, w SkillWrite, p SkillProvenance) (*Skill, bool, error) {
	desc, digest, err := validSkill(w.Name, w.Content, w.Files)
	if err != nil {
		return nil, false, err
	}
	if err := validSkillProvenance(p); err != nil {
		return nil, false, err
	}
	tag, err := tx.Exec(ctx, `INSERT INTO skills (name) VALUES ($1) ON CONFLICT DO NOTHING`, w.Name)
	if err != nil {
		return nil, false, err
	}
	version := 1
	if tag.RowsAffected() == 0 {
		if w.Create {
			return nil, false, fmt.Errorf("a skill named %q exists: %w", w.Name, ErrConflict)
		}
		cur, err := lockSkill(ctx, tx, w.Name, w.Expect)
		if err != nil {
			return nil, false, err
		}
		if cur.SHA256 == digest && cur.Status == "active" {
			return cur, false, nil
		}
		version = cur.Version + 1
	}
	if err := insertSkillVersion(ctx, tx, w.Name, version, "active", desc, w.Content, digest, w.Files, nil, p); err != nil {
		return nil, false, err
	}
	k, err := skillAt(ctx, tx, w.Name, version, false)
	return k, true, err
}

func insertSkillVersion(ctx context.Context, tx pgx.Tx, name string, version int, status, desc, content, digest string, files []SkillFile, restoredFrom *int, p SkillProvenance) error {
	if _, err := tx.Exec(ctx, `INSERT INTO skill_versions
		    (skill, version, status, description, content, sha256, source, source_run, author, note, restored_from)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NULLIF($8, '')::uuid, NULLIF($9, ''), $10, $11)`,
		name, version, status, desc, content, digest, p.Source, p.RunID, p.Author, p.Note, restoredFrom); err != nil {
		if isForeignKeyViolation(err) {
			return fmt.Errorf("%w: the run does not exist", ErrInvalid)
		}
		return err
	}
	for _, f := range files {
		if _, err := tx.Exec(ctx, `INSERT INTO skill_files (skill, version, path, content) VALUES ($1, $2, $3, $4)`,
			name, version, f.Path, f.Content); err != nil {
			return err
		}
	}
	_, err := tx.Exec(ctx, `UPDATE skills SET current_version = $2 WHERE name = $1`, name, version)
	return err
}

func lockSkill(ctx context.Context, tx pgx.Tx, name string, expect int) (*Skill, error) {
	var current int
	err := tx.QueryRow(ctx, `SELECT current_version FROM skills WHERE name = $1 FOR UPDATE`, name).Scan(&current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if expect > 0 && expect != current {
		return nil, fmt.Errorf("the skill is at version %d, not %d: %w", current, expect, ErrConflict)
	}
	return skillAt(ctx, tx, name, current, false)
}

// skillAt reads a skill at a version (0: its current one), in full when full is set.
func skillAt(ctx context.Context, q queryer, name string, version int, full bool) (*Skill, error) {
	k, err := scanSkill(q.QueryRow(ctx, `SELECT `+skillCols+`
		FROM skills s JOIN skill_versions v ON v.skill = s.name
		WHERE s.name = $1 AND v.version = CASE WHEN $2 > 0 THEN $2 ELSE s.current_version END`, name, version))
	if err != nil || !full {
		return k, err
	}
	if err := q.QueryRow(ctx, `SELECT content FROM skill_versions WHERE skill = $1 AND version = $2`, name, k.Version).Scan(&k.Content); err != nil {
		return nil, err
	}
	rows, err := q.Query(ctx, `SELECT path, content FROM skill_files WHERE skill = $1 AND version = $2 ORDER BY path`, name, k.Version)
	if err != nil {
		return nil, err
	}
	k.Files, err = pgx.CollectRows(rows, pgx.RowToStructByPos[SkillFile])
	return k, err
}

// GetSkill returns a skill at a version (0: its current one), with its content and files when full is set.
func (s *Store) GetSkill(ctx context.Context, name string, version int, full bool) (*Skill, error) {
	return skillAt(ctx, s.Pool, name, version, full)
}

// SkillVersions lists a skill's versions, newest first, without content.
func (s *Store) SkillVersions(ctx context.Context, name string) ([]Skill, error) {
	if _, err := skillAt(ctx, s.Pool, name, 0, false); err != nil {
		return nil, err
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+skillCols+` FROM skills s JOIN skill_versions v ON v.skill = s.name
		WHERE s.name = $1 ORDER BY v.version DESC`, name)
	if err != nil {
		return nil, err
	}
	return collectSkills(rows)
}

func collectSkills(rows pgx.Rows) ([]Skill, error) {
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		k, err := scanSkill(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, rows.Err()
}

// ListSkills returns every skill's current version, by name; status is active (the default), retired or all.
func (s *Store) ListSkills(ctx context.Context, status string) ([]Skill, error) {
	switch status {
	case "":
		status = "active"
	case "active", "retired":
	case "all":
		status = ""
	default:
		return nil, fmt.Errorf("%w: status must be active, retired or all", ErrInvalid)
	}
	rows, err := s.Pool.Query(ctx, `SELECT `+skillCols+` FROM skills s JOIN skill_versions v ON v.skill = s.name AND v.version = s.current_version
		WHERE $1 = '' OR v.status = $1 ORDER BY s.name`, status)
	if err != nil {
		return nil, err
	}
	return collectSkills(rows)
}

// SkillsByName returns the current, active versions of the named skills in full, by name; names that are unknown or retired are left out.
func (s *Store) SkillsByName(ctx context.Context, names []string) ([]Skill, error) {
	if len(names) == 0 {
		return nil, nil
	}
	rows, err := s.Pool.Query(ctx, `SELECT s.name FROM skills s JOIN skill_versions v ON v.skill = s.name AND v.version = s.current_version
		WHERE s.name = ANY($1) AND v.status = 'active' ORDER BY s.name`, names)
	if err != nil {
		return nil, err
	}
	found, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return nil, err
	}
	var out []Skill
	for _, n := range found {
		k, err := skillAt(ctx, s.Pool, n, 0, true)
		if err != nil {
			return nil, err
		}
		out = append(out, *k)
	}
	return out, nil
}

// RetireSkill writes a retired version: agents stop getting it, and every version stays readable.
func (s *Store) RetireSkill(ctx context.Context, name string, expect int, p SkillProvenance) (*Skill, error) {
	var out *Skill
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var err error
		out, err = retireSkill(ctx, tx, name, expect, p)
		return err
	})
	return out, err
}

func retireSkill(ctx context.Context, tx pgx.Tx, name string, expect int, p SkillProvenance) (*Skill, error) {
	if err := validSkillProvenance(p); err != nil {
		return nil, err
	}
	cur, err := lockSkill(ctx, tx, name, expect)
	if err != nil {
		return nil, err
	}
	if cur.Status == "retired" {
		return cur, nil
	}
	full, err := skillAt(ctx, tx, name, cur.Version, true)
	if err != nil {
		return nil, err
	}
	if err := insertSkillVersion(ctx, tx, name, cur.Version+1, "retired", cur.Description, full.Content, cur.SHA256, full.Files, nil, p); err != nil {
		return nil, err
	}
	return skillAt(ctx, tx, name, cur.Version+1, false)
}

// RestoreSkill writes the next version of a skill as a copy of an earlier one, active.
func (s *Store) RestoreSkill(ctx context.Context, name string, version, expect int, p SkillProvenance) (*Skill, error) {
	var out *Skill
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if err := validSkillProvenance(p); err != nil {
			return err
		}
		cur, err := lockSkill(ctx, tx, name, expect)
		if err != nil {
			return err
		}
		if version <= 0 || version >= cur.Version {
			return fmt.Errorf("%w: restore names an earlier version, 1 to %d", ErrInvalid, cur.Version-1)
		}
		old, err := skillAt(ctx, tx, name, version, true)
		if err != nil {
			return err
		}
		if p.Note == "" {
			p.Note = fmt.Sprintf("restored version %d", version)
		}
		if err := insertSkillVersion(ctx, tx, name, cur.Version+1, "active", old.Description, old.Content, old.SHA256, old.Files, &version, p); err != nil {
			return err
		}
		out, err = skillAt(ctx, tx, name, cur.Version+1, false)
		return err
	})
	return out, err
}

// SkillImportResult is what an import did, or would do, with one skill.
type SkillImportResult struct {
	Name    string `json:"name"`
	Outcome string `json:"outcome"`
	Version int    `json:"version,omitempty"`
}

// ImportSkills writes each skill's next version where its content differs from the current one (or it is new, or retired), in one transaction; dryRun reports and rolls back.
func (s *Store) ImportSkills(ctx context.Context, skills []SkillWrite, p SkillProvenance, dryRun bool) ([]SkillImportResult, error) {
	if len(skills) > MaxImportSkills {
		return nil, fmt.Errorf("%w: at most %d skills per import", ErrInvalid, MaxImportSkills)
	}
	seen := map[string]bool{}
	for _, k := range skills {
		if seen[k.Name] {
			return nil, fmt.Errorf("%w: skill %q appears twice", ErrInvalid, k.Name)
		}
		seen[k.Name] = true
	}
	var results []SkillImportResult
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		for _, w := range skills {
			var exists bool
			if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM skills WHERE name = $1)`, w.Name).Scan(&exists); err != nil {
				return err
			}
			w.Create, w.Expect = false, 0
			k, changed, err := putSkill(ctx, tx, w, p)
			if err != nil {
				return fmt.Errorf("skill %s: %w", w.Name, err)
			}
			r := SkillImportResult{Name: w.Name, Outcome: "unchanged", Version: k.Version}
			switch {
			case !exists:
				r.Outcome = "created"
			case changed:
				r.Outcome = "updated"
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

// applySkillWrites applies a run's skill writes in savepoints of its outcome, as applyEntryWrites does entries: a write the store refuses is reported and skipped. A written skill is attached to the agent, so a skill it writes is one it runs with.
func applySkillWrites(ctx context.Context, tx pgx.Tx, runID, agent string, writes []*AgentSkillWrite) error {
	for _, w := range writes {
		p := SkillProvenance{Source: SkillByAgent, RunID: runID, Note: w.Note}
		sp, err := tx.Begin(ctx)
		if err != nil {
			return err
		}
		var k *Skill
		changed := true
		if w.Retire {
			k, err = retireSkill(ctx, sp, w.Name, w.Expect, p)
		} else {
			files := w.Files
			if files == nil && w.Expect > 0 {
				// An agent rewrites SKILL.md; the skill's other files stay as they are.
				var cur *Skill
				if cur, err = skillAt(ctx, sp, w.Name, 0, true); err == nil {
					files = cur.Files
				}
			}
			k, changed, err = putSkill(ctx, sp, SkillWrite{Name: w.Name, Content: w.Content, Files: files, Expect: w.Expect, Create: w.Expect == 0}, p)
			if err == nil {
				_, err = sp.Exec(ctx, `INSERT INTO agent_attachments (agent, kind, name, attached_by)
					VALUES ($1, 'skill', $2, NULL) ON CONFLICT (agent, kind, name) WHERE detached_at IS NULL DO NOTHING`, agent, w.Name)
			}
		}
		if err == nil {
			err = sp.Commit(ctx)
		}
		switch {
		case err == nil:
			w.Result = EntryWriteResult{Outcome: "written", Key: k.Name, Version: k.Version}
			if !changed {
				w.Result.Outcome = "unchanged"
			}
		case errors.Is(err, ErrInvalid), errors.Is(err, ErrConflict), errors.Is(err, ErrNotFound):
			if rerr := sp.Rollback(ctx); rerr != nil {
				return rerr
			}
			w.Result = EntryWriteResult{Outcome: "refused", Error: err.Error()}
		default:
			return err
		}
	}
	return nil
}

// AgentSkillWrite is a skill a run wrote or retired. Expect 0 creates a new skill (refused if the name is taken); above 0 it is the version of an existing skill the run was given.
type AgentSkillWrite struct {
	Name    string
	Content string
	Files   []SkillFile
	Expect  int
	Retire  bool
	Note    string
	Result  EntryWriteResult
}
