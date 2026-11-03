package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"

	"github.com/kensac/orbit/internal/claudesync"
	"github.com/kensac/orbit/internal/store"
)

// syncCmd writes orbit's skills and memory entries into Claude Code's files on this machine, the reverse of orbit import:
//
//	orbit sync claude [--skills-dir ~/.claude/skills] [--memory-dir <dir> --scope profile,topic:NAME] [--only a,b] [--dry-run | --apply <digest>]
//
// It always plans first and prints the plan with its digest; --apply <digest> writes only when the plan is still that one.
func syncCmd(ctx context.Context, args []string, stdout io.Writer) error {
	if len(args) == 0 || args[0] != "claude" {
		return errors.New("sync: say what to sync: claude")
	}
	fs := flag.NewFlagSet("sync claude", flag.ContinueOnError)
	var api apiFlags
	api.register(fs)
	home, _ := os.UserHomeDir()
	skillsDir := fs.String("skills-dir", filepath.Join(home, ".claude", "skills"), "where skills go, one directory each ('' for none)")
	memoryDir := fs.String("memory-dir", "", "a Claude Code memory directory (~/.claude/projects/<project>/memory) for memory entries ('' for none)")
	scopes := fs.String("scope", store.ScopeProfile, "comma list of the entries to write: profile, topic:<name>, agent:<name>")
	only := fs.String("only", "", "comma list of skill names to sync (default: every skill)")
	_ = fs.Bool("dry-run", true, "plan and print, writing nothing (the default)")
	apply := fs.String("apply", "", "write the plan whose digest a dry run printed, if it is still the plan")
	bundle := fs.String("bundle", "", "write a portable CLAUDE.md and skills tar for a box (no account state or other config)")
	claudeDir := fs.String("claude-dir", filepath.Join(home, ".claude"), "the local Claude config directory used by --bundle")
	mcpPatch := fs.String("mcp-patch", "", "write a reviewable credential-free orbit.yaml patch from local remote MCP declarations")
	mcpConfig := fs.String("mcp-config", filepath.Join(home, ".claude.json"), "local Claude config read by --mcp-patch")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *bundle != "" {
		if *mcpPatch != "" || *memoryDir != "" || *only != "" || *skillsDir != filepath.Join(home, ".claude", "skills") {
			return errors.New("--bundle cannot be combined with --mcp-patch, --skills-dir, --memory-dir or --only")
		}
		return exportClaudeBundle(*bundle, *claudeDir, *apply, stdout)
	}
	if *mcpPatch != "" {
		if *memoryDir != "" || *only != "" || *skillsDir != filepath.Join(home, ".claude", "skills") {
			return errors.New("--mcp-patch cannot be combined with --skills-dir, --memory-dir or --only")
		}
		return exportClaudeMCPConfigPatch(*mcpPatch, *mcpConfig, *apply, stdout)
	}
	var ops []claudesync.Op
	if *skillsDir != "" {
		skills, err := fetchSkills(ctx, &api, *only)
		if err != nil {
			return err
		}
		ops = append(ops, claudesync.PlanSkills(*skillsDir, skills)...)
	}
	if *memoryDir != "" {
		entries, err := fetchEntries(ctx, &api, *scopes)
		if err != nil {
			return err
		}
		mem, err := claudesync.PlanMemory(*memoryDir, entries)
		if err != nil {
			return err
		}
		ops = append(ops, mem...)
	}
	plan := claudesync.NewPlan(ops)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	counts := map[string]int{}
	for _, o := range plan.Ops {
		if o.Kind != claudesync.KindIndex {
			// MEMORY.md follows the entries; it is listed, not counted.
			counts[o.Action]++
		}
		if o.Action == claudesync.Unchanged {
			continue
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", o.Action, o.Kind, o.Path, o.Why)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(stdout, "\n%d to create, %d to update, %d to remove, %d unchanged, %d changed locally (left alone)\nplan %s\n",
		counts[claudesync.Create], counts[claudesync.Update], counts[claudesync.Remove], counts[claudesync.Unchanged], counts[claudesync.ChangedLocally], plan.Digest)
	if *apply == "" {
		_, _ = fmt.Fprintf(stdout, "dry run: nothing was written; run again with --apply %s\n", plan.Digest)
		return nil
	}
	if *apply != plan.Digest {
		return fmt.Errorf("the plan is no longer %s (it is now %s): orbit or a local file changed; read the plan above and apply that one", *apply, plan.Digest)
	}
	failed := 0
	for _, r := range claudesync.Apply(plan) {
		if r.Error != "" {
			failed++
			_, _ = fmt.Fprintf(stdout, "failed\t%s\t%s\n", r.Path, r.Error)
			continue
		}
		_, _ = fmt.Fprintf(stdout, "%s\t%s\n", pastTense(r.Action), r.Path)
	}
	if failed > 0 {
		return fmt.Errorf("%d of the plan's writes failed", failed)
	}
	return nil
}

func exportClaudeMCPConfigPatch(dest, source, apply string, stdout io.Writer) error {
	f, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("read local Claude config %q: %w", source, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return fmt.Errorf("inspect local Claude config %q: %w", source, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 1<<20 {
		return fmt.Errorf("local Claude config %q must be a regular file no larger than 1048576 bytes", source)
	}
	data, err := io.ReadAll(io.LimitReader(f, (1<<20)+1))
	if err != nil {
		return fmt.Errorf("read local Claude config %q: %w", source, err)
	}
	patch, err := claudesync.NewMCPConfigPatch(data)
	if err != nil {
		return err
	}
	_, _ = fmt.Fprintf(stdout, "%d credential-free HTTPS MCP declarations\npatch bytes %d\nplan %s\n", patch.Servers, len(patch.Config), patch.Digest)
	if apply == "" {
		_, _ = fmt.Fprintf(stdout, "dry run: nothing was written; run again with --apply %s\n", patch.Digest)
		return nil
	}
	if apply != patch.Digest {
		return fmt.Errorf("the MCP config patch changed: --apply %s does not match plan %s", apply, patch.Digest)
	}
	out, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create MCP config patch %q: %w", dest, err)
	}
	if _, err := out.Write(patch.Config); err != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("write MCP config patch %q: %w", dest, err)
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("sync MCP config patch %q: %w", dest, err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("close MCP config patch %q: %w", dest, err)
	}
	_, _ = fmt.Fprintf(stdout, "created\t%s\n", dest)
	return nil
}

func exportClaudeBundle(dest, source, apply string, stdout io.Writer) error {
	bundle, err := claudesync.NewBoxBundle(source)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	for _, f := range bundle.Files {
		_, _ = fmt.Fprintf(tw, "include\t%s\t%d bytes\n", f.Path, f.Bytes)
	}
	for _, warning := range bundle.Warnings {
		_, _ = fmt.Fprintf(tw, "skip\t%s\n", warning)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(stdout, "\n%d files, %d archive bytes\nplan %s\n", len(bundle.Files), len(bundle.Archive), bundle.Digest)
	if apply == "" {
		_, _ = fmt.Fprintf(stdout, "dry run: nothing was written; run again with --apply %s\n", bundle.Digest)
		return nil
	}
	if apply != bundle.Digest {
		return fmt.Errorf("the bundle changed: --apply %s does not match plan %s", apply, bundle.Digest)
	}
	f, err := os.OpenFile(dest, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create bundle %q: %w", dest, err)
	}
	if _, err := f.Write(bundle.Archive); err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("write bundle %q: %w", dest, err)
	}
	if err := f.Sync(); err != nil {
		_ = f.Close()
		_ = os.Remove(dest)
		return fmt.Errorf("sync bundle %q: %w", dest, err)
	}
	if err := f.Close(); err != nil {
		_ = os.Remove(dest)
		return fmt.Errorf("close bundle %q: %w", dest, err)
	}
	_, _ = fmt.Fprintf(stdout, "created\t%s\n", dest)
	return nil
}

func pastTense(action string) string {
	return map[string]string{claudesync.Create: "created", claudesync.Update: "updated", claudesync.Remove: "removed"}[action]
}

// fetchSkills reads every skill (active and retired) with its versions' digests, and the current version in full.
func fetchSkills(ctx context.Context, api *apiFlags, only string) ([]claudesync.SkillState, error) {
	var list struct {
		Skills []store.Skill `json:"skills"`
	}
	if err := api.call(ctx, http.MethodGet, "/api/v1/skills?status=all", nil, &list); err != nil {
		return nil, err
	}
	want := map[string]bool{}
	for n := range strings.SplitSeq(only, ",") {
		if n = strings.TrimSpace(n); n != "" {
			want[n] = true
		}
	}
	var out []claudesync.SkillState
	for _, k := range list.Skills {
		if len(want) > 0 && !want[k.Name] {
			continue
		}
		var one struct {
			Skill    store.Skill   `json:"skill"`
			Versions []store.Skill `json:"versions"`
		}
		if err := api.call(ctx, http.MethodGet, "/api/v1/skills/"+url.PathEscape(k.Name), nil, &one); err != nil {
			return nil, err
		}
		out = append(out, claudesync.SkillState{Current: one.Skill, Versions: one.Versions})
	}
	return out, nil
}

// fetchEntries reads every entry (active and retired) of the scopes, each with every version.
func fetchEntries(ctx context.Context, api *apiFlags, scopes string) ([]claudesync.EntryState, error) {
	var out []claudesync.EntryState
	for sc := range strings.SplitSeq(scopes, ",") {
		if sc = strings.TrimSpace(sc); sc == "" {
			continue
		}
		scope, topic, agent, err := parseScope(sc)
		if err != nil {
			return nil, err
		}
		q := url.Values{"scope": {scope}, "status": {"all"}, "limit": {"1000"}}
		if topic != "" {
			q.Set("topic", topic)
		}
		if agent != "" {
			q.Set("agent", agent)
		}
		var list struct {
			Entries []store.Entry `json:"entries"`
		}
		if err := api.call(ctx, http.MethodGet, "/api/v1/memory/entries?"+q.Encode(), nil, &list); err != nil {
			return nil, err
		}
		for _, e := range list.Entries {
			var one struct {
				Entry    store.Entry   `json:"entry"`
				Versions []store.Entry `json:"versions"`
			}
			if err := api.call(ctx, http.MethodGet, "/api/v1/memory/entries/"+e.ID, nil, &one); err != nil {
				return nil, err
			}
			out = append(out, claudesync.EntryState{Current: one.Entry, Versions: one.Versions})
		}
	}
	return out, nil
}
