package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
	"time"

	"github.com/kensac/orbit/internal/claudesync"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/store"
)

// syncCmd writes orbit's skills and memory entries into Claude Code's files on this machine, the reverse of orbit import:
//
//	orbit sync claude [--skills-dir ~/.claude/skills] [--memory-dir <dir> --scope profile,topic:NAME] [--only a,b] [--dry-run | --apply <digest>]
//	orbit sync claude --workspace <id> --account <id> [--claude-dir <dir>] [--apply <digest>]
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
	workspaceID := fs.String("workspace", "", "box workspace id for account-scoped Claude config import")
	accountID := fs.String("account", "", "Claude account id for --workspace import")
	if err := fs.Parse(args[1:]); err != nil {
		return err
	}
	if *workspaceID != "" || *accountID != "" {
		if *workspaceID == "" || *accountID == "" {
			return errors.New("--workspace and --account must be used together")
		}
		if *bundle != "" || *memoryDir != "" || *only != "" || *skillsDir != filepath.Join(home, ".claude", "skills") {
			return errors.New("box import cannot be combined with --bundle, --skills-dir, --memory-dir or --only")
		}
		return importClaudeConfigToBox(ctx, &api, *workspaceID, *accountID, *claudeDir, *apply, stdout)
	}
	if *bundle != "" {
		if *memoryDir != "" || *only != "" || *skillsDir != filepath.Join(home, ".claude", "skills") {
			return errors.New("--bundle cannot be combined with --skills-dir, --memory-dir or --only")
		}
		return exportClaudeBundle(*bundle, *claudeDir, *apply, stdout)
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

func importClaudeConfigToBox(ctx context.Context, api *apiFlags, workspaceID, accountID, claudeDir, apply string, stdout io.Writer) error {
	if !store.IsUUID(workspaceID) || !store.IsUUID(accountID) {
		return errors.New("--workspace and --account must be UUIDs")
	}
	bundle, err := claudesync.NewBoxBundle(claudeDir)
	if err != nil {
		return err
	}
	endpoint := "/api/v1/workspaces/" + url.PathEscape(workspaceID) + "/accounts/" + url.PathEscape(accountID) + "/claude-config"
	var response struct {
		Config sandbox.ClaudeConfigStatus `json:"config"`
	}
	if err := api.call(ctx, http.MethodGet, endpoint, nil, &response); err != nil {
		return err
	}
	if !validClaudeConfigStatus(response.Config) {
		return errors.New("box returned an invalid Claude config manifest")
	}
	plan := claudeBoxPlanDigest(api.url, workspaceID, accountID, bundle.Digest, response.Config.Digest)
	tw := tabwriter.NewWriter(stdout, 0, 4, 2, ' ', 0)
	for _, file := range bundle.Files {
		_, _ = fmt.Fprintf(tw, "include\t%s\t%d bytes\n", file.Path, file.Bytes)
	}
	for _, warning := range bundle.Warnings {
		_, _ = fmt.Fprintf(tw, "skip\t%s\n", warning)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintf(stdout, "\nbox account %s, %d files, %d archive bytes\ntarget %s\nplan %s\n", accountID, len(bundle.Files), len(bundle.Archive), response.Config.Digest, plan)
	if apply == "" {
		_, _ = fmt.Fprintf(stdout, "dry run: nothing was sent; run again with --apply %s\n", plan)
		return nil
	}
	if apply != plan {
		return fmt.Errorf("the box import plan changed: --apply %s does not match current plan %s", apply, plan)
	}
	var applied struct {
		Config sandbox.ClaudeConfigStatus `json:"config"`
	}
	if err := api.putClaudeConfig(ctx, endpoint, response.Config.Digest, bundle.Digest, bundle.Archive, &applied); err != nil {
		return err
	}
	if !validClaudeConfigStatus(applied.Config) {
		return errors.New("box returned an invalid updated Claude config manifest")
	}
	_, _ = fmt.Fprintf(stdout, "applied\t%s\n", applied.Config.Digest)
	return nil
}

func claudeBoxPlanDigest(apiURL, workspaceID, accountID, bundleDigest, targetDigest string) string {
	material := strings.Join([]string{"orbit-claude-box-plan-v1", strings.TrimRight(apiURL, "/"), workspaceID, accountID, bundleDigest, targetDigest}, "\x00")
	h := sha256.Sum256([]byte(material))
	return hex.EncodeToString(h[:])
}

func validClaudePlanDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	for _, c := range digest {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

func validClaudeConfigStatus(status sandbox.ClaudeConfigStatus) bool {
	return validClaudePlanDigest(status.Digest) && status.Digest == sandbox.ClaudeConfigDigest(status.Files)
}

func (f *apiFlags) putClaudeConfig(ctx context.Context, path, expected, bundleDigest string, archive []byte, out any) error {
	if f.token == "" {
		return errors.New("no token: pass --token or set ORBIT_API_TOKEN")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, strings.TrimRight(f.url, "/")+path, bytes.NewReader(archive))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/x-tar")
	req.Header.Set("If-Match", expected)
	req.Header.Set("X-Orbit-Bundle-SHA256", bundleDigest)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("PUT %s: %s: %s", path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
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
