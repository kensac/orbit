// Package engine runs one agent wake end to end: read its context, fetch its
// inputs, call its backend, validate the result, store everything, and tell
// the person. It is the only package that knows what a run means; the
// runner, sources, actions and notify packages each know one piece.
package engine

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/kensac/orbit/internal/actions"
	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/mail"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/notify"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/sources"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// Engine holds what every run shares. Config is swapped atomically on reload
// through Current; a run keeps the snapshot it started with.
type Engine struct {
	Store   *store.Store
	DataDir string
	// ConfigDir is the configuration directory (orbit.yaml, agents/),
	// loaded with the database's agent definitions (config.go).
	ConfigDir string
	Env       func(string) (string, bool)
	Client    *http.Client
	// Secret signs approval links (see ApprovalSig).
	Secret []byte
	// NewRunner is runner.New, replaceable in tests.
	NewRunner func(name string, rs spec.RunnerSpec, env func(string) (string, bool)) (runner.Runner, error)
	Log       *slog.Logger
	// Live holds the live Claude Code processes of agents with
	// session.live. Nil runs every turn in a process of its own.
	Live *runner.Live
	// Harness is the orbit binary a claude-code run starts for orbit's
	// tools and hooks (os.Executable in serve). Empty runs without them.
	Harness string
	// HeartbeatEvery overrides heartbeatEvery when set; tests shorten it.
	HeartbeatEvery time.Duration
	// Builder runs build runs (internal/build); nil fails them.
	Builder *build.Runner
	// Drain is this instance's drain; a draining instance starts no build
	// sandbox (build.go).
	Drain *Drain
	// Outbox makes this a worker's engine (orbit worker): it holds no
	// channel credential and no Secret, so SetConfig builds no channels
	// and every notification is recorded for control to send
	// (DeliverOutbox), with the links only control can sign.
	Outbox bool
	// Worker names this instance in the outbox and session rows it writes.
	Worker string
	// SessionStore keeps Claude Code transcripts in the database as well
	// as on this host's volume, so a session continues on whichever host
	// claims its next run (sessionstore.go).
	SessionStore bool

	mu  sync.RWMutex
	cfg *Snapshot
}

// Snapshot is one loaded configuration with its built channels.
type Snapshot struct {
	Config   *spec.Config
	Channels map[string]notify.Channel
}

// heartbeatEvery is well inside the reaper's stale bound (StaleAfter), so a
// live run is never reaped for one slow heartbeat.
const heartbeatEvery = 30 * time.Second

// StaleAfter is how long a running run may go without a heartbeat before the
// reaper fails it: ten missed beats.
const StaleAfter = 10 * heartbeatEvery

// SetConfig installs a configuration. Channels are built here so a channel
// with a missing credential fails the reload, not the first notification.
func (e *Engine) SetConfig(cfg *spec.Config) error {
	chans := map[string]notify.Channel{}
	for name, c := range cfg.Deployment.Channels {
		if e.Outbox {
			break
		}
		ch, err := notify.New(name, c, e.Env, e.Client)
		if err != nil {
			return err
		}
		chans[name] = ch
	}
	e.mu.Lock()
	e.cfg = &Snapshot{Config: cfg, Channels: chans}
	e.mu.Unlock()
	return nil
}

// Current returns the configuration in force.
func (e *Engine) Current() *Snapshot {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.cfg
}

// Execute runs one claimed run to a final state. It returns an error only
// when it could not record that state; a failed run is a recorded outcome.
func (e *Engine) Execute(ctx context.Context, c *store.ClaimedRun) error {
	snap := e.Current()
	start := time.Now()
	log := e.Log.With("run", c.ID, "agent", c.Agent)
	a, ok := snap.Config.Agents[c.Agent]
	if !ok {
		return e.fail(ctx, snap, nil, c, start, fmt.Errorf("agent %q is no longer configured", c.Agent), nil)
	}

	// actx ends when ctx does (a drain past its cap) or when the attempt
	// loses its claim; the heartbeat finds out which.
	actx, lose := context.WithCancelCause(ctx)
	defer lose(nil)
	go e.heartbeat(actx, c, lose, log)
	if c.Trigger == store.TriggerBuild {
		return e.executeBuild(ctx, actx, snap, a, c, start, log)
	}
	if c.Trigger == store.TriggerLearn {
		return e.executeLearn(ctx, actx, snap, a, c, start, log)
	}

	st := &attemptState{}
	out, res, err := e.attempt(actx, snap, a, c, log, st)
	if errors.Is(context.Cause(actx), errLostClaim) {
		// The run is another attempt's now; anything written here would be
		// refused, and a notification would be a lie.
		log.Warn("attempt stopped: it lost its claim on the run", "attempt", c.Attempt)
		return nil
	}
	if ctx.Err() != nil {
		// orbit is stopping, not the run failing: give it back to the
		// queue with what it spent, and the next attempt, here or on
		// another instance, continues its session (planRecovery).
		cost, total := st.spent()
		log.Info("handed back", "attempt", c.Attempt, "session", st.sessionID)
		// The next attempt may be claimed on another host.
		e.saveSession(ctx, c, st.home, st.workdir, st.sessionID)
		return e.Store.HandBack(context.WithoutCancel(ctx), c.ID, c.Attempt, cost, total)
	}
	var sk *gateSkip
	if errors.As(err, &sk) {
		return e.skip(context.WithoutCancel(ctx), snap, a, c, start, sk)
	}
	if err != nil {
		return e.fail(ctx, snap, a, c, start, err, res)
	}
	if st.keeps && res != nil {
		// Stored before the run is recorded as succeeded, so the
		// conversation's next run, claimed on any host, finds it.
		e.saveSession(ctx, c, st.home, st.workdir, res.SessionID)
	}
	return e.succeed(ctx, snap, a, c, start, out, res)
}

// heartbeat keeps the attempt's claim alive and cancels the attempt with
// errLostClaim when the claim is gone, or when no beat has landed for long
// enough that the reaper (on any instance) may have requeued it: one beat
// short of StaleAfter, so this attempt stops before another can start.
func (e *Engine) heartbeat(ctx context.Context, c *store.ClaimedRun, lose context.CancelCauseFunc, log *slog.Logger) {
	every := heartbeatEvery
	if e.HeartbeatEvery > 0 {
		every = e.HeartbeatEvery
	}
	t := time.NewTicker(every)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			err := e.Store.Heartbeat(context.WithoutCancel(ctx), c.ID, c.Attempt)
			switch {
			case err == nil:
				last = time.Now()
			case errors.Is(err, store.ErrConflict):
				lose(errLostClaim)
				return
			default:
				log.Warn("heartbeat", "err", err)
				if time.Since(last) > StaleAfter-heartbeatEvery {
					lose(errLostClaim)
					return
				}
			}
		}
	}
}

// attempt does the work; any error fails the run. st collects what a
// hand-back needs to cost the attempt.
func (e *Engine) attempt(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, log *slog.Logger, st *attemptState) (*Output, *runner.Result, error) {
	rs := snap.Config.Deployment.Runners[a.Runner]
	r, err := e.NewRunner(a.Runner, rs, e.Env)
	if err != nil {
		return nil, nil, err
	}
	if err := e.Store.SetRunner(ctx, c.ID, c.Attempt, r.Kind(), r.Model(), a.SHA); err != nil {
		return nil, nil, err
	}

	// Every byte the run read is stored as an artifact, so a workdir is
	// scratch. A Claude Code run without a session gets one per run, not
	// per attempt, with its HOME inside: the CLI files a session under
	// its cwd, so an attempt that continues an interrupted one's session
	// must run where that one did. Other backends get one per attempt. An
	// agent with a session keeps one workdir and HOME (its sessions live
	// under HOME, keyed by the workdir's path, and a live process has one
	// cwd for its life); its inputs go in a directory named for the
	// attempt, so a continued session never mistakes this run's files for
	// the ones it read before. Scratch is removed when the attempt ends
	// the run, and kept when it is handed back, for the next attempt.
	attemptName := fmt.Sprintf("%s-%d", c.ID, c.Attempt)
	claudeCode := r.Kind() == "claude-code"
	workdir := filepath.Join(e.DataDir, "runs", attemptName)
	if claudeCode {
		workdir = filepath.Join(e.DataDir, "runs", c.ID)
	}
	home := filepath.Join(workdir, ".home")
	inputsDir := "inputs"
	keepScratch := func() bool { return ctx.Err() != nil }
	withSession := a.Session != nil && a.Session.Resume && claudeCode
	chat := a.Chat && claudeCode
	if withSession || chat {
		base := filepath.Join(e.DataDir, "agents", a.Name)
		workdir, home = filepath.Join(base, "work"), filepath.Join(base, "home")
		if chat {
			// A chat's workspace is its conversation's, kept between
			// messages and stored after every run.
			workdir, home = e.chatDirs(c.ConversationID)
			if err := e.restoreWorkspace(ctx, c.ConversationID, workdir); err != nil {
				return nil, nil, err
			}
			defer e.snapshotWorkspace(ctx, c, workdir)
		}
		inputsDir = filepath.Join("inputs", attemptName)
		// Earlier runs' inputs are in their artifacts; a crashed run's may
		// still be here.
		if err := os.RemoveAll(filepath.Join(workdir, "inputs")); err != nil {
			return nil, nil, err
		}
		defer func() {
			if !keepScratch() {
				_ = os.RemoveAll(filepath.Join(workdir, "inputs"))
			}
		}()
		// The tool ledger and the hook's counters are per run.
		if err := os.RemoveAll(harness.StateDir(home)); err != nil {
			return nil, nil, err
		}
	} else {
		defer func() {
			if !keepScratch() {
				_ = os.RemoveAll(workdir)
			}
		}()
	}
	st.home, st.workdir, st.keeps = home, workdir, withSession
	if err := os.MkdirAll(filepath.Join(workdir, inputsDir), 0o700); err != nil {
		return nil, nil, err
	}

	loc, _ := time.LoadLocation(a.Timezone)
	now := time.Now().In(loc)

	rec, skipped, err := e.planRecovery(ctx, a, c, r.Kind(), workdir, home)
	if err != nil {
		return nil, nil, err
	}

	rc, err := e.Store.StartRunContext(ctx, c.ID, a.Name)
	if err != nil {
		return nil, nil, err
	}
	if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", "memory.md", "text/markdown", []byte(rc.Memory)); err != nil {
		return nil, nil, err
	}
	attached, err := e.placeUploads(ctx, workdir, rc)
	if err != nil {
		return nil, nil, err
	}
	if len(rc.Messages) > 0 {
		b, _ := json.Marshal(rc.Messages)
		if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", "messages.json", "application/json", b); err != nil {
			return nil, nil, err
		}
	}

	seq := 0
	figures := a.Verifies("figures")
	var transcript [][]byte // the backend's message lines, for the figure check
	emit := func(kind string, payload any) {
		b, ok := payload.([]byte)
		if !ok {
			b, _ = json.Marshal(payload)
		}
		if figures && (kind == "assistant" || kind == "user") {
			transcript = append(transcript, b)
		}
		seq++
		if err := e.Store.AppendEvent(context.WithoutCancel(ctx), c.ID, c.Attempt, seq, kind, b); err != nil {
			log.Warn("append event", "err", err)
		}
	}

	if c.Attempt > 1 && claudeCode {
		emit("orbit.recovery", map[string]any{"recovers_attempt": recFrom(rec), "skipped_because": skipped})
	}
	fetcher := &sources.Fetcher{Client: e.Client, Env: e.Env}
	vars := sources.Vars(now)
	var inputs []runner.Input
	if rec != nil {
		// The session read these; it continues on exactly them.
		if inputs, err = restoreInputs(rec, workdir); err != nil {
			return nil, nil, err
		}
		for _, in := range inputs {
			if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", in.Path, in.MediaType, in.Content); err != nil {
				return nil, nil, err
			}
		}
	}
	raw := map[string][]byte{} // every source fetched, for its transforms
	for _, s := range a.Sources {
		if rec != nil {
			break
		}
		f, err := fetcher.Fetch(ctx, s, vars)
		if err != nil {
			emit("orbit.source_failed", map[string]any{"source": s.Name, "error": err.Error()})
			return nil, nil, fmt.Errorf("source %s: %w", s.Name, err)
		}
		ext := ".txt"
		switch f.MediaType {
		case "application/json":
			ext = ".json"
		case "text/markdown":
			ext = ".md"
		}
		raw[s.Name] = f.Content
		if a.Withheld(s.Name) {
			// Only its transforms read it: kept as the record of what was
			// fetched, never written where the model can read it.
			if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", "sources/"+s.Name+ext, f.MediaType, f.Content); err != nil {
				return nil, nil, err
			}
			emit("orbit.source_fetched", map[string]any{"source": s.Name, "bytes": len(f.Content), "meta": f.Meta, "withheld": true})
			continue
		}
		rel := filepath.Join(inputsDir, s.Name+ext)
		if err := os.WriteFile(filepath.Join(workdir, rel), f.Content, 0o600); err != nil {
			return nil, nil, err
		}
		if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", rel, f.MediaType, f.Content); err != nil {
			return nil, nil, err
		}
		emit("orbit.source_fetched", map[string]any{"source": s.Name, "bytes": len(f.Content), "meta": f.Meta})
		inputs = append(inputs, runner.Input{Name: s.Name, Path: rel, MediaType: f.MediaType, Content: f.Content})
	}
	if rec == nil {
		if inputs, err = e.transform(ctx, a, c, workdir, inputsDir, raw, inputs, emit); err != nil {
			return nil, nil, err
		}
	}
	if gated(a, c, rc, rec != nil) {
		docs := maps.Clone(raw)
		for _, in := range inputs {
			docs[in.Name] = in.Content
		}
		sk, err := e.gate(ctx, a, inputs, docs)
		if err != nil {
			return nil, nil, err
		}
		if sk != nil {
			emit("orbit.gate", map[string]any{"gate": sk.gate, "skipped": true, "reason": sk.summary})
			return nil, nil, sk
		}
	}

	var sess *runner.Session
	continuing := false
	switch {
	case withSession:
		sess, continuing, err = e.planSession(ctx, a, c, workdir, home, rec, emit)
		if err != nil {
			return nil, nil, err
		}
	case claudeCode:
		// No session between runs, but one within the run, kept in the
		// run's own HOME (removed with the workdir), so an interrupted
		// attempt can be continued rather than paid for twice.
		sess = &runner.Session{Home: home, KeepDays: 1, Started: func(string, string) error { return nil }}
		if rec != nil {
			sess.Resume, sess.BaseCostUSD = rec.sessionID, rec.base
		}
		// rec.base is 0 when the transcript saved no total, which is this
		// run's base anyway: it started from nothing.
	}
	if sess != nil {
		st.baseCostUSD = sess.BaseCostUSD
		sess.Attached = func(id string) {
			st.sessionID = id
			if err := e.Store.AttachSession(context.WithoutCancel(ctx), c.ID, c.Attempt, id); err != nil {
				log.Warn("attach session", "err", err)
			}
		}
	}
	var convo []store.ConversationItem
	if !continuing && rec == nil {
		// A continued session holds the thread already; a fresh one is
		// told it as text.
		items, err := e.Store.Conversation(ctx, a.Name, c.ID, conversationRows)
		if err != nil {
			return nil, nil, err
		}
		var text string
		text, convo = conversationText(items, now.Location())
		if text != "" {
			if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", "conversation.md", "text/markdown", []byte(text)); err != nil {
				return nil, nil, err
			}
		}
	}

	types := actions.Catalog(a, snap.Config.Deployment)
	schema := OutputSchema(types, snap.Config.Deployment.Defaults.MemoryMaxBytes, true)
	envelope := OutputSchema(types, snap.Config.Deployment.Defaults.MemoryMaxBytes, false)
	schema, envelope = withEntries(schema, true), withEntries(envelope, false)
	tk, err := e.Store.TicketForConversation(ctx, c.ConversationID)
	if err != nil {
		return nil, nil, err
	}
	planning := takesPlan(tk)
	if planning {
		schema, envelope = withPlan(schema), withPlan(envelope)
	}
	if tk != nil {
		if inputs, err = e.ticketInput(ctx, c, tk, workdir, inputsDir, inputs); err != nil {
			return nil, nil, err
		}
	}
	topics, err := e.agentTopics(ctx, a)
	if err != nil {
		return nil, nil, err
	}
	// The entries a run reads are also the ones its entry writes may name (entries.go), so a recovered attempt reads them too.
	knownEntries, err := e.Store.EntriesFor(ctx, a.Name, topics)
	if err != nil {
		return nil, nil, err
	}
	var known string
	if rec == nil {
		if known, inputs, err = e.knownInput(ctx, c, knownEntries, workdir, inputsDir, inputs); err != nil {
			return nil, nil, err
		}
	}
	var hr *runner.Harness
	if e.Harness != "" && r.Kind() == "claude-code" {
		hr = &runner.Harness{Bin: e.Harness, Figures: figures && !chat}
	}
	if chat {
		return e.chatTurn(ctx, snap, a, c, r, chatTurn{workdir: workdir, home: home, inputsDir: inputsDir, now: now,
			run: promptRun{Trigger: c.Trigger, Wake: c.Prompt, Chain: c.Chain, Continuing: continuing, Known: known, Attached: attached},
			rc: rc, convo: convo, inputs: inputs, types: types, sess: sess, harness: hr, emit: emit, cancel: st.cancel})
	}
	system := systemPrompt(a, types, inputs, r.Kind() != "anthropic", hr != nil)
	if tk != nil {
		system += ticketContract(tk, planning)
	}
	if known != "" {
		system += knownContract
	}
	system += entriesContract(topics)
	trigger := c.Trigger
	prompt := userPrompt(a, now, promptRun{Trigger: trigger, Wake: c.Prompt, Chain: c.Chain, Continuing: continuing, Known: known, Attached: attached},
		rc, convo, inputs)
	if rec != nil {
		prompt = recoveryPrompt(rec, rc, now.Location())
	}
	for name, b := range map[string]string{"system.md": system, "prompt.md": prompt, "schema.json": string(schema)} {
		mt := "text/markdown"
		if strings.HasSuffix(name, ".json") {
			mt = "application/json"
		}
		if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "in", name, mt, []byte(b)); err != nil {
			return nil, nil, err
		}
	}

	runCtx, cancel := context.WithTimeout(ctx, time.Duration(a.Timeout))
	defer cancel()
	res, err := r.Run(runCtx, runner.Request{
		RunID: c.ID, Workdir: workdir, System: system, Prompt: prompt, Inputs: inputs,
		Tools: a.Tools, OutputSchema: schema, Session: sess, Harness: hr,
	}, func(ev runner.Event) { emit(ev.Kind, ev.Payload) })
	if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
		return nil, res, fmt.Errorf("timed out after %s", time.Duration(a.Timeout))
	}
	if err != nil {
		return nil, res, err
	}
	if err := e.Store.PutArtifact(ctx, c.ID, c.Attempt, "out", "output.json", "application/json", res.Output); err != nil {
		return nil, res, err
	}
	out, err := validate(envelope, res.Output)
	if err != nil {
		return nil, res, fmt.Errorf("output does not match the contract: %w", err)
	}
	if planning {
		out.planTicket = tk.ID
	}
	out.known, out.topics = knownEntries, topics
	if figures {
		// The hook already sent the output back for these, up to
		// MaxFigureDenials times (claude-code); this is the same check on
		// what was finally submitted, for every backend. A figure no code
		// produced is reported, not hidden.
		backed := harness.NewBacked()
		for _, in := range inputs {
			backed.AddJSON(in.Content)
		}
		for _, t := range harness.OrbitToolResults(transcript) {
			backed.AddText(t)
		}
		if missing := backed.Unbacked(harness.ReportText(res.Output)); len(missing) > 0 {
			emit("orbit.figures_unverified", map[string]any{"amounts": missing})
			out.Report = strings.TrimRight(out.Report, "\n") + "\n\n_orbit could not trace these amounts to the inputs or a computed result: " +
				strings.Join(missing, ", ") + "._"
		}
	}
	return out, res, nil
}

// validateAgainst checks output against a JSON Schema, naming every failing location.
func validateAgainst(schema, output json.RawMessage) error {
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(schema))
	if err != nil {
		return err
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("contract.json", doc); err != nil {
		return err
	}
	sch, err := c.Compile("contract.json")
	if err != nil {
		return err
	}
	inst, err := jsonschema.UnmarshalJSON(bytes.NewReader(output))
	if err != nil {
		return err
	}
	if err := sch.Validate(inst); err != nil {
		var ve *jsonschema.ValidationError
		if errors.As(err, &ve) {
			return errors.New(leafErrors(ve.BasicOutput()))
		}
		return err
	}
	return nil
}

// validate checks output against the schema the backend was given. Backends
// enforce it too, but not all of them, and not identically.
func validate(schema, output json.RawMessage) (*Output, error) {
	if err := validateAgainst(schema, output); err != nil {
		return nil, err
	}
	var out Output
	if err := json.Unmarshal(output, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// leafErrors lists where and why the output failed, one cause per location.
func leafErrors(o *jsonschema.OutputUnit) string {
	var parts []string
	var walk func(u *jsonschema.OutputUnit)
	walk = func(u *jsonschema.OutputUnit) {
		if u.Error != nil && len(u.Errors) == 0 {
			loc := u.InstanceLocation
			if loc == "" {
				loc = "/"
			}
			parts = append(parts, loc+": "+u.Error.String())
		}
		for i := range u.Errors {
			walk(&u.Errors[i])
		}
	}
	walk(o)
	if len(parts) == 0 {
		return "does not match the schema"
	}
	if len(parts) > 10 {
		parts = append(parts[:10], fmt.Sprintf("and %d more", len(parts)-10))
	}
	return strings.Join(parts, "; ")
}

func (e *Engine) succeed(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, start time.Time, out *Output, res *runner.Result) error {
	types := map[string]actions.Type{}
	for _, t := range actions.Catalog(a, snap.Config.Deployment) {
		types[t.Name] = t
	}
	o := store.Outcome{Questions: out.Questions}
	if out.planTicket != "" && strings.TrimSpace(out.Plan) != "" {
		o.Plan = &store.PlanProposal{TicketID: out.planTicket, Body: strings.TrimSpace(out.Plan)}
	}
	if out.Memory.Update {
		m := out.Memory.Content
		o.Memory = &m
	}
	for _, act := range out.Actions {
		pa := store.ProposedAction{Type: act.Type, Params: act.Params, Reason: act.Reason, Rule: string(a.RuleFor(act.Type))}
		if t, ok := types[act.Type]; !ok {
			pa.Rule, pa.Error = string(spec.RuleDeny), "action type not enabled for this agent"
		} else if err := t.Validate(act.Params); err != nil {
			pa.Rule, pa.Error = string(spec.RuleDeny), "invalid params: "+err.Error()
		}
		if act.Type == "continue" && pa.Error == "" && c.Chain >= actions.MaxContinueChain {
			// Refused here so the run says why; EnqueueContinuation holds the
			// same bound for an action that reaches it anyway.
			pa.Rule, pa.Error = string(spec.RuleDeny), fmt.Sprintf("continuation limit reached: this run is continuation %d of at most %d", c.Chain, actions.MaxContinueChain)
		}
		if len(pa.Params) == 0 {
			pa.Params = json.RawMessage(`{}`)
		}
		o.Actions = append(o.Actions, pa)
	}
	writes, refused := resolveEntryWrites(a.Name, out.topics, out.known, out.Entries)
	o.Entries = writes
	rr := store.RunResult{Attempt: c.Attempt, Status: "succeeded", Summary: out.Summary, Report: out.Report}
	fillResult(&rr, res)
	ids, err := e.Store.RecordOutcome(ctx, c.ID, a.Name, rr, o)
	if err != nil {
		return err
	}
	e.recordEntryWrites(ctx, c, out.Entries, writes, refused)
	metrics.RunFinished(a.Name, "succeeded", runnerKind(snap, a), time.Since(start), rr.CostUSD)

	// Delivery: the report, then each pending approval.
	link := e.runLink(snap, c.ID)
	pr := notify.Priority(out.Notify.Priority)
	if pr == "" {
		pr = notify.Default
	}
	if Attended(c.Trigger) {
		// The person is reading the conversation this run answered in:
		// its reply, questions and approvals are there, not in a channel.
		return nil
	}
	send := a.Notify.When == "always" || (a.Notify.When == "agent" && out.Notify.Send)
	// A person who wrote by mail is answered by mail, as a reply to theirs.
	var inReplyTo, mailSubject string
	if snap.Config.Deployment.Mail != nil {
		var merr error
		if inReplyTo, mailSubject, merr = e.Store.MailForRun(ctx, c.ID); merr != nil {
			e.Log.Warn("mail for run", "run", c.ID, "err", merr)
		}
		send = send || inReplyTo != ""
	}
	if send || len(out.Questions) > 0 {
		title := out.Notify.Title
		if title == "" {
			title = out.Summary
		}
		body := out.Report
		if !send {
			// Only the questions warrant a ping.
			title, body = a.Name+" has a question", ""
		}
		if len(out.Questions) > 0 {
			body += "\n\n**Questions**\n"
			for _, q := range out.Questions {
				body += "\n- " + q
			}
		}
		m := notify.Message{
			Title: a.Name + ": " + title, Body: strings.TrimSpace(body), Priority: pr,
			Tags: []string{"orbit", a.Name}, Link: link,
		}
		if inReplyTo != "" {
			m.InReplyTo = inReplyTo
			if mailSubject != "" && !strings.HasPrefix(strings.ToLower(mailSubject), "re:") {
				mailSubject = "Re: " + mailSubject
			}
			m.Subject = mailSubject
		}
		e.deliver(ctx, snap, a, &c.ID, nil, "report", m)
	}
	for i, pa := range o.Actions {
		if pa.Rule != string(spec.RuleApprove) {
			continue
		}
		id := ids[i]
		e.deliver(ctx, snap, a, &c.ID, &id, "approval", notify.Message{
			Title:    fmt.Sprintf("%s wants to %s", a.Name, pa.Type),
			Body:     pa.Reason + "\n\n```json\n" + string(pa.Params) + "\n```",
			Priority: notify.High,
			Tags:     []string{"orbit", "approval"},
			Link:     e.ApprovalLink(snap, id),
			Buttons:  []notify.Button{{Label: "Review", URL: e.ApprovalLink(snap, id)}},
		})
	}
	return nil
}

func (e *Engine) fail(ctx context.Context, snap *Snapshot, a *spec.Agent, c *store.ClaimedRun, start time.Time, runErr error, r *runner.Result) error {
	e.Log.Warn("run failed", "run", c.ID, "agent", c.Agent, "err", runErr)
	rr := store.RunResult{Attempt: c.Attempt, Status: "failed", Error: runErr.Error()}
	fillResult(&rr, r)
	if err := e.Store.FinishRun(context.WithoutCancel(ctx), c.ID, rr); err != nil {
		return err
	}
	metrics.RunFinished(c.Agent, "failed", runnerKind(snap, a), time.Since(start), rr.CostUSD)
	if a != nil && !Attended(c.Trigger) {
		e.deliver(ctx, snap, a, &c.ID, nil, "failure", notify.Message{
			Title: a.Name + " run failed", Body: runErr.Error(), Priority: notify.High,
			Tags: []string{"orbit", "warning"}, Link: e.runLink(snap, c.ID),
		})
	}
	return nil
}

// Attended reports whether a run's trigger means its person is watching
// the conversation: a message they sent. Such a run
// notifies nothing (its reply, questions, approvals and failure are in the
// thread); channels are for unattended runs (schedule, continue, build).
func Attended(trigger string) bool { return trigger == "message" }

func runnerKind(snap *Snapshot, a *spec.Agent) string {
	if a == nil {
		return ""
	}
	return snap.Config.Deployment.Runners[a.Runner].Kind
}

func fillResult(rr *store.RunResult, r *runner.Result) {
	if r == nil {
		return
	}
	rr.Output, rr.Usage, rr.CostUSD, rr.SessionID = r.Output, r.Usage, r.CostUSD, r.SessionID
	rr.SessionCostUSD = r.SessionCostUSD
}

// deliver sends to every channel of the agent and records each attempt. A
// failed channel does not stop the others.
func (e *Engine) deliver(ctx context.Context, snap *Snapshot, a *spec.Agent, runID, actionID *string, purpose string, m notify.Message) {
	if purpose == "report" && a.Notify.When == "never" {
		return
	}
	ctx = context.WithoutCancel(ctx)
	if e.Outbox {
		// Control adds the link (an approval link is signed with a secret
		// a worker does not hold) when it sends.
		m.Link, m.Buttons = "", nil
		b, err := json.Marshal(m)
		if err == nil {
			err = e.Store.Enqueue(ctx, e.Worker, store.OutboxItem{Agent: a.Name, RunID: runID, ActionID: actionID, Purpose: purpose, Message: b})
		}
		if err != nil {
			e.Log.Error("record notification for control", "purpose", purpose, "err", err)
		}
		return
	}
	// On control only: the token is signed with the secret a worker does not hold.
	e.mailHeaders(ctx, snap, runID, &m)
	for _, name := range a.Notify.Channels {
		ch := snap.Channels[name]
		sendCtx, cancel := context.WithTimeout(ctx, time.Minute)
		err := ch.Send(sendCtx, m)
		cancel()
		metrics.Delivery(name, err)
		if err != nil {
			e.Log.Warn("delivery failed", "channel", name, "purpose", purpose, "err", err)
		}
		if rerr := e.Store.RecordDelivery(ctx, runID, actionID, name, purpose, err); rerr != nil {
			e.Log.Error("record delivery", "err", rerr)
		}
	}
}

// mailHeaders names the run's conversation in m's reply address and Message-ID when orbit has a mailbox, so a reply to the mail continues that conversation (internal/mail).
func (e *Engine) mailHeaders(ctx context.Context, snap *Snapshot, runID *string, m *notify.Message) {
	mc := snap.Config.Deployment.Mail
	if mc == nil || runID == nil {
		return
	}
	conv, err := e.Store.RunConversation(ctx, *runID)
	if err != nil {
		e.Log.Warn("mail headers", "run", *runID, "err", err)
		return
	}
	tok := mail.Token(e.Secret, conv)
	m.ReplyTo = mail.ReplyAddress(mc.Address, tok)
	m.MessageID = mail.MessageID(mc.Address, tok)
	ref := mail.ThreadRef(mc.Address, tok)
	if m.InReplyTo != "" {
		ref += " " + m.InReplyTo
	}
	m.References = ref
}

func (e *Engine) runLink(snap *Snapshot, runID string) string {
	if snap.Config.Deployment.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(snap.Config.Deployment.PublicURL, "/") + "/ui/runs/" + runID
}

// ApprovalLink is the page where a person approves or rejects an action. The
// signature lets the link work from a phone without a login while naming
// exactly one action.
func (e *Engine) ApprovalLink(snap *Snapshot, actionID string) string {
	if snap.Config.Deployment.PublicURL == "" {
		return ""
	}
	return strings.TrimRight(snap.Config.Deployment.PublicURL, "/") + "/ui/actions/" + actionID + "?sig=" + e.ApprovalSig(actionID)
}

// ApprovalSig is an HMAC of the action id under the deployment secret.
func (e *Engine) ApprovalSig(actionID string) string {
	m := hmac.New(sha256.New, e.Secret)
	m.Write([]byte("action:" + actionID))
	return hex.EncodeToString(m.Sum(nil))
}

// CheckApprovalSig reports whether sig is ApprovalSig(actionID).
func (e *Engine) CheckApprovalSig(actionID, sig string) bool {
	want := e.ApprovalSig(actionID)
	return hmac.Equal([]byte(want), []byte(sig))
}

// DeliverOutbox sends every notification workers recorded, through this
// instance's channels, with the links it signs, and records each attempt
// as deliver does. Control instances only.
func (e *Engine) DeliverOutbox(ctx context.Context) error {
	for {
		it, err := e.Store.ClaimOutbox(ctx)
		if err != nil || it == nil {
			return err
		}
		snap := e.Current()
		var m notify.Message
		if err := json.Unmarshal(it.Message, &m); err != nil {
			e.Log.Error("outbox: unreadable notification", "id", it.ID, "err", err)
		} else if a, ok := snap.Config.Agents[it.Agent]; !ok {
			e.Log.Warn("outbox: agent no longer configured; not sent", "id", it.ID, "agent", it.Agent)
		} else {
			switch {
			case it.Purpose == "approval" && it.ActionID != nil:
				m.Link = e.ApprovalLink(snap, *it.ActionID)
				m.Buttons = []notify.Button{{Label: "Review", URL: m.Link}}
			case it.RunID != nil:
				m.Link = e.runLink(snap, *it.RunID)
			}
			e.deliver(ctx, snap, a, it.RunID, it.ActionID, it.Purpose, m)
		}
		if err := e.Store.OutboxSent(context.WithoutCancel(ctx), it.ID); err != nil {
			return err
		}
	}
}

// ExecuteActions runs every approved action, one at a time, until none is
// left. Auto actions arrive here approved by rule, through the same path as
// a person's approval.
func (e *Engine) ExecuteActions(ctx context.Context) error {
	snap := e.Current()
	ex := &actions.Executor{Deployment: snap.Config.Deployment, Channels: snap.Channels, Client: e.Client, Env: e.Env,
		Continue: func(ctx context.Context, agent, fromRun string, after time.Duration, note string) (string, bool, error) {
			return e.Store.EnqueueContinuation(ctx, store.Continuation{
				Agent: agent, FromRun: fromRun, After: after, Note: note, MaxChain: actions.MaxContinueChain})
		}}
	for {
		act, err := e.Store.ClaimApprovedAction(ctx)
		if err != nil || act == nil {
			return err
		}
		actx, cancel := context.WithTimeout(ctx, ActionTimeout)
		result, xerr := ex.Execute(actx, actions.Call{Agent: act.Agent, RunID: act.RunID, Type: act.Type, Params: act.Params})
		cancel()
		if err := e.Store.FinishAction(context.WithoutCancel(ctx), act.ID, result, xerr); err != nil {
			return err
		}
		metrics.ActionExecuted(act.Agent, act.Type, xerr)
		if xerr != nil {
			e.Log.Warn("action failed", "action", act.ID, "type", act.Type, "err", xerr)
			if a, ok := snap.Config.Agents[act.Agent]; ok {
				e.deliver(ctx, snap, a, &act.RunID, &act.ID, "action", notify.Message{
					Title: fmt.Sprintf("%s: %s failed", act.Agent, act.Type), Body: xerr.Error(),
					Priority: notify.High, Tags: []string{"orbit", "warning"}, Link: e.runLink(snap, act.RunID),
				})
			}
		}
	}
}

func recFrom(r *recovery) int {
	if r == nil {
		return 0
	}
	return r.from
}
