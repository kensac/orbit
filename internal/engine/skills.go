package engine

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// maxSkillWrites bounds the skills one run writes or retires: a skill is a worked-out procedure, and a run has at most a few.
const maxSkillWrites = 3

// skillsPluginPrefix names the plugin directory a Claude Code run loads its skills from, under its config dir. The suffix is a digest of the skills it holds, so a change of skills is a change of the CLI's arguments, and a live process holding the old set is closed rather than fed a turn it would answer with stale stubs.
const skillsPluginPrefix = "orbit-skills-"

// knownSkillsFile is the file of every skill in full that a backend without Claude Code's skills reads, under the run's inputs; like knownFile, no source can produce the name.
const knownSkillsFile = "_skills.md"

// agentSkills are the skill names an agent runs with: its spec's and those the API attached, sorted, once each.
func (e *Engine) agentSkills(ctx context.Context, a *spec.Agent) ([]string, error) {
	attached, err := e.Store.AttachedNames(ctx, a.Name, store.AttachSkill)
	if err != nil {
		return nil, err
	}
	names := append(append([]string{}, a.Skills...), attached...)
	slices.Sort(names)
	return slices.Compact(names), nil
}

// skillsDigest names a set of skill versions.
func skillsDigest(skills []store.Skill) string {
	h := sha256.New()
	for _, k := range skills {
		_, _ = fmt.Fprintf(h, "%s\x00%s\x00", k.Name, k.SHA256)
	}
	return hex.EncodeToString(h.Sum(nil))[:12]
}

// skillStub is the SKILL.md Claude Code loads: the name and description it decides by, and where the text is. The text itself is served by orbit's skill tool from the run's own copy, so what the model follows is exactly the version this run was given.
func skillStub(k store.Skill) string {
	desc, _ := json.Marshal(k.Description)
	return fmt.Sprintf("---\nname: %s\ndescription: %s\n---\n\nThis skill's full text is kept by orbit. Call the %s tool with {\"name\": %q} and follow what it returns. Read the skill's other files with the same tool, passing a file's path as file.\n",
		k.Name, desc, harness.ToolSkill, k.Name)
}

// writeSkills gives a Claude Code run its skills: stubs in a plugin under the CLI's config dir, reconciled by digest (an unchanged set is left as it is, others are removed), and every skill in full where orbit's skill tool reads it. Without orbit's tools the plugin holds the full SKILL.md instead (its other files are then out of the model's reach). It returns the plugin directory, "" for an agent without skills.
func writeSkills(home string, skills []store.Skill, withTool bool) (string, error) {
	config := filepath.Join(home, ".claude")
	digest := skillsDigest(skills)
	want := ""
	if len(skills) > 0 {
		want = filepath.Join(config, skillsPluginPrefix+digest)
	}
	old, _ := filepath.Glob(filepath.Join(config, skillsPluginPrefix+"*"))
	for _, d := range old {
		if d != want {
			if err := os.RemoveAll(d); err != nil {
				return "", err
			}
		}
	}
	if want == "" {
		return "", nil
	}
	if withTool {
		full := harness.SkillsDir(harness.StateDir(home))
		if err := os.RemoveAll(full); err != nil {
			return "", err
		}
		for _, k := range skills {
			if err := writeSkillTree(filepath.Join(full, k.Name), k); err != nil {
				return "", err
			}
		}
	}
	if _, err := os.Stat(filepath.Join(want, ".claude-plugin", "plugin.json")); err == nil {
		return want, nil // the same set, written by an earlier run
	}
	tmp := want + ".tmp"
	_ = os.RemoveAll(tmp)
	manifest, _ := json.Marshal(map[string]any{"name": "orbit", "description": "This agent's skills, kept by orbit.", "version": digest})
	if err := writeFile(filepath.Join(tmp, ".claude-plugin", "plugin.json"), manifest); err != nil {
		return "", err
	}
	for _, k := range skills {
		dir := filepath.Join(tmp, "skills", k.Name)
		if withTool {
			if err := writeFile(filepath.Join(dir, "SKILL.md"), []byte(skillStub(k))); err != nil {
				return "", err
			}
		} else if err := writeFile(filepath.Join(dir, "SKILL.md"), []byte(k.Content)); err != nil {
			return "", err
		}
	}
	if err := os.Rename(tmp, want); err != nil {
		return "", err
	}
	return want, nil
}

// writeSkillTree writes a skill's SKILL.md and files under dir. Paths were checked when the version was stored (store.ValidSkillPath) and are checked again here: a write never leaves dir.
func writeSkillTree(dir string, k store.Skill) error {
	if err := writeFile(filepath.Join(dir, "SKILL.md"), []byte(k.Content)); err != nil {
		return err
	}
	for _, f := range k.Files {
		if err := store.ValidSkillPath(f.Path); err != nil {
			return err
		}
		p := filepath.Join(dir, filepath.FromSlash(f.Path))
		if !strings.HasPrefix(p, filepath.Clean(dir)+string(filepath.Separator)) {
			return fmt.Errorf("skill %s: file %q leaves its directory", k.Name, f.Path)
		}
		if err := writeFile(p, f.Content); err != nil {
			return err
		}
	}
	return nil
}

func writeFile(p string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
		return err
	}
	return os.WriteFile(p, b, 0o600)
}

// skillsInput is every skill's SKILL.md as one input file, for a backend without Claude Code's skills.
func skillsInput(skills []store.Skill) []byte {
	var b strings.Builder
	b.WriteString("# Your skills\n\nProcedures your person keeps for you. Follow one when its description fits the task.\n")
	for _, k := range skills {
		fmt.Fprintf(&b, "\n## %s\n\n%s\n", k.Name, strings.TrimSpace(k.Content))
	}
	return []byte(b.String())
}

// prepareSkills reads an agent's skills and gives them to the run: a plugin directory for Claude Code, an input file for other backends. It records which versions the run had (skills.json).
func (e *Engine) prepareSkills(ctx context.Context, c *store.ClaimedRun, a *spec.Agent, kind, home, workdir, inputsDir string, withTool bool, inputs []runner.Input) ([]store.Skill, string, []runner.Input, error) {
	names, err := e.agentSkills(ctx, a)
	if err != nil {
		return nil, "", nil, err
	}
	skills, err := e.Store.SkillsByName(ctx, names)
	if err != nil {
		return nil, "", nil, err
	}
	if len(skills) > 0 {
		type had struct {
			Name    string `json:"name"`
			Version int    `json:"version"`
			SHA256  string `json:"sha256"`
		}
		var list []had
		for _, k := range skills {
			list = append(list, had{k.Name, k.Version, k.SHA256})
		}
		b, _ := json.Marshal(list)
		if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", "skills.json", "application/json", b); err != nil {
			return nil, "", nil, err
		}
	}
	if kind == "claude-code" {
		dir, err := writeSkills(home, skills, withTool)
		return skills, dir, inputs, err
	}
	if len(skills) == 0 {
		return nil, "", inputs, nil
	}
	rel := filepath.Join(inputsDir, knownSkillsFile)
	for _, in := range inputs {
		if in.Path == rel {
			return skills, "", inputs, nil
		}
	}
	b := skillsInput(skills)
	if err := writeFile(filepath.Join(workdir, rel), b); err != nil {
		return nil, "", nil, err
	}
	if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", rel, "text/markdown", b); err != nil {
		return nil, "", nil, err
	}
	return skills, "", append(inputs, runner.Input{Name: "skills", Path: rel, MediaType: "text/markdown", Content: b}), nil
}

// agentHooks are the agent's spec hooks as the runner installs them.
func agentHooks(a *spec.Agent) []runner.AgentHook {
	var out []runner.AgentHook
	for _, h := range a.Hooks {
		timeout := time.Duration(h.Timeout)
		if len(h.Run) > 0 && timeout == 0 {
			timeout = spec.DefaultHookTimeout
		}
		out = append(out, runner.AgentHook{Event: h.Event, Matcher: h.Matcher,
			Def: harness.AgentHookDef{Event: h.Event, Say: h.Say, Deny: h.Deny, Run: h.Run, Timeout: timeout}})
	}
	return out
}

// OutputSkill is one skill write in a run's output.
type OutputSkill struct {
	// Op is write (a new skill, or a new SKILL.md for one the run has) or retire.
	Op   string `json:"op"`
	Name string `json:"name"`
	// Content is the whole SKILL.md, frontmatter (name, description) included; empty for retire.
	Content string `json:"content"`
	Note    string `json:"note"`
}

// skillsSchema is the output's skills property; every field required, as for entries.
func skillsSchema() map[string]any {
	return map[string]any{
		"type": "array", "maxItems": maxSkillWrites,
		"items": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"op":      map[string]any{"type": "string", "enum": []string{"write", "retire"}},
				"name":    map[string]any{"type": "string", "pattern": store.SkillNameRE.String()},
				"content": map[string]any{"type": "string", "maxLength": 256 << 10},
				"note":    map[string]any{"type": "string", "maxLength": 2 << 10},
			},
			"required":             []string{"op", "name", "content", "note"},
			"additionalProperties": false,
		},
	}
}

// withSkills adds the skills property to a schema, required for the backend, optional in the engine's envelope.
func withSkills(schema json.RawMessage, required bool) json.RawMessage {
	var s map[string]any
	if json.Unmarshal(schema, &s) != nil {
		return schema
	}
	props, _ := s["properties"].(map[string]any)
	if props == nil {
		return schema
	}
	props["skills"] = skillsSchema()
	if req, _ := s["required"].([]any); required && !slices.Contains(req, any("skills")) {
		s["required"] = append(req, "skills")
	}
	b, _ := json.Marshal(s)
	return b
}

// resolveSkillWrites turns a run's skill ops into store writes: a write to a skill the run has is its next version (at the version the run had), a write to a new name creates it, and a retire must name a skill the run has. A skill a run writes is attached to its agent.
func resolveSkillWrites(had []store.Skill, ops []OutputSkill) ([]*store.AgentSkillWrite, []refusedWrite) {
	byName := map[string]store.Skill{}
	for _, k := range had {
		byName[k.Name] = k
	}
	var writes []*store.AgentSkillWrite
	var refused []refusedWrite
	for i, op := range ops {
		if i >= maxSkillWrites {
			refused = append(refused, refusedWrite{Index: i, Op: op.Op, Ref: op.Name, Error: fmt.Sprintf("at most %d skill writes per run", maxSkillWrites)})
			continue
		}
		k, has := byName[op.Name]
		w := &store.AgentSkillWrite{Name: op.Name, Content: op.Content, Note: op.Note}
		switch op.Op {
		case "write":
			if has {
				w.Expect = k.Version
			}
		case "retire":
			if !has {
				refused = append(refused, refusedWrite{Index: i, Op: op.Op, Ref: op.Name, Error: "only a skill this run has can be retired"})
				continue
			}
			w.Retire, w.Expect = true, k.Version
		default:
			refused = append(refused, refusedWrite{Index: i, Op: op.Op, Ref: op.Name, Error: "op must be write or retire"})
			continue
		}
		writes = append(writes, w)
	}
	return writes, refused
}

// skillsContract is the system prompt's part about skills.
func skillsContract(skills []store.Skill, cliSkills bool) string {
	var b strings.Builder
	b.WriteString("\n- skills: your Claude Code skills, procedures kept for you between runs.")
	if len(skills) > 0 {
		names := make([]string, 0, len(skills))
		for _, k := range skills {
			names = append(names, k.Name)
		}
		if cliSkills {
			fmt.Fprintf(&b, " You have: %s (use the Skill tool when one fits).", strings.Join(names, ", "))
		} else {
			fmt.Fprintf(&b, " You have: %s, in full in the skills input.", strings.Join(names, ", "))
		}
	}
	b.WriteString(" Write one when you have worked out a procedure your person will want done the same way again, or improve one you have; it is applied directly (every version is kept, and your person can restore any). op write gives the whole SKILL.md: frontmatter with name and a description saying when to use it, then the steps; a skill you have keeps its other files. op retire ends one you have. Return an empty list when there is nothing to change.\n")
	return b.String()
}

// recordSkillWrites stores what became of a run's skill writes as its skill-writes.json artifact, in output order.
func (e *Engine) recordSkillWrites(ctx context.Context, c *store.ClaimedRun, ops []OutputSkill, writes []*store.AgentSkillWrite, refused []refusedWrite) {
	if len(ops) == 0 {
		return
	}
	type row struct {
		Op     string                 `json:"op"`
		Name   string                 `json:"name"`
		Result store.EntryWriteResult `json:"result"`
	}
	bad := map[int]string{}
	for _, r := range refused {
		bad[r.Index] = r.Error
	}
	rows := make([]row, len(ops))
	next := 0
	for i, op := range ops {
		rows[i] = row{Op: op.Op, Name: op.Name}
		if msg, ok := bad[i]; ok {
			rows[i].Result = store.EntryWriteResult{Outcome: "refused", Error: msg}
		} else if next < len(writes) {
			rows[i].Result = writes[next].Result
			next++
		}
	}
	b, _ := json.MarshalIndent(rows, "", "  ")
	if err := e.Store.PutArtifact(context.WithoutCancel(ctx), c.ID, c.Attempt, "out", "skill-writes.json", "application/json", b); err != nil {
		e.Log.Warn("record skill writes", "run", c.ID, "err", err)
	}
}
