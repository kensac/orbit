package engine

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/actions"
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

// skillProposal resolves a run's skill:write against the skills the run had: a write to one it has applies against that version, a write to a new name creates it, a retire must name one it has. It sets the params' version and the change code worked out (skillChanges), or returns why it is denied. A run proposes at most maxSkillWrites.
func skillProposal(had []store.Skill, params json.RawMessage, count int) (json.RawMessage, error) {
	if count > maxSkillWrites {
		return nil, fmt.Errorf("at most %d skill writes per run", maxSkillWrites)
	}
	w, err := actions.DecodeSkillWrite(params)
	if err != nil {
		return nil, err
	}
	var cur *store.Skill
	for i := range had {
		if had[i].Name == w.Name {
			cur = &had[i]
		}
	}
	w.Version, w.Changes = 0, nil
	switch {
	case w.Op == "retire" && cur == nil:
		return nil, errors.New("only a skill this run has can be retired")
	case cur != nil:
		w.Version = cur.Version
	}
	if w.Op == "write" {
		if _, err := store.SkillDescription(w.Name, w.Content); err != nil {
			return nil, err
		}
	}
	w.Changes = skillChanges(cur, w)
	return json.Marshal(w)
}

// skillChangesDiffMax bounds the diff an approval shows: a page, with the counts saying how much more there is.
const skillChangesDiffMax = 16 << 10

// skillChanges is what a write changes: SKILL.md's lines (a retire removes none; it ends the skill), the files the skill keeps, and which of them are scripts or executables.
func skillChanges(cur *store.Skill, w actions.SkillWrite) *actions.SkillChanges {
	ch := &actions.SkillChanges{Files: []string{}, Scripts: []string{}}
	before := ""
	if cur != nil {
		ch.From, before = cur.Version, cur.Content
		for _, f := range cur.Files {
			ch.Files = append(ch.Files, f.Path)
			if isScript(f) {
				ch.Scripts = append(ch.Scripts, f.Path)
			}
		}
	}
	if w.Op == "write" {
		ch.Diff, ch.Added, ch.Removed = changedLines(before, w.Content, skillChangesDiffMax)
	}
	return ch
}

// scriptExts are file extensions of code a shell or interpreter runs.
var scriptExts = []string{".sh", ".bash", ".zsh", ".fish", ".py", ".rb", ".pl", ".php", ".js", ".mjs", ".cjs", ".ts", ".lua", ".ps1", ".bat", ".cmd", ".exe", ".bin", ".so", ".dylib"}

// isScript reports whether a skill's file is code that can run: under scripts/ or bin/, a script's extension, a #! line, or an ELF, Mach-O or PE header.
func isScript(f store.SkillFile) bool {
	p := strings.ToLower(f.Path)
	if strings.HasPrefix(p, "scripts/") || strings.HasPrefix(p, "bin/") || strings.Contains(p, "/scripts/") || strings.Contains(p, "/bin/") {
		return true
	}
	for _, ext := range scriptExts {
		if strings.HasSuffix(p, ext) {
			return true
		}
	}
	for _, magic := range [][]byte{[]byte("#!"), {0x7f, 'E', 'L', 'F'}, {0xcf, 0xfa, 0xed, 0xfe}, {0xfe, 0xed, 0xfa, 0xcf}, {0xca, 0xfe, 0xba, 0xbe}, []byte("MZ")} {
		if bytes.HasPrefix(f.Content, magic) {
			return true
		}
	}
	return false
}

// lineDiffMax bounds the lines a diff compares exactly; a longer SKILL.md is shown as a whole replacement.
const lineDiffMax = 4000

// changedLines is a diff of b against a by lines (a longest common subsequence): each changed line prefixed + or -, and the counts. It is cut at limit bytes, saying how many changed lines it leaves out.
func changedLines(a, b string, limit int) (string, int, int) {
	al, bl := splitLines(a), splitLines(b)
	var ops []string
	if len(al) > lineDiffMax || len(bl) > lineDiffMax {
		for _, l := range al {
			ops = append(ops, "-"+l)
		}
		for _, l := range bl {
			ops = append(ops, "+"+l)
		}
	} else {
		n, m := len(al), len(bl)
		lcs := make([][]int, n+1)
		for i := range lcs {
			lcs[i] = make([]int, m+1)
		}
		for i := n - 1; i >= 0; i-- {
			for j := m - 1; j >= 0; j-- {
				if al[i] == bl[j] {
					lcs[i][j] = lcs[i+1][j+1] + 1
				} else {
					lcs[i][j] = max(lcs[i+1][j], lcs[i][j+1])
				}
			}
		}
		i, j := 0, 0
		for i < n || j < m {
			switch {
			case i < n && j < m && al[i] == bl[j]:
				ops = append(ops, " "+al[i])
				i, j = i+1, j+1
			case j < m && (i == n || lcs[i][j+1] >= lcs[i+1][j]):
				ops = append(ops, "+"+bl[j])
				j++
			default:
				ops = append(ops, "-"+al[i])
				i++
			}
		}
	}
	added, removed, left := 0, 0, 0
	var out strings.Builder
	for _, op := range ops {
		switch op[0] {
		case '+':
			added++
		case '-':
			removed++
		default:
			continue
		}
		if left > 0 || out.Len()+len(op)+1 > limit {
			left++
			continue
		}
		out.WriteString(op + "\n")
	}
	if left > 0 {
		fmt.Fprintf(&out, "(%d more changed lines not shown)\n", left)
	}
	return out.String(), added, removed
}

func splitLines(s string) []string {
	if s == "" {
		return nil
	}
	return strings.Split(strings.TrimSuffix(s, "\n"), "\n")
}

// skillApprovalText is what a person approving a skill:write is told, before the params: what it writes, how SKILL.md changes, and every script or executable the skill holds.
func skillApprovalText(params json.RawMessage) string {
	w, err := actions.DecodeSkillWrite(params)
	if err != nil || w.Changes == nil {
		return ""
	}
	var b strings.Builder
	switch {
	case w.Op == "retire":
		fmt.Fprintf(&b, "Retires skill %s (version %d).\n", w.Name, w.Changes.From)
	case w.Changes.From == 0:
		fmt.Fprintf(&b, "Creates skill %s: SKILL.md, %d lines.\n", w.Name, w.Changes.Added)
	default:
		fmt.Fprintf(&b, "Writes skill %s (version %d to %d): SKILL.md +%d -%d lines; its %d other files stay.\n", w.Name, w.Changes.From, w.Changes.From+1, w.Changes.Added, w.Changes.Removed, len(w.Changes.Files))
	}
	if len(w.Changes.Scripts) > 0 {
		fmt.Fprintf(&b, "Scripts and executables in this skill (they run only if the agent's tools let them, when SKILL.md says to): %s\n", strings.Join(w.Changes.Scripts, ", "))
	}
	if w.Changes.Diff != "" {
		b.WriteString("\n```diff\n" + w.Changes.Diff + "```\n")
	}
	return b.String()
}

// skillWriteAction executes a skill:write: the write applies against the version the run had, as the next version with the run as its source, and a written skill is attached to the agent. Its result says what it did and what changed.
func (e *Engine) skillWriteAction(ctx context.Context, c actions.Call, w actions.SkillWrite) (json.RawMessage, error) {
	sw := &store.AgentSkillWrite{Name: w.Name, Content: w.Content, Expect: w.Version, Retire: w.Op == "retire", Note: w.Note}
	if err := e.Store.ApplySkillWrite(ctx, c.RunID, c.Agent, sw); err != nil {
		return nil, err
	}
	if sw.Result.Outcome == "refused" {
		return nil, fmt.Errorf("skill:write %s: %s", w.Name, sw.Result.Error)
	}
	return json.Marshal(map[string]any{"skill": w.Name, "op": w.Op, "outcome": sw.Result.Outcome, "version": sw.Result.Version, "changes": w.Changes})
}

// skillsContract is the system prompt's part about skills: which the run has, and, when skill:write is offered, that skills are written with it.
func skillsContract(skills []store.Skill, cliSkills, canWrite bool) string {
	if len(skills) == 0 && !canWrite {
		return ""
	}
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
	if canWrite {
		fmt.Fprintf(&b, " Write or retire one with the %s action.", actions.SkillWriteType)
	}
	b.WriteString("\n")
	return b.String()
}
