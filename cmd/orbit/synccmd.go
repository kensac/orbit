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
	if err := fs.Parse(args[1:]); err != nil {
		return err
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
