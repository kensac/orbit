package actions

import (
	"encoding/json"
	"fmt"
	"regexp"
)

// SkillWriteType writes or retires one of orbit's skills: a new skill, or a new SKILL.md for one the run was given (its other files stay), as the next version (every version kept, restorable through the API). A skill is instructions later runs follow, so writing one is an action under a rule like any other; the change, and the scripts the skill holds, are worked out by code when it is proposed and shown to whoever approves it (engine/skills.go).
const SkillWriteType = "skill:write"

// Bounds a skill write keeps: those of a stored skill (store/skills.go) and of an entry's note.
const (
	skillContentMax = 256 << 10
	skillNoteMax    = 2 << 10
)

var skillNameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)

// SkillWrite is skill:write's params. Version and Changes are set by code when the action is proposed, never taken from the model.
type SkillWrite struct {
	// Op is write or retire.
	Op   string `json:"op"`
	Name string `json:"name"`
	// Content is the whole SKILL.md, frontmatter (name, description) included; empty for retire.
	Content string `json:"content"`
	Note    string `json:"note"`
	// Version is the skill's version the proposing run had, which the write applies against (a newer one refuses it); 0 creates a new skill.
	Version int `json:"version,omitempty"`
	// Changes is what the write changes.
	Changes *SkillChanges `json:"changes,omitempty"`
}

// SkillChanges is what a skill write changes, for the person approving it and the record.
type SkillChanges struct {
	// From is the version it replaces, 0 for a new skill.
	From int `json:"from_version"`
	// Added and Removed count SKILL.md's lines.
	Added   int `json:"lines_added"`
	Removed int `json:"lines_removed"`
	// Diff is SKILL.md's change as a unified diff, bounded.
	Diff string `json:"diff"`
	// Files are the skill's other files, which a write keeps.
	Files []string `json:"files"`
	// Scripts are those of Files that run: under scripts/ or bin/, with a script's extension, a #! line, or an executable's header. A script runs only if the agent's own tools let it, and the SKILL.md written may tell it to.
	Scripts []string `json:"scripts"`
}

func skillWriteType() Type {
	return Type{
		Name:  SkillWriteType,
		Brief: "Write (the whole SKILL.md) or retire one of your skills; every version is kept and can be restored (the orbit-actions skill says how).",
		Description: "Write or retire one of your skills (Claude Code skills orbit keeps for you between runs). op write gives the whole SKILL.md: " +
			"frontmatter with name and a description saying when to use it, then the steps; a skill you have keeps its other files. op retire ends one you have. " +
			"Write one when you have worked out a procedure your person will want done the same way again, or improve one you have. Every version is kept and can be restored.",
		ParamsSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":      map[string]any{"type": "string", "enum": []string{"write", "retire"}},
				"name":    map[string]any{"type": "string", "pattern": skillNameRE.String()},
				"content": map[string]any{"type": "string", "maxLength": skillContentMax},
				"note":    map[string]any{"type": "string", "maxLength": skillNoteMax},
			},
			"required":             []string{"op", "name", "content", "note"},
			"additionalProperties": false,
		},
		Validate: func(p json.RawMessage) error {
			_, err := DecodeSkillWrite(p)
			return err
		},
	}
}

// DecodeSkillWrite reads skill:write's params.
func DecodeSkillWrite(p json.RawMessage) (SkillWrite, error) {
	var v SkillWrite
	if err := strictDecode(p, &v); err != nil {
		return v, err
	}
	if v.Op != "write" && v.Op != "retire" {
		return v, fmt.Errorf("skill:write: op must be write or retire")
	}
	if !skillNameRE.MatchString(v.Name) {
		return v, fmt.Errorf("skill:write: name must match %s", skillNameRE)
	}
	if v.Op == "write" && v.Content == "" {
		return v, fmt.Errorf("skill:write: content is the whole SKILL.md")
	}
	if len(v.Content) > skillContentMax || len(v.Note) > skillNoteMax {
		return v, fmt.Errorf("skill:write: content is at most %d bytes, note %d", skillContentMax, skillNoteMax)
	}
	if v.Version < 0 {
		return v, fmt.Errorf("skill:write: version is not negative")
	}
	return v, nil
}
