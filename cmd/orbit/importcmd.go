package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/kensac/orbit/internal/claudeimport"
	"github.com/kensac/orbit/internal/store"
)

// importCmd seeds orbit from Claude Code's files on disk, through the API:
//
//	orbit import claude-memory <dir> --scope <profile|topic:NAME|agent:NAME> [--profile-types user] [--apply]
//	orbit import skills <dir> [--only a,b] [--attach agent] [--apply]
//
// Without --apply it is a dry run: the server reports what it would create, update or leave alone, and writes nothing. --print shows what was read from disk without calling the server at all.
func importCmd(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return errors.New("import: say what to import: claude-memory <dir>")
	}
	switch args[0] {
	case "claude-memory":
		return importMemory(ctx, args[1:])
	case "skills":
		return importSkills(ctx, args[1:])
	}
	return fmt.Errorf("import: unknown kind %q (want claude-memory or skills)", args[0])
}

// apiFlags are how an operator command reaches an orbit API: by default the internal listener inside the container, with the operator token.
type apiFlags struct {
	url, token string
}

func (f *apiFlags) register(fs *flag.FlagSet) {
	fs.StringVar(&f.url, "api", env("ORBIT_API_URL", "http://"+local(env("ORBIT_INTERNAL_LISTEN", "127.0.0.1:8081"))),
		"the orbit API's base URL (ORBIT_API_URL)")
	fs.StringVar(&f.token, "token", os.Getenv("ORBIT_API_TOKEN"),
		"a bearer token: the operator token on the internal listener, or an authentik access token (ORBIT_API_TOKEN)")
}

// call sends one JSON request and decodes the response into out.
func (f *apiFlags) call(ctx context.Context, method, path string, in, out any) error {
	if f.token == "" {
		return errors.New("no token: pass --token or set ORBIT_API_TOKEN")
	}
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimRight(f.url, "/")+path, bytes.NewReader(b))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, strings.TrimSpace(string(body)))
	}
	return json.Unmarshal(body, out)
}

// parseScope reads profile, topic:NAME or agent:NAME.
func parseScope(s string) (scope, topic, agent string, err error) {
	kind, name, _ := strings.Cut(s, ":")
	switch {
	case kind == store.ScopeProfile && name == "":
		return store.ScopeProfile, "", "", nil
	case kind == store.ScopeTopic && name != "":
		return store.ScopeTopic, name, "", nil
	case kind == store.ScopeAgent && name != "":
		return store.ScopeAgent, "", name, nil
	}
	return "", "", "", fmt.Errorf("--scope must be profile, topic:<name> or agent:<name>, not %q", s)
}

func importMemory(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import claude-memory", flag.ContinueOnError)
	var api apiFlags
	api.register(fs)
	scopeFlag := fs.String("scope", "", "where the entries go: profile, topic:<name> or agent:<name> (required)")
	profileTypes := fs.String("profile-types", "user", "comma list of memory types that go to the profile whatever --scope says ('' for none)")
	apply := fs.Bool("apply", false, "write the entries; without it the server only reports what it would do")
	printOnly := fs.Bool("print", false, "print the entries read from disk as JSON and stop, without calling the server")
	dir, rest := "", args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		dir, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if dir == "" && fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if dir == "" {
		return errors.New("import claude-memory: name the memory directory (~/.claude/projects/<project>/memory)")
	}
	scope, topic, agent, err := parseScope(*scopeFlag)
	if err != nil {
		return err
	}
	toProfile := map[string]bool{}
	for _, t := range strings.Split(*profileTypes, ",") {
		if t = strings.TrimSpace(t); t != "" {
			toProfile[t] = true
		}
	}
	mems, warnings, err := claudeimport.ReadMemoryDir(dir)
	if err != nil {
		return err
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	type entry struct {
		Scope string `json:"scope"`
		Topic string `json:"topic,omitempty"`
		Agent string `json:"agent,omitempty"`
		Key   string `json:"key"`
		store.EntryFields
	}
	entries := make([]entry, 0, len(mems))
	for _, m := range mems {
		e := entry{Scope: scope, Topic: topic, Agent: agent, Key: m.Key, EntryFields: m.EntryFields}
		if toProfile[m.Type] {
			e.Scope, e.Topic, e.Agent = store.ScopeProfile, "", ""
		}
		entries = append(entries, e)
	}
	if *printOnly {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(entries)
	}
	var out struct {
		DryRun  bool                 `json:"dry_run"`
		Results []store.ImportResult `json:"results"`
	}
	abs, _ := filepath.Abs(dir)
	if err := api.call(ctx, http.MethodPost, "/api/v1/memory/import", map[string]any{
		"dry_run": !*apply, "entries": entries, "note": "imported from Claude Code memory " + filepath.Base(filepath.Dir(abs)),
	}, &out); err != nil {
		return err
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 4, 2, ' ', 0)
	counts := map[string]int{}
	for _, r := range out.Results {
		where := r.Scope
		if r.Topic != "" {
			where += ":" + r.Topic
		}
		if r.Agent != "" {
			where += ":" + r.Agent
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\n", r.Outcome, where, r.Key)
		counts[r.Outcome]++
	}
	_ = tw.Flush()
	verb := "would be"
	if !out.DryRun {
		verb = "were"
	}
	fmt.Printf("\n%d created, %d updated, %d unchanged (%s); %d skipped\n", counts["created"], counts["updated"], counts["unchanged"], verb, len(warnings))
	if out.DryRun {
		fmt.Println("dry run: nothing was written; run again with --apply")
	}
	return nil
}

// importSkills sends each skill directory under dir to POST /api/v1/skills/import, and with --attach attaches them to an agent.
func importSkills(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("import skills", flag.ContinueOnError)
	var api apiFlags
	api.register(fs)
	only := fs.String("only", "", "comma list of skill names to import (default: every skill under the directory)")
	attach := fs.String("attach", "", "an agent to attach the imported skills to (with --apply)")
	apply := fs.Bool("apply", false, "write the skills; without it the server only reports what it would do")
	printOnly := fs.Bool("print", false, "list the skills read from disk and stop, without calling the server")
	dir, rest := "", args
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		dir, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if dir == "" && fs.NArg() > 0 {
		dir = fs.Arg(0)
	}
	if dir == "" {
		return errors.New("import skills: name the skills directory (~/.claude/skills or a project's .claude/skills)")
	}
	skills, warnings, err := claudeimport.ReadSkillsDir(dir)
	if err != nil {
		return err
	}
	if *only != "" {
		want := map[string]bool{}
		for _, n := range strings.Split(*only, ",") {
			want[strings.TrimSpace(n)] = true
		}
		var kept []claudeimport.Skill
		for _, k := range skills {
			if want[k.Name] {
				kept = append(kept, k)
				delete(want, k.Name)
			}
		}
		for n := range want {
			warnings = append(warnings, n+": not found under "+dir)
		}
		skills = kept
	}
	for _, w := range warnings {
		fmt.Fprintln(os.Stderr, "warning:", w)
	}
	if *printOnly {
		for _, k := range skills {
			size := len(k.Content)
			for _, f := range k.Files {
				size += len(f.Content)
			}
			fmt.Printf("%s\t%d files\t%d bytes\n", k.Name, len(k.Files), size)
		}
		return nil
	}
	var out struct {
		DryRun  bool                      `json:"dry_run"`
		Results []store.SkillImportResult `json:"results"`
	}
	abs, _ := filepath.Abs(dir)
	if err := api.call(ctx, http.MethodPost, "/api/v1/skills/import", map[string]any{
		"dry_run": !*apply, "skills": skills, "note": "imported from " + filepath.Base(abs),
	}, &out); err != nil {
		return err
	}
	counts := map[string]int{}
	for _, r := range out.Results {
		fmt.Printf("%s\t%s\tversion %d\n", r.Outcome, r.Name, r.Version)
		counts[r.Outcome]++
	}
	verb := "would be"
	if !out.DryRun {
		verb = "were"
	}
	fmt.Printf("\n%d created, %d updated, %d unchanged (%s); %d skipped\n", counts["created"], counts["updated"], counts["unchanged"], verb, len(warnings))
	if out.DryRun {
		fmt.Println("dry run: nothing was written; run again with --apply")
		return nil
	}
	if *attach != "" {
		for _, r := range out.Results {
			var res map[string]any
			if err := api.call(ctx, http.MethodPut, "/api/v1/agents/"+*attach+"/skills/"+r.Name, nil, &res); err != nil {
				return err
			}
			fmt.Printf("attached %s to %s\n", r.Name, *attach)
		}
	}
	return nil
}
