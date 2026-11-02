package build

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/mail"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/spec"
)

// Sandbox is sandboxd's four operations (*sandbox.Client).
type Sandbox interface {
	Create(ctx context.Context, runID string, archive io.Reader) (sandbox.CreateResponse, error)
	Attach(ctx context.Context, id string, ar sandbox.AttachRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Archive(ctx context.Context, id string) (io.ReadCloser, error)
	Destroy(ctx context.Context, id string) error
}

// Identity is a commit author.
type Identity struct{ Name, Email string }

// ParseIdentity reads "Name <email>".
func ParseIdentity(s string) (Identity, error) {
	a, err := mail.ParseAddress(s)
	if err != nil {
		return Identity{}, fmt.Errorf("author %q: %w", s, err)
	}
	return Identity{Name: a.Name, Email: a.Address}, nil
}

// Job is one build run, fully resolved: nothing in it is chosen by a model.
type Job struct {
	RunID   string
	Attempt int
	// TicketID names the branch (Branch).
	TicketID    string
	Title       string
	Repo        string
	Plan        string
	PlanVersion int
	// Ticket is the ticket as data for the prompt (ticket.json).
	Ticket json.RawMessage
	// Purpose is "build" (from the default branch) or "fix" (on top of the
	// PR's head, with Failure).
	Purpose string
	Failure json.RawMessage
	Policy  spec.BuildRepo
	// Model and MaxTurns are the build agent's runner's; Timeout its.
	Model    string
	MaxTurns int
	Timeout  time.Duration
	// Credential is the backend credential the CLI needs
	// (CLAUDE_CODE_OAUTH_TOKEN), the only secret the sandbox receives.
	Credential string
	Author     Identity
}

// Deps is what a build reaches: GitHub as the App, sandboxd, and orbit's
// records.
type Deps struct {
	App     *github.App
	Sandbox Sandbox
	// Emit records one event of the run's stream.
	Emit func(kind string, payload []byte)
	// Artifact stores one of the run's artifacts.
	Artifact func(direction, name, mediaType string, content []byte) error
	// Record stores a permission decision before it is answered.
	Record func(PermissionRequest, Decision) error
}

// Result is a delivered build.
type Result struct {
	Output   Output
	BaseSHA  string
	HeadSHA  string
	Branch   string
	PRNumber int
	PRURL    string
	Diff     *Diff
	// TrustBoundary lists changed paths that change what orbit itself may
	// do (only for orbit's own repository).
	TrustBoundary []string
	CostUSD       *float64
	Usage         json.RawMessage
	// Gate, when set, says why the model was not called.
	Gate string
}

// Runner runs build jobs.
type Runner struct {
	// WorkRoot holds one clone per attempt, removed when it ends.
	WorkRoot string
	// Git is the git binary ("git").
	Git string
	// Binary is the CLI inside the sandbox ("claude").
	Binary string
	// CloneURL is where a repository is fetched from and pushed to;
	// https://github.com/<repo>.git by default (tests point it elsewhere).
	CloneURL func(repo string) string
}

// trustGlobs are the paths of orbit's own repository whose change alters
// what orbit may do (docs/tickets.md, Self-hosting). A PR touching one says
// so, in its body and in the merge's approval.
var trustGlobs = []string{
	"internal/actions/**", "internal/github/**", "internal/runner/**", "internal/sandbox/**",
	"internal/sandboxd/**", "internal/egress/**", "internal/build/**", "internal/harness/**",
	"deploy/**", "AGENTS.md", "docs/agents/security.md",
}

// TrustBoundary returns the changed paths of repo that touch orbit's trust
// boundary.
func TrustBoundary(repo string, paths []string) []string {
	if repo != spec.SelfRepo {
		return nil
	}
	var out []string
	for _, p := range paths {
		for _, g := range trustGlobs {
			if MatchGlob(g, p) {
				out = append(out, p)
				break
			}
		}
	}
	return out
}

func (r *Runner) cloneURL(repo string) string {
	if r.CloneURL != nil {
		return r.CloneURL(repo)
	}
	return "https://github.com/" + repo + ".git"
}

func (d Deps) emit(kind string, v any) {
	if d.Emit == nil {
		return
	}
	b, _ := json.Marshal(v)
	d.Emit(kind, b)
}

// Run builds one job end to end: clone, sandbox, the CLI under orbit's
// permission handler, readback, the diff policy, commit, push and the PR.
// It returns the delivered result or why nothing was delivered.
func (r *Runner) Run(ctx context.Context, j Job, d Deps) (*Result, error) {
	if j.Credential == "" {
		return nil, errors.New("the build needs CLAUDE_CODE_OAUTH_TOKEN for the CLI in the sandbox")
	}
	if d.App == nil || d.Sandbox == nil {
		return nil, errors.New("the build needs the GitHub App and sandboxd")
	}
	branch, err := Branch(j.TicketID)
	if err != nil {
		return nil, err
	}
	work := filepath.Join(r.WorkRoot, fmt.Sprintf("%s-%d", j.RunID, j.Attempt))
	if err := os.RemoveAll(work); err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Join(work, "home"), 0o700); err != nil {
		return nil, err
	}
	defer os.RemoveAll(work)
	bin := r.Git
	if bin == "" {
		bin = "git"
	}
	top := git{bin: bin, dir: work, home: filepath.Join(work, "home")}
	if _, err := top.run(ctx, nil, "init", "--quiet", "repo"); err != nil {
		return nil, err
	}
	g := git{bin: bin, dir: filepath.Join(work, "repo"), home: top.home}
	if _, err := g.run(ctx, nil, "remote", "add", "origin", r.cloneURL(j.Repo)); err != nil {
		return nil, err
	}

	read, err := d.App.Token(ctx, j.Repo, github.ReadContents)
	if err != nil {
		return nil, err
	}
	revokeRead := func() { _ = read.Revoke(context.WithoutCancel(ctx)) }
	defer revokeRead()
	defBranch, defSHA, branchSHA, err := g.remoteHeads(ctx, read.GitAuthEnv(), branch)
	if err != nil {
		return nil, err
	}
	res := &Result{Branch: branch}

	// Gate: an earlier attempt of this run pushed the branch and died
	// before recording it. The commit names its run, so the push is not
	// repeated and the model is not called again.
	if branchSHA != "" {
		if err := g.fetchDepth(ctx, read.GitAuthEnv(), "refs/heads/"+branch, "refs/orbit/branch", 2); err != nil {
			return nil, err
		}
		msg, err := g.run(ctx, nil, "log", "-1", "--format=%B", branchSHA)
		if err != nil {
			return nil, err
		}
		if strings.Contains(msg, runTrailer+j.RunID) {
			return r.redeliver(ctx, j, d, g, res, defBranch, branchSHA, msg)
		}
	}

	start := defSHA
	if j.Purpose == "fix" {
		if branchSHA == "" {
			return nil, fmt.Errorf("the PR's branch %s is gone; retry the ticket to build it again", branch)
		}
		start = branchSHA
	} else {
		if err := g.fetch(ctx, read.GitAuthEnv(), "refs/heads/"+defBranch, "refs/orbit/base"); err != nil {
			return nil, err
		}
		res.BaseSHA = defSHA
	}
	if _, err := g.run(ctx, nil, "checkout", "--quiet", "--detach", start); err != nil {
		return nil, err
	}
	revokeRead()
	d.emit("orbit.build", map[string]any{"step": "checkout", "repo": j.Repo, "start": start, "purpose": j.Purpose, "branch": branch})

	out, cost, err := r.sandboxed(ctx, j, d, g)
	if out == nil {
		return nil, err
	}
	res.CostUSD, res.Usage = cost.cost, cost.usage
	if err != nil {
		return res, err
	}
	res.Output = *out

	policy := DiffPolicy{Protected: j.Policy.Protected, MaxDiffLines: j.Policy.MaxDiffLines, Secrets: []string{j.Credential}}
	diff, err := policy.check(ctx, g)
	if err != nil {
		return res, err
	}
	res.Diff = diff
	res.TrustBoundary = TrustBoundary(j.Repo, diff.Paths())
	if d.Artifact != nil {
		if err := d.Artifact("out", "diff.patch", "text/x-diff", diff.Patch); err != nil {
			return res, err
		}
	}
	d.emit("orbit.build", map[string]any{"step": "diff", "files": diff.Files, "added": diff.Added, "deleted": diff.Deleted,
		"trust_boundary": res.TrustBoundary})
	title := prTitle(out.PRTitle, j.Title)
	sha, err := commit(ctx, g, j.Author, title, commitBody(out.PRBody), j.RunID)
	if err != nil {
		return res, err
	}
	res.HeadSHA = sha
	if err := r.deliver(ctx, j, d, g, res, defBranch, title, out.PRBody, true); err != nil {
		return res, err
	}
	return res, nil
}

// costs is what the CLI reported spending.
type costs struct {
	cost  *float64
	usage json.RawMessage
}

// sandboxed runs the CLI in a fresh sandbox over the clone's tree and
// writes the sandbox's tree back into the clone. The sandbox is destroyed
// whatever happens. out is nil when the model never produced a result.
func (r *Runner) sandboxed(ctx context.Context, j Job, d Deps, g git) (*Output, costs, error) {
	tree, err := packTree(g.dir)
	if err != nil {
		return nil, costs{}, err
	}
	sb := d.Sandbox
	cr, err := sb.Create(ctx, j.RunID, tree)
	if err != nil {
		return nil, costs{}, fmt.Errorf("create the sandbox: %w", err)
	}
	defer func() {
		if err := sb.Destroy(context.WithoutCancel(ctx), cr.ID); err != nil {
			d.emit("orbit.build", map[string]any{"step": "destroy", "error": err.Error()})
		}
	}()
	d.emit("orbit.build", map[string]any{"step": "sandbox", "id": cr.ID, "expires_at": cr.ExpiresAt})

	policy := Policy{Root: sandbox.WorkDir, Protected: j.Policy.Protected, Commands: j.Policy.Argv()}
	binary := r.Binary
	if binary == "" {
		binary = "claude"
	}
	ar := sandbox.AttachRequest{
		Argv: cliArgs(binary, j.Model, j.MaxTurns, systemPrompt(policy, j.Purpose)),
		Env:  map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": j.Credential},
	}
	proc := func(ctx context.Context, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
		return sb.Attach(ctx, cr.ID, ar, stdin, stdout, stderr)
	}
	timeout := j.Timeout
	if timeout <= 0 {
		timeout = spec.MaxBuildTimeout
	}
	runCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	emit := d.Emit
	if emit == nil {
		emit = func(string, []byte) {}
	}
	cv := &conversation{perms: Permissions{Policy: policy, Record: d.Record}, emit: emit}
	cli, err := cv.run(runCtx, proc, userPrompt(j))
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, costs{}, fmt.Errorf("the build timed out after %s", timeout)
	}
	if err != nil {
		return nil, costs{}, err
	}
	c := costs{cost: cli.TotalCostUSD, usage: cli.Usage}
	if n := len(cli.PermissionDenials); n > 0 && string(cli.PermissionDenials) != "null" && string(cli.PermissionDenials) != "[]" {
		emit("orbit.permission_denials", cli.PermissionDenials)
	}
	if cli.IsError {
		return &Output{}, c, fmt.Errorf("claude-code %s/%s: %s", cli.Subtype, cli.TerminalReason, lastLine(cli.Result))
	}
	var out Output
	dec := json.NewDecoder(bytes.NewReader(cli.StructuredOutput))
	dec.DisallowUnknownFields()
	if len(cli.StructuredOutput) == 0 || dec.Decode(&out) != nil || strings.TrimSpace(out.Summary) == "" {
		return &Output{}, c, fmt.Errorf("the build finished without a valid result: %s", lastLine(cli.Result))
	}
	if d.Artifact != nil {
		if err := d.Artifact("out", "output.json", "application/json", cli.StructuredOutput); err != nil {
			return &out, c, err
		}
	}
	rc, err := sb.Archive(ctx, cr.ID)
	if err != nil {
		return &out, c, fmt.Errorf("read the sandbox's tree back: %w", err)
	}
	defer rc.Close()
	if err := applyTree(g.dir, rc); err != nil {
		return &out, c, err
	}
	return &out, c, nil
}

// redeliver finishes a run whose earlier attempt pushed its commit: the PR
// is found or opened from the commit itself.
func (r *Runner) redeliver(ctx context.Context, j Job, d Deps, g git, res *Result, defBranch, head, msg string) (*Result, error) {
	res.Gate = "an earlier attempt of this run already pushed " + head[:12]
	d.emit("orbit.gate", map[string]any{"gate": "pushed", "head": head})
	parent, err := g.run(ctx, nil, "log", "-1", "--format=%P", head)
	if err != nil {
		return nil, err
	}
	res.HeadSHA, res.BaseSHA = head, strings.Fields(parent + " ")[0]
	if j.Purpose == "fix" {
		res.BaseSHA = ""
	}
	if names, err := g.run(ctx, nil, "diff", "--name-only", "-z", res.HeadSHA+"^", res.HeadSHA); err == nil {
		res.TrustBoundary = TrustBoundary(j.Repo, strings.Split(strings.TrimSuffix(names, "\x00"), "\x00"))
	}
	title, body := splitMessage(msg)
	res.Output = Output{Summary: "Delivered the commit an earlier attempt pushed", PRTitle: title, PRBody: body}
	return res, r.deliver(ctx, j, d, g, res, defBranch, title, body, false)
}

// deliver pushes the commit (when push) and opens or updates the PR, with
// a token for that and nothing else, revoked after.
func (r *Runner) deliver(ctx context.Context, j Job, d Deps, g git, res *Result, defBranch, title, body string, push bool) error {
	tok, err := d.App.Token(ctx, j.Repo, github.Deliver)
	if err != nil {
		return err
	}
	defer func() { _ = tok.Revoke(context.WithoutCancel(ctx)) }()
	if push {
		rs, err := refspec(j.TicketID, res.HeadSHA)
		if err != nil {
			return err
		}
		if _, err := g.run(ctx, tok.GitAuthEnv(), "push", "--quiet", "origin", rs); err != nil {
			return err
		}
		d.emit("orbit.build", map[string]any{"step": "push", "branch": res.Branch, "head": res.HeadSHA})
	}
	c := tok.Client()
	full := PRBody(body, j, res.TrustBoundary)
	pr, err := c.OpenPR(ctx, j.Repo, res.Branch)
	if err != nil {
		return fmt.Errorf("find the PR: %w", err)
	}
	if pr == nil {
		pr, err = c.CreatePR(ctx, j.Repo, github.NewPR{Title: title, Head: res.Branch, Base: defBranch, Body: full})
		if err != nil {
			return fmt.Errorf("open the PR: %w", err)
		}
	} else if err := c.UpdatePR(ctx, j.Repo, pr.Number, title, full); err != nil {
		return fmt.Errorf("update PR #%d: %w", pr.Number, err)
	}
	res.PRNumber, res.PRURL = pr.Number, pr.HTMLURL
	d.emit("orbit.build", map[string]any{"step": "pr", "number": pr.Number, "url": pr.HTMLURL})
	return nil
}

// prTitle is the model's title on one line within prTitleMax, or the
// ticket's title.
func prTitle(model, ticket string) string {
	t := strings.TrimSpace(strings.SplitN(github.ScrubAttribution(model), "\n", 2)[0])
	if t == "" {
		t = strings.TrimSpace(strings.SplitN(ticket, "\n", 2)[0])
	}
	if r := []rune(t); len(r) > prTitleMax {
		t = string(r[:prTitleMax])
	}
	return t
}

// commitBody is the PR body as the commit carries it.
func commitBody(body string) string { return github.ScrubAttribution(body) }

// splitMessage reads a build commit back: title, body, trailer dropped.
func splitMessage(msg string) (title, body string) {
	title, rest, _ := strings.Cut(strings.TrimSpace(msg), "\n")
	var keep []string
	for _, l := range strings.Split(rest, "\n") {
		if !strings.HasPrefix(l, runTrailer) {
			keep = append(keep, l)
		}
	}
	return strings.TrimSpace(title), strings.TrimSpace(strings.Join(keep, "\n"))
}

// PRBody is the PR's body: the model's description, scrubbed of
// attribution, then what orbit knows: the ticket, the plan version and, for
// orbit itself, whether the change touches its trust boundary.
func PRBody(model string, j Job, trust []string) string {
	var b strings.Builder
	b.WriteString(strings.TrimSpace(github.ScrubAttribution(model)))
	fmt.Fprintf(&b, "\n\n---\n\nBuilt by orbit from ticket `%s` (%q), plan version %d.", j.TicketID, j.Title, j.PlanVersion)
	if len(trust) > 0 {
		fmt.Fprintf(&b, "\n\n**Trust boundary:** this change touches what orbit itself may do: %s. Review those files before approving the merge.",
			"`"+strings.Join(trust, "`, `")+"`")
	}
	return github.ScrubAttribution(b.String())
}
