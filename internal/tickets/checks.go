package tickets

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/store"
)

// Failure report bounds: the end of each failing job's log, at most
// logTailLines lines within logTailBytes, for at most maxFailing checks. A
// fix needs the failure, not the whole log, and the report rides in its
// prompt; 150 lines holds a Go test failure with its stack and the make
// target's summary.
const (
	logTailBytes = 16 << 10
	logTailLines = 150
	maxFailing   = 5
)

// MaxReruns is how many times the loop reruns a head's failed Actions jobs
// on its own before it spends a fix: once, so a flaky test or a runner that
// died costs a rerun, not a build, and a real failure costs one rerun more.
const MaxReruns = 1

// What a failing check did on the commit the build started from.
const (
	baseFailed  = "failed"
	basePassed  = "passed"
	basePending = "pending"
	baseAbsent  = "absent"
)

// failure is one failing check as a fix is told it.
type failure struct {
	github.Failing
	// OnBase is what the same check did on the commit the build started
	// from: failed there too means this change did not cause it.
	OnBase  string `json:"on_base"`
	LogTail string `json:"log_tail,omitempty"`
	LogNote string `json:"log_note,omitempty"`
}

// rerunJob is one job a rerun asked GitHub to run again.
type rerunJob struct {
	Name  string `json:"name"`
	JobID int64  `json:"job_id"`
}

func rerunnable(failing []github.Failing) []rerunJob {
	var out []rerunJob
	for _, f := range failing {
		if f.JobID > 0 {
			out = append(out, rerunJob{Name: f.Name, JobID: f.JobID})
		}
	}
	return out
}

// rerun asks GitHub to run each job again, with a token that can do that
// and nothing else the loop holds. It stops at the first refusal.
func (k *tick) rerun(ctx context.Context, repo string, jobs []rerunJob) error {
	c, err := k.client(ctx, repo, github.RerunPermissions, "rerun")
	if err != nil {
		return fmt.Errorf("a token to rerun jobs (the App needs Actions read and write): %w", err)
	}
	for _, j := range jobs {
		if err := c.RerunJob(ctx, repo, j.JobID); err != nil {
			return fmt.Errorf("rerun %s: %w", j.Name, err)
		}
	}
	return nil
}

// checksFailed reruns the head's failed Actions jobs once, then starts a
// fix with the failure report, or fails the ticket: once MaxFixAttempts
// fixes were spent on this PR, or when every failing check fails on the
// base commit too (the change did not cause it, so a fix cannot be told to
// make it pass).
func (k *tick) checksFailed(ctx context.Context, t *store.Ticket, c *github.Client, pr *github.PullRequest, failing []github.Failing) error {
	head := pr.Head.SHA
	if jobs := rerunnable(failing); len(jobs) > 0 {
		n, err := k.l.Store.OrbitReruns(ctx, t.ID, head)
		if err != nil {
			return err
		}
		if n < MaxReruns {
			jb, _ := json.Marshal(jobs)
			rerr := k.rerun(ctx, *t.Repo, jobs)
			msg := ""
			if rerr != nil {
				msg = rerr.Error()
			}
			if err := k.l.Store.RecordRerun(ctx, t.ID, head, jb, msg); err != nil {
				return err
			}
			if rerr == nil {
				k.l.Log.Info("ticket checks rerun", "ticket", t.ID, "head", short(head), "jobs", len(jobs))
				return nil
			}
			k.l.Log.Warn("ticket checks rerun", "ticket", t.ID, "err", rerr)
		}
	}
	names := make([]string, 0, len(failing))
	for _, f := range failing {
		s := f.Name + " (" + f.Conclusion + ")"
		if f.URL != "" {
			s += " " + f.URL
		}
		names = append(names, s)
	}
	fixes, err := k.l.Store.FixesSinceBuild(ctx, t.ID)
	if err != nil {
		return err
	}
	if fixes >= MaxFixAttempts {
		return k.fail(ctx, t, fmt.Sprintf("checks failed after %d fix attempts: %s", fixes, strings.Join(names, "; ")))
	}
	report, err := k.failureReport(ctx, t, c, failing)
	if err != nil {
		return err
	}
	inherited := len(report) > 0
	for _, f := range report {
		inherited = inherited && f.OnBase == baseFailed
	}
	if inherited {
		return k.fail(ctx, t, fmt.Sprintf("every failing check also fails on %s, where this change started; fix that first, then retry: %s",
			short(*t.BaseSHA), strings.Join(names, "; ")))
	}
	b, err := json.Marshal(report)
	if err != nil {
		return err
	}
	return k.startBuild(ctx, t, store.BuildPurposeFix, b)
}

// failureReport is what a fix is told, collected by code: each failing
// check's name, conclusion and URL, what the same check did on the base
// commit, and the end of its Actions job log.
func (k *tick) failureReport(ctx context.Context, t *store.Ticket, c *github.Client, failing []github.Failing) ([]failure, error) {
	onBase := map[string]string{}
	if t.BaseSHA != nil && *t.BaseSHA != "" {
		runs, err := c.CheckRuns(ctx, *t.Repo, *t.BaseSHA)
		if err != nil && !github.IsStatus(err, 404) && !github.IsStatus(err, 422) {
			return nil, err
		}
		for _, r := range runs {
			switch {
			case r.Status != "completed":
				onBase[r.Name] = basePending
			case r.Conclusion == "success" || r.Conclusion == "skipped" || r.Conclusion == "neutral":
				onBase[r.Name] = basePassed
			default:
				onBase[r.Name] = baseFailed
			}
		}
	}
	var logs *github.Client
	out := []failure{}
	for _, f := range failing[:min(len(failing), maxFailing)] {
		e := failure{Failing: f, OnBase: baseAbsent}
		if s, ok := onBase[f.Name]; ok {
			e.OnBase = s
		}
		if f.JobID > 0 {
			if logs == nil {
				var err error
				if logs, err = k.client(ctx, *t.Repo, github.FailureReport, "failures"); err != nil {
					return nil, err
				}
			}
			tail, err := logs.JobLogTail(ctx, *t.Repo, f.JobID, logTailBytes)
			if err != nil {
				e.LogNote = "orbit could not read the job log: " + err.Error()
			} else {
				e.LogTail = TidyLog(tail, logTailLines)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

// personRerun carries out a person's rerun request: the PR's failed Actions
// jobs at the head the request named are run again, and a failed ticket
// goes back to review so the loop watches them. A PR that closed or moved
// on answers the request with why instead.
func (k *tick) personRerun(ctx context.Context, t *store.Ticket, r *store.Rerun) error {
	finish := func(jobs []rerunJob, why string) error {
		var jb json.RawMessage
		if len(jobs) > 0 {
			jb, _ = json.Marshal(jobs)
		}
		err := k.l.Store.FinishRerun(ctx, r.ID, jb, why, true)
		if errors.Is(err, store.ErrConflict) {
			return nil
		}
		return err
	}
	c, pr, err := k.pr(ctx, t)
	if err != nil {
		return err
	}
	switch {
	case pr.State != "open":
		return finish(nil, fmt.Sprintf("PR #%d is not open", pr.Number))
	case pr.Head.SHA != r.HeadSHA:
		return finish(nil, fmt.Sprintf("PR #%d's head is %s, not the %s the rerun was asked for", pr.Number, short(pr.Head.SHA), short(r.HeadSHA)))
	}
	required, err := c.RequiredChecks(ctx, *t.Repo, pr.Base.Ref)
	if err != nil {
		return err
	}
	runs, err := c.CheckRuns(ctx, *t.Repo, pr.Head.SHA)
	if err != nil {
		return err
	}
	statuses, err := c.Statuses(ctx, *t.Repo, pr.Head.SHA)
	if err != nil {
		return err
	}
	_, failing := github.Classify(required, runs, statuses)
	jobs := rerunnable(failing)
	if len(failing) > 0 && len(jobs) == 0 {
		names := make([]string, 0, len(failing))
		for _, f := range failing {
			names = append(names, f.Name)
		}
		return finish(nil, "no failed Actions job to rerun; failing: "+strings.Join(names, ", "))
	}
	if len(jobs) > 0 {
		if err := k.rerun(ctx, *t.Repo, jobs); err != nil {
			return finish(jobs, err.Error())
		}
	}
	k.l.Log.Info("ticket checks rerun", "ticket", t.ID, "by", r.RequestedBy, "jobs", len(jobs))
	return finish(jobs, "")
}

var (
	// ansiRE is a terminal escape sequence (colours, cursor moves).
	ansiRE = regexp.MustCompile(`\x1b\[[0-9;?]*[A-Za-z]`)
	// stampRE is the timestamp Actions puts before every log line.
	stampRE = regexp.MustCompile(`^\d{4}-\d\d-\d\dT\d\d:\d\d:\d\d(\.\d+)?Z `)
)

// TidyLog is the end of a job log as a fix reads it: terminal escapes and
// Actions' per-line timestamps removed, CRLF made LF, a first line cut
// mid-way by the byte tail dropped, and at most lines lines kept. Nothing
// is reworded, so what the model reads is what the job printed.
func TidyLog(tail string, lines int) string {
	tail = strings.ReplaceAll(tail, "\r\n", "\n")
	all := strings.Split(tail, "\n")
	if len(all) > 1 && !strings.HasPrefix(tail, "\n") {
		// The tail starts wherever the byte cut fell; its first line is
		// partial unless the log was shorter than the tail.
		if len(tail) >= logTailBytes {
			all = all[1:]
		}
	}
	trailing := len(all) > 0 && all[len(all)-1] == ""
	if trailing {
		all = all[:len(all)-1]
	}
	if len(all) > lines {
		all = all[len(all)-lines:]
	}
	for i, l := range all {
		all[i] = stampRE.ReplaceAllString(ansiRE.ReplaceAllString(l, ""), "")
	}
	out := strings.Join(all, "\n")
	if trailing {
		out += "\n"
	}
	return out
}
