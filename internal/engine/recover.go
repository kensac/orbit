package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// errLostClaim is the cause an attempt is cancelled with when it no longer
// holds its run: the reaper requeued it (and another worker may have
// claimed it), or orbit could not heartbeat for long enough that it would
// have. The attempt then writes nothing; the run belongs to its next claim.
var errLostClaim = errors.New("this attempt lost its claim on the run")

// attemptState is what an attempt learned about its backend session, for
// costing it when it is handed back.
type attemptState struct {
	home, workdir string
	// keeps is true for an agent whose session outlives the run.
	keeps       bool
	sessionID   string
	baseCostUSD float64
	// instructor is the person the run answers (runInstructor).
	instructor string
	// cancel is closed when a person asks the run to stop.
	cancel     chan struct{}
	cancelOnce sync.Once
	// task is the task whose conversation the run is in, nil for none.
	task *store.Task
	// steer is set while a chat turn runs: where the attempt's poll hands
	// the conversation's steer messages (steer.go).
	steer atomic.Pointer[steerFeed]
}

// stop closes cancel once.
func (st *attemptState) stop() { st.cancelOnce.Do(func() { close(st.cancel) }) }

// spent reads what the attempt's session had cost when it stopped, from
// its transcript: (this attempt's share, the session's running total), nil
// when the backend saved none.
func (st *attemptState) spent() (cost, total *float64) {
	if st.sessionID == "" {
		return nil, nil
	}
	t, ok := runner.TranscriptCost(st.home, st.workdir, st.sessionID)
	if !ok {
		return nil, nil
	}
	share := t
	if t >= st.baseCostUSD {
		share = t - st.baseCostUSD
	}
	return &share, &t
}

// recovery is an interrupted attempt this attempt continues: its session
// (forked, never written into), the inputs it read, and the messages it
// had already been shown.
type recovery struct {
	from      int
	sessionID string
	// base is the session's running cost total its transcript saved,
	// when it saved one (baseKnown).
	base      float64
	baseKnown bool
	inputs    []store.StoredInput
	seen      map[string]bool
}

// planRecovery decides whether this attempt continues the previous
// attempt's Claude Code session rather than starting the run over, so a
// run a deploy or a crash interrupted does not pay twice for the work it
// had done. It continues when the previous attempt ran on Claude Code
// under the same spec, its stream named a session, and that session's
// transcript is on the data volume (shared between instances). Otherwise
// the run starts over and skipped says why. Both are recorded on the
// attempt.
func (e *Engine) planRecovery(ctx context.Context, a *spec.Agent, c *store.ClaimedRun, kind, workdir, home string) (rec *recovery, skipped string, err error) {
	if c.Attempt < 2 {
		return nil, "", nil
	}
	if kind != "claude-code" {
		// Recorded all the same: a stream waits for this decision to know
		// whether the new attempt continues the last one's narration.
		skipped = "only a Claude Code session can be continued"
		return nil, skipped, e.Store.SetRecovery(ctx, c.ID, c.Attempt, 0, skipped)
	}
	prev, err := e.Store.PriorAttempt(ctx, c.ID, c.Attempt)
	if err != nil {
		return nil, "", err
	}
	switch {
	case prev == nil:
		skipped = "the interrupted attempt was not recorded"
	case prev.SessionID == nil:
		skipped = "the interrupted attempt stopped before its backend named a session"
	case prev.RunnerKind == nil || *prev.RunnerKind != kind:
		skipped = "the interrupted attempt ran on another backend"
	case prev.SpecSHA == nil || *prev.SpecSHA != a.SHA:
		skipped = "the agent's spec changed since the interrupted attempt"
	default:
		if _, ok := runner.SessionFile(home, workdir, *prev.SessionID); !ok {
			skipped = "the interrupted attempt's transcript path is too long to locate"
		} else if present, serr := e.ensureSession(ctx, home, workdir, *prev.SessionID); serr != nil {
			return nil, "", serr
		} else if !present {
			skipped = "the interrupted attempt's transcript is gone (is " + e.DataDir + " a volume shared by every instance?" + e.storeHint() + ")"
		}
	}
	if skipped != "" {
		return nil, skipped, e.Store.SetRecovery(ctx, c.ID, c.Attempt, 0, skipped)
	}

	rec = &recovery{from: prev.Attempt, sessionID: *prev.SessionID, seen: map[string]bool{}}
	// An attempt whose process died was costed by nobody; its transcript
	// says what it spent when the CLI saved one (a SIGTERM does, a SIGKILL
	// does not).
	if total, ok := runner.TranscriptCost(home, workdir, rec.sessionID); ok {
		rec.base, rec.baseKnown = total, true
		if prev.CostUSD == nil {
			base := 0.0
			if prev.SessionCostUSD != nil {
				base = *prev.SessionCostUSD
			}
			share := total
			if total >= base {
				share = total - base
			}
			if err := e.Store.SetAttemptCost(ctx, c.ID, prev.Attempt, &share, &total); err != nil {
				return nil, "", err
			}
		}
	}
	arts, err := e.Store.AttemptArtifacts(ctx, c.ID, prev.Attempt, "in")
	if err != nil {
		return nil, "", err
	}
	for _, art := range arts {
		switch {
		case strings.HasPrefix(art.Name, "inputs/"):
			rec.inputs = append(rec.inputs, art)
		case art.Name == "messages.json":
			var ms []store.Message
			if err := json.Unmarshal(art.Content, &ms); err != nil {
				return nil, "", fmt.Errorf("the interrupted attempt's messages.json: %w", err)
			}
			for _, m := range ms {
				rec.seen[m.ID] = true
			}
		}
	}
	return rec, "", e.Store.SetRecovery(ctx, c.ID, c.Attempt, rec.from, "")
}

// restoreInputs writes the interrupted attempt's inputs back where it read
// them, from their artifacts. The session refers to those paths, and it
// continues on exactly what it saw rather than on a refetch that may
// differ.
func restoreInputs(rec *recovery, workdir string) ([]runner.Input, error) {
	var inputs []runner.Input
	for _, art := range rec.inputs {
		path := filepath.Join(workdir, filepath.FromSlash(art.Name))
		if !strings.HasPrefix(path, filepath.Clean(workdir)+string(filepath.Separator)) {
			return nil, fmt.Errorf("artifact %q is outside the workdir", art.Name)
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, art.Content, 0o600); err != nil {
			return nil, err
		}
		name := strings.TrimSuffix(filepath.Base(art.Name), filepath.Ext(art.Name))
		inputs = append(inputs, runner.Input{Name: name, Path: art.Name, MediaType: art.MediaType, Content: art.Content})
	}
	return inputs, nil
}

// recoveryPrompt is the message that continues an interrupted attempt's
// session: the session already holds the run's prompt and the work done
// so far, so this says only that it was interrupted, and adds what reached
// the agent since.
func recoveryPrompt(rec *recovery, rc *store.RunContext, loc *time.Location) string {
	var b strings.Builder
	b.WriteString("orbit was restarted while you were working on this run, so your previous turn was interrupted. " +
		"Continue from exactly where you stopped: do not redo work already done above, your inputs are where they were, " +
		"and finish by returning the structured output as before.\n")
	var fresh []store.Message
	for _, m := range rc.Messages {
		if !rec.seen[m.ID] {
			fresh = append(fresh, m)
		}
	}
	if len(fresh) > 0 {
		b.WriteString("\n# Messages from your person since the interruption\n\n")
		for _, m := range fresh {
			fmt.Fprintf(&b, "- %s: %s\n", m.CreatedAt.In(loc).Format("2006-01-02 15:04"), m.Body)
		}
	}
	return b.String()
}
