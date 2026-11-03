// Command orbit is the whole service: the scheduler, the executors, the API,
// and the operator commands.
//
//	orbit serve              control: API, scheduler, delivery, actions, config sync, and executors
//	orbit worker             executors only: claim and run what this host's capabilities cover
//	orbit migrate            apply database migrations and exit
//	orbit restore-check [--expect FILE]   read a restored orbit-db read only (deploy/restore-drill.sh)
//	orbit preflight [--config DIR]   check this binary can migrate the database and the running one survives it, changing nothing
//	orbit validate [dir]     load and check a configuration directory
//	orbit run <agent> [prompt]   queue a manual run (a running serve picks it up)
//	orbit approve <action-id> / orbit reject <action-id>
//	orbit healthcheck [--ready | --url <url>]  exit 0 if the local server answers /healthz (/readyz), or url answers 200
//	orbit drain              tell the local server to drain (a deploy replacing it)
//	orbit import claude-memory <dir> --scope <s> [--apply]   seed memory entries through the API
//	orbit import skills <dir> [--attach <agent>] [--apply]   seed the skills library through the API
//	orbit import claude-sessions <dir> [--apply]   import Claude Code session transcripts through the API
//	orbit sync claude [--memory-dir <dir>] [--bundle <tar> | --mcp-patch <yaml>] [--apply <digest>]   sync orbit's skills and memory, or export local Claude config
//	orbit mcp --root <dir> --state <dir>   orbit's tool server, started by Claude Code
//	orbit hook --root <dir> --state <dir> [--figures] [--status <socket>]   orbit's hook, run by Claude Code
//	orbit agent-hook --root <dir> --def <hook>   one of an agent's own hooks, run by Claude Code
//	orbit tls-cert --dir <d> [--owner uid:gid]   orbit-db's self-signed TLS certificate
//	orbit sandboxd           the build sandbox service (holds the docker socket)
//	orbit egress             the allowlisting proxy, a sandbox's only route out
//	orbit credproxy          a sandbox's route to its backend, adding the credential it never holds
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/auth"
	"github.com/kensac/orbit/internal/boxkeep"
	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/github"
	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/mail"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/push"
	"github.com/kensac/orbit/internal/repos"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/scheduler"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/tickets"
	subusage "github.com/kensac/orbit/internal/usage"
)

// revision is set at build time (-ldflags -X main.revision=<sha>).
var revision = "unknown"

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil)).With("revision", revision)
	slog.SetDefault(log)
	if len(os.Args) < 2 {
		usage()
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve(ctx, log)
	case "worker":
		err = worker(ctx, log)
	case "migrate":
		err = withStore(ctx, func(s *store.Store) error { return s.Migrate(ctx) })
	case "preflight":
		err = preflightCmd(ctx, os.Args[2:])
	case "validate":
		err = validate(os.Args[2:])
	case "restore-check":
		err = restoreCheckCmd(ctx, os.Args[2:])
	case "run":
		err = enqueue(ctx, os.Args[2:])
	case "approve", "reject":
		err = decide(ctx, os.Args[1] == "approve", os.Args[2:])
	case "healthcheck":
		err = healthcheckCmd(os.Args[2:])
	case "drain":
		err = drainCmd()
	case "import":
		err = importCmd(ctx, os.Args[2:])
	case "sync":
		err = syncCmd(ctx, os.Args[2:], os.Stdout)
	case "mcp", "hook":
		err = harnessCmd(os.Args[1], os.Args[2:])
	case "agent-hook":
		os.Exit(agentHookCmd(os.Args[2:]))
	case "tls-cert":
		err = tlsCertCmd(os.Args[2:])
	case "sandboxd":
		err = sandboxdCmd(ctx, log)
	case "egress":
		err = egressCmd(ctx, log)
	case "credproxy":
		err = credproxyCmd(ctx, log)
	case "vapid":
		err = vapid()
	case "version":
		fmt.Println(revision)
	default:
		usage()
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "orbit:", err)
		os.Exit(1)
	}
}

// harnessCmd is the code Claude Code runs on orbit's behalf, inside the
// model's process tree: it reads only under --root and needs nothing from
// orbit's environment, which it does not have.
func harnessCmd(name string, args []string) error {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	root := fs.String("root", "", "the run's working directory")
	figures := fs.Bool("figures", false, "check the output's money amounts")
	state := fs.String("state", "", "the directory the tool server and the hook share")
	statusSock := fs.String("status", "", "the run's status socket the hook posts every event to")
	search := fs.String("search", "", "the engine's search socket for this agent (mcp)")
	repoSock := fs.String("repos", "", "the engine's repository tools socket for this agent (mcp)")
	promSock := fs.String("prometheus", "", "the engine's prometheus tool socket for this agent (mcp)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return errors.New(name + ": --root is required")
	}
	if name == "mcp" {
		return harness.ServeMCP(os.Stdin, os.Stdout, *root, *state, *search, *repoSock, *promSock, revision)
	}
	return harness.Hook(os.Stdin, os.Stdout, harness.HookOptions{Root: *root, Figures: *figures, State: *state, Status: *statusSock})
}

// agentHookCmd runs one of an agent's own hooks for Claude Code (spec hooks): the definition is a flag orbit wrote, the event is stdin, and the exit code is the CLI's to read.
func agentHookCmd(args []string) int {
	fs := flag.NewFlagSet("agent-hook", flag.ContinueOnError)
	root := fs.String("root", "", "the run's working directory")
	def := fs.String("def", "", "the hook, as orbit encoded it")
	if err := fs.Parse(args); err != nil {
		return 1
	}
	d, err := harness.DecodeAgentHook(*def)
	if err != nil || *root == "" {
		fmt.Fprintln(os.Stderr, "orbit agent-hook: --root and a valid --def are required")
		return 1
	}
	return harness.AgentHook(os.Stdin, os.Stdout, os.Stderr, d, *root)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: orbit serve | worker | migrate | preflight [--config dir] | validate [dir] | restore-check [--expect file] | run <agent> [prompt] | approve <id> | reject <id> | healthcheck [--ready | --url <url>] | drain | vapid | import claude-memory|skills|claude-sessions <dir> | sync claude | version | mcp | hook | tls-cert | sandboxd | egress | credproxy")
	os.Exit(2)
}

// healthcheckCmd is the image's HEALTHCHECK (liveness, /healthz), with --ready the rollout's readiness wait (/readyz), and with --url the healthcheck compose gives sandboxd and egress: the image has no curl.
func healthcheckCmd(args []string) error {
	fs := flag.NewFlagSet("healthcheck", flag.ContinueOnError)
	ready := fs.Bool("ready", false, "probe /readyz instead of /healthz")
	url := fs.String("url", "", "probe this URL instead (sandboxd's and egress's healthchecks)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *url != "" && *ready {
		return errors.New("healthcheck: --url and --ready are exclusive")
	}
	target := *url
	if target == "" {
		path := "/healthz"
		if *ready {
			path = "/readyz"
		}
		target = "http://" + local(env("ORBIT_LISTEN", ":8080")) + path
	}
	return probe(target)
}

// probe is one healthcheck request: 200 within the timeout, or an error naming the answer.
func probe(target string) error {
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get(target) //nolint:gosec // the operator's own probe target, from the command line
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", target, resp.Status)
	}
	return nil
}

// local turns a listen address into one to dial on this host.
func local(addr string) string {
	if strings.HasPrefix(addr, ":") {
		return "127.0.0.1" + addr
	}
	return addr
}

// drainCmd asks the server in this container to drain, over the internal
// listener with the operator token, both from the environment the server
// itself runs with (docker exec orbit orbit drain). It returns at once;
// the server exits when its runs are done.
func drainCmd() error {
	token := os.Getenv("ORBIT_API_TOKEN")
	if token == "" {
		return errors.New("ORBIT_API_TOKEN is not set")
	}
	addr := local(env("ORBIT_INTERNAL_LISTEN", "127.0.0.1:8081"))
	if strings.HasPrefix(addr, "0.0.0.0:") {
		addr = "127.0.0.1" + strings.TrimPrefix(addr, "0.0.0.0")
	}
	req, err := http.NewRequest(http.MethodPost, "http://"+addr+"/api/v1/drain", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("drain: %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	fmt.Println(strings.TrimSpace(string(b)))
	return nil
}

func env(name, def string) string {
	if v := os.Getenv(name); v != "" {
		return v
	}
	return def
}

func withStore(ctx context.Context, f func(*store.Store) error) error {
	url := os.Getenv("ORBIT_DATABASE_URL")
	if url == "" {
		return errors.New("ORBIT_DATABASE_URL is not set")
	}
	s, err := store.Open(ctx, url)
	if err != nil {
		return err
	}
	defer s.Close()
	return f(s)
}

func configDir(args []string) string {
	if len(args) > 0 {
		return args[0]
	}
	return env("ORBIT_CONFIG_DIR", "/config")
}

// validate loads a configuration and lists the environment it needs, so a
// change can be checked in CI and before a deploy.
func validate(args []string) error {
	// The files and the built-in agents; database agents are checked when
	// they are written (POST /api/v1/agents).
	cfg, err := spec.LoadAll(configDir(args), nil)
	if err != nil {
		return err
	}
	names := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("ok: %d agent(s): %s\n", len(names), strings.Join(names, ", "))
	env := referencedEnv(cfg)
	if len(env) > 0 {
		fmt.Printf("environment referenced: %s\n", strings.Join(env, ", "))
	}
	return nil
}

// referencedEnv is the environment a configuration needs, sorted. The claude-code backend's is listed as its two alternatives in one entry.
func referencedEnv(cfg *spec.Config) []string {
	b, _ := json.Marshal(cfg)
	refs := map[string]bool{}
	for _, r := range spec.EnvRefs(string(b)) {
		refs[r] = true
	}
	for _, ch := range cfg.Deployment.Channels {
		for _, v := range []string{ch.TopicEnv, ch.TokenEnv, ch.UsernameEnv, ch.PasswordEnv} {
			if v != "" {
				refs[v] = true
			}
		}
	}
	for _, r := range cfg.Deployment.Runners {
		if r.APIKeyEnv != "" {
			refs[r.APIKeyEnv] = true
		}
		if r.Kind == "claude-code" {
			refs[claudeBackend] = true
		}
	}
	if g := cfg.Deployment.GitHub; g != nil {
		refs[g.PrivateKeyEnv] = true
	}
	for v := range cfg.Deployment.WebhookSecrets {
		refs[v] = true
	}
	if m := cfg.Deployment.Mail; m != nil {
		refs[m.IMAP.UsernameEnv], refs[m.IMAP.PasswordEnv] = true, true
	}
	for _, c := range cfg.Deployment.Connectors {
		for _, v := range connectorEnv(c) {
			refs[v] = true
		}
	}
	// A build or a sandboxed chat hands the sandbox the subscription token itself (engine.buildCredential), so these name it alone.
	if b := cfg.Deployment.Build; b != nil {
		refs[b.TokenEnv] = true
		refs["CLAUDE_CODE_OAUTH_TOKEN"] = true
	}
	if addr, tokenEnv := cfg.Deployment.SandboxService(); addr != "" {
		refs[tokenEnv] = true
		refs["CLAUDE_CODE_OAUTH_TOKEN"] = true
	}
	env := make([]string, 0, len(refs))
	for r := range refs {
		env = append(env, r)
	}
	sort.Strings(env)
	return env
}

// connectorEnv is the environment a connector's calls need: its header's credentials, a configured client's secret, and for oauth the key its tokens are sealed with.
func connectorEnv(c spec.Connector) []string {
	out := spec.EnvRefs(c.Auth.Value)
	if c.Auth.ClientSecretEnv != "" {
		out = append(out, c.Auth.ClientSecretEnv)
	}
	if c.AuthKind() == spec.ConnectorAuthOAuth {
		out = append(out, spec.ConnectorKeyEnv)
	}
	return out
}

func enqueue(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: orbit run <agent> [prompt]")
	}
	return withStore(ctx, func(s *store.Store) error {
		eng := &engine.Engine{Store: s, ConfigDir: configDir(nil)}
		cfg, err := eng.LoadConfig(ctx, nil)
		if err != nil {
			return err
		}
		a, ok := cfg.Agents[args[0]]
		if !ok {
			return fmt.Errorf("no agent %q", args[0])
		}
		if err := engine.SyncAgents(ctx, s, cfg); err != nil {
			return err
		}
		id, err := s.EnqueueRun(ctx, store.NewRun{Agent: a.Name, SpecSHA: a.SHA, Trigger: "manual",
			Prompt: strings.Join(args[1:], " ")})
		if err == nil {
			fmt.Println(id)
		}
		return err
	})
}

func decide(ctx context.Context, approve bool, args []string) error {
	if len(args) != 1 {
		return errors.New("usage: orbit approve|reject <action-id>")
	}
	return withStore(ctx, func(s *store.Store) error { return s.DecideAction(ctx, args[0], approve, "cli") })
}

// ---------------------------------------------------------------- serve

func serve(ctx context.Context, log *slog.Logger) error {
	token := os.Getenv("ORBIT_API_TOKEN")
	if len(token) < 32 {
		// It also signs approval links, so a short one is a forgeable one.
		return errors.New("ORBIT_API_TOKEN must be set, at least 32 characters (openssl rand -hex 32)")
	}
	workers, err := strconv.Atoi(env("ORBIT_WORKERS", "2"))
	if err != nil || workers < 0 {
		return fmt.Errorf("ORBIT_WORKERS: want 0 (control only) or a positive integer")
	}
	// Control runs every agent unless told what this host has.
	caps, err := spec.ParseCapabilities(env("ORBIT_CAPABILITIES", "*"))
	if err != nil {
		return fmt.Errorf("ORBIT_CAPABILITIES: %w", err)
	}
	verifier, err := oidcVerifier()
	if err != nil {
		return err
	}
	if verifier == nil {
		log.Warn("ORBIT_OIDC_ISSUER is not set: the public /api/v1 refuses every request")
	}
	oauth, err := mcpOAuth(token)
	if err != nil {
		return err
	}
	dir := configDir(nil)
	hostname, _ := os.Hostname()
	workerName := env("ORBIT_WORKER_NAME", hostname)

	return withStore(ctx, func(s *store.Store) error {
		if err := s.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		eng := &engine.Engine{
			Store: s, DataDir: env("ORBIT_DATA_DIR", "/data"), ConfigDir: dir, Env: os.LookupEnv,
			Client: &http.Client{Timeout: 2 * time.Minute}, Secret: []byte(token),
			NewRunner: runner.New, Log: log, Live: runner.NewLive(runner.MaxLive),
			Worker: workerName, SessionStore: true,
		}
		setHarness(eng, log)
		if err := eng.Reload(ctx); err != nil {
			return err
		}
		cfg := eng.Current().Config
		metrics.ConfigValid.Set(1)
		configState := &engine.ConfigState{}
		configState.Loaded(time.Now())
		if err := seedMetrics(ctx, s); err != nil {
			return err
		}
		failOrphanedActions(ctx, s, log)

		// A manual run, a message or a decision wakes one executor and the
		// action loop rather than waiting for their next poll.
		wakeRuns := make(chan struct{}, 1)
		wakeActions := make(chan struct{}, 1)
		nudge := func() {
			for _, ch := range []chan struct{}{wakeRuns, wakeActions} {
				select {
				case ch <- struct{}{}:
				default:
				}
			}
		}
		reload := make(chan struct{}, 1)
		eng.Wake = nudge

		// SIGTERM, or POST /api/v1/drain on the internal listener, drains:
		// no new claims, not ready, the API and streams still served, the
		// runs held here finish under their own timeouts. life outlives
		// the signal until the drain is over; runCtx is cancelled only at
		// the drain's cap, which hands the remaining runs back.
		drain := engine.NewDrain()
		eng.Drain = drain
		// One spare clone per build repository: a build starts from a
		// checkout that needs only an incremental fetch.
		// Mirrors of the repositories agents read (repos:), which also seed
		// a build's new clone.
		eng.Repos = &repos.Mirrors{Root: filepath.Join(eng.DataDir, "mirrors")}
		checkouts := &build.Pool{Root: filepath.Join(eng.DataDir, "checkouts"), Warm: 1, Seed: eng.Repos.Has}
		eng.Builder = &build.Runner{WorkRoot: filepath.Join(eng.DataDir, "builds"), Pool: checkouts}
		life, endLife := context.WithCancel(context.WithoutCancel(ctx))
		defer endLife()
		runCtx, cancelRuns := context.WithCancel(life)
		defer cancelRuns()
		claimCtx, stopClaims := context.WithCancel(life)
		defer stopClaims()
		go func() {
			select {
			case <-ctx.Done():
				if drain.Start() {
					log.Info("draining: signal received")
				}
			case <-drain.Done():
			}
			stopClaims()
			metrics.Draining.Set(1)
		}()

		pusher, err := push.FromEnv(s, log.With("component", "push"))
		if err != nil {
			return err
		}
		pollUsage, err := boxkeep.ParsePollUsage(os.Getenv("ORBIT_ACCOUNT_USAGE_POLL"))
		if err != nil {
			return err
		}
		if pusher == nil {
			log.Info("ORBIT_VAPID_* not set: Web Push is off (the inbox still lists what needs attention)")
		}
		// reloadNow puts the files and the database's definitions in force,
		// for the poll below and for the API after it writes a definition.
		var reloadMu sync.Mutex
		reloadNow := func(ctx context.Context) error {
			reloadMu.Lock()
			defer reloadMu.Unlock()
			if err := eng.Reload(ctx); err != nil {
				metrics.ConfigValid.Set(0)
				configState.Failed(err, time.Now())
				return err
			}
			metrics.ConfigValid.Set(1)
			configState.Loaded(time.Now())
			select {
			case reload <- struct{}{}:
			default:
			}
			return nil
		}

		eng.Reloader = reloadNow

		stopStreams := make(chan struct{})
		apiSrv := &api.Server{Store: s, Engine: eng, Token: token, Verifier: verifier, Log: log, Wake: nudge, Stop: stopStreams,
			ConfigState: configState, Drain: drain, OAuth: oauth, Push: pusher,
			WorkspaceGroup: strings.TrimSpace(os.Getenv("ORBIT_WORKSPACE_GROUP")),
			Boxes:          boxClient(func() spec.Deployment { return eng.Current().Config.Deployment }), WorkspaceOrigins: auth.SplitList(os.Getenv("ORBIT_WORKSPACE_ORIGINS"))}
		srv := httpServer(env("ORBIT_LISTEN", ":8080"), apiSrv.Handler())
		internal := httpServer(env("ORBIT_INTERNAL_LISTEN", "127.0.0.1:8081"), apiSrv.InternalHandler())
		// Bind before any executor starts: an instance that cannot serve
		// (its port taken) must not claim runs, or a rollout's readiness
		// wait would be answered by whatever holds the port.
		for _, hs := range []*http.Server{srv, internal} {
			ln, err := net.Listen("tcp", hs.Addr)
			if err != nil {
				return fmt.Errorf("listen: %w", err)
			}
			go func() {
				if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
					log.Error("http", "addr", hs.Addr, "err", err)
				}
			}()
		}

		var loops sync.WaitGroup
		loops.Add(1)
		go func() {
			defer loops.Done()
			watchSandboxd(life, func() *spec.Config { return eng.Current().Config }, log.With("component", "sandboxd-probe"))
		}()
		// The ticket loop starts builds and proposes merges, so it stops
		// with claims: a draining instance leaves both to the ready one.
		loops.Add(1)
		go func() {
			defer loops.Done()
			(&tickets.Loop{
				Store: s, Config: func() *spec.Config { return eng.Current().Config }, Env: os.LookupEnv,
				Client: &http.Client{Timeout: 2 * time.Minute}, Revision: revision, Log: log.With("component", "tickets"),
				Approval: eng.NotifyApproval, Wake: nudge, Restack: eng.Builder.Restack, Checkouts: checkouts,
			}).Run(claimCtx)
		}()

		// The box keeper keeps a GitHub token in every running box and
		// polls accounts' usage when that is on; it stops with claims, so a
		// draining instance leaves the boxes to the ready one.
		loops.Add(1)
		go func() {
			defer loops.Done()
			(&boxkeep.Keeper{
				Store: s, Boxes: apiSrv.Boxes, PollUsage: pollUsage, Log: log.With("component", "boxes"),
				Agents:     func() []string { return boxkeep.BoxAgents(eng.Current().Config) },
				GitHub:     func() *spec.GitHub { return eng.Current().Config.Deployment.GitHub },
				MCPServers: func() map[string]json.RawMessage { return eng.Current().Config.Deployment.Box.MCPJSON() },
				App: func(g *spec.GitHub) (*github.App, error) {
					return github.NewApp(g, os.LookupEnv, &http.Client{Timeout: time.Minute})
				},
			}).Run(claimCtx)
		}()

		// The mailbox poller turns mail into messages; it stops with claims, so a draining instance leaves the mailbox to the ready one (two polling at once is safe: a mail is acted on once, by Message-ID).
		loops.Add(1)
		go func() {
			defer loops.Done()
			(&mail.Poller{
				Store: s, Config: func() *spec.Config { return eng.Current().Config }, Secret: []byte(token),
				Env: os.LookupEnv, Log: log.With("component", "mail"), Wake: nudge,
			}).Run(claimCtx)
		}()

		// The usage poller is best effort and off without usage_poll; it
		// stops with claims, and two instances share one poll per interval
		// (subusage.Poller.maybePoll).
		loops.Add(1)
		go func() {
			defer loops.Done()
			(&subusage.Poller{
				Store: s, Client: &http.Client{}, Env: os.LookupEnv, Worker: eng.Worker, Version: revision,
				Config: func() *spec.Deployment { return &eng.Current().Config.Deployment },
				Log:    log.With("component", "usage"),
			}).Run(claimCtx)
		}()

		// The scheduler only enqueues, so it keeps leading through a drain
		// (fires stay on time, the ready instance claims them) and lets go
		// of the lock when this process ends.
		sched := &scheduler.Scheduler{
			Store: s, Log: log.With("component", "scheduler"), Reload: reload, Now: time.Now,
			Config: func() *spec.Config { return eng.Current().Config },
		}
		loops.Add(1)
		go func() {
			defer loops.Done()
			for life.Err() == nil {
				if err := sched.Run(life); err != nil {
					log.Error("scheduler", "err", err)
					sleep(life, 30*time.Second)
				}
			}
		}()

		// The journal catches up on what its live writes did not project:
		// every run once at start (the backfill of runs from before it
		// existed), then recent runs, which an older instance may still be
		// writing during a deploy.
		loops.Add(1)
		go func() {
			defer loops.Done()
			all := true
			for life.Err() == nil {
				n, failed, err := s.BackfillJournal(life, all)
				for _, f := range failed {
					log.Error("journal backfill skipped a run", "err", f)
				}
				if err != nil {
					if life.Err() == nil {
						log.Error("journal backfill", "err", err)
					}
				} else {
					if n > 0 {
						log.Info("journal synced", "runs", n, "all", all)
					}
					all = false
				}
				sleep(life, journalSyncEvery)
			}
		}()

		executors := (&pool{
			Store: s, Engine: eng, Log: log, N: workers, Name: workerName, Wake: wakeRuns,
			Claim: store.Claim{Any: caps.Any, Capabilities: caps.List, Builds: true},
			// Its actions are ready the moment it finishes; a draining
			// instance leaves them to the ready one.
			After: func() {
				if !drain.Draining() {
					if err := eng.ExecuteActions(claimCtx); err != nil && claimCtx.Err() == nil {
						log.Error("actions", "err", err)
					}
					reconcileTasks(claimCtx, eng, log)
					deliverOutbox(claimCtx, eng, log)
					deliverPush(claimCtx, pusher, log)
				}
			},
		}).start(claimCtx, runCtx)

		// Workers' notifications and the actions their runs proposed are
		// this instance's to send and execute; a short poll keeps their
		// latency close to a run executed here.
		loops.Add(1)
		go func() {
			defer loops.Done()
			t := time.NewTicker(outboxEvery)
			defer t.Stop()
			for {
				select {
				case <-claimCtx.Done():
					return
				case <-t.C:
				}
				if drain.Draining() {
					continue
				}
				deliverOutbox(claimCtx, eng, log)
				deliverPush(claimCtx, pusher, log)
				if err := eng.ExecuteActions(claimCtx); err != nil && claimCtx.Err() == nil {
					log.Error("actions", "err", err)
				}
				// Runs a worker finished, and decisions a person resolved,
				// move their tasks here.
				reconcileTasks(claimCtx, eng, log)
			}
		}()

		loops.Add(1)
		go func() {
			defer loops.Done()
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			last := configHash(dir) + definitionGeneration(life, s)
			// Learning runs are enqueued here, on every serving instance (runs_one_learning keeps each one once).
			var lastLearn time.Time
			for {
				select {
				case <-life.Done():
					return
				case <-wakeActions:
				case <-t.C:
				}
				if !drain.Draining() {
					if err := eng.ExecuteActions(claimCtx); err != nil && claimCtx.Err() == nil {
						log.Error("actions", "err", err)
					}
				}
				if failed, err := s.ReapStale(life, engine.StaleAfter); err != nil {
					log.Error("reap", "err", err)
				} else if len(failed) > 0 {
					log.Warn("failed runs whose worker was lost", "runs", failed)
				}
				failOrphanedActions(life, s, log)
				if !drain.Draining() {
					// A held runner is tried again here (engine/hold.go); a probe that succeeds wakes the executors.
					if _, err := eng.ProbeHolds(life); err != nil {
						log.Error("probe held runners", "err", err)
					}
				}
				if !drain.Draining() && time.Since(lastLearn) >= engine.LearnEvery {
					lastLearn = time.Now()
					if err := eng.EnqueueLearning(life); err != nil {
						log.Error("learning", "err", err)
					} else {
						nudge()
					}
				}
				refreshGauges(life, s, log)
				if h := configHash(dir) + definitionGeneration(life, s); h != last {
					if err := reloadNow(life); err != nil {
						log.Error("config reload failed; keeping the previous configuration", "err", err)
					} else {
						last = h
						log.Info("config reloaded")
					}
				}
			}
		}()

		log.Info("orbit serving", "addr", srv.Addr, "internal_addr", internal.Addr, "workers", workers,
			"capabilities", caps.String(), "agents", len(cfg.Agents), "oidc", verifier != nil)
		<-drain.Done()
		awaitDrain(executors, drainMax(eng.Current().Config, log), cancelRuns, log)
		// No run is in progress here now, so no live process is mid-turn:
		// each is closed between turns, and the next run of its
		// conversation, on whichever instance, resumes its session from
		// the shared data volume.
		eng.Live.Close()
		log.Info("drained; exiting")
		// Streams end last (clients reconnect to the ready instance and
		// replay from Last-Event-ID); Shutdown then waits for the rest.
		close(stopStreams)
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(shutCtx)
		_ = internal.Shutdown(shutCtx)
		cancel()
		endLife()
		loops.Wait()
		return nil
	})
}

// reconcileTasks moves delegated tasks and wakes their parents
// (engine.ReconcileTasks); control only, as it executes the actions that
// create them.
func reconcileTasks(ctx context.Context, eng *engine.Engine, log *slog.Logger) {
	if err := eng.ReconcileTasks(ctx); err != nil && ctx.Err() == nil {
		log.Error("delegation: reconcile", "err", err)
	}
}

// drainMax is ORBIT_DRAIN_MAX, or engine.DrainMax of the configuration in
// force: long enough that every run held here can finish within its own
// timeout.
func drainMax(cfg *spec.Config, log *slog.Logger) time.Duration {
	if os.Getenv("ORBIT_DRAIN") != "" {
		log.Warn("ORBIT_DRAIN is no longer read; ORBIT_DRAIN_MAX bounds a drain")
	}
	if v := os.Getenv("ORBIT_DRAIN_MAX"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d >= 0 {
			return d
		}
		log.Warn("ORBIT_DRAIN_MAX is not a duration; using the default", "value", v)
	}
	return engine.DrainMax(cfg)
}

func failOrphanedActions(ctx context.Context, s *store.Store, log *slog.Logger) {
	if n, err := s.FailOrphanedActions(ctx, engine.OrphanAfter); err != nil {
		if ctx.Err() == nil {
			log.Error("orphaned actions", "err", err)
		}
	} else if n > 0 {
		log.Warn("failed orphaned actions", "count", n)
	}
}

// writeTimeout bounds an ordinary response, so a client that stops reading
// does not hold a handler forever. Generous, because an artifact can be
// several MB to a phone; a run stream clears it (api.streamRun).
const writeTimeout = 5 * time.Minute

// journalSyncEvery is how often the journal catches up on recent runs. A
// reader syncs the run it reads itself, so this only bounds how stale the
// steps of a run nobody streams can be while an older instance writes it.
const journalSyncEvery = 30 * time.Second

// boxClient is sandboxd's client for agents' boxes: the sandboxd chats use
// (spec.Deployment.SandboxService) with its token, made again only when
// either changes.
func boxClient(deployment func() spec.Deployment) func() (*sandbox.Client, error) {
	var mu sync.Mutex
	var key string
	var c *sandbox.Client
	return func() (*sandbox.Client, error) {
		addr, tokenEnv := deployment().SandboxService()
		if addr == "" {
			return nil, errors.New("orbit.yaml names no sandboxd (a sandbox: or build: section)")
		}
		token := os.Getenv(tokenEnv)
		mu.Lock()
		defer mu.Unlock()
		if c != nil && key == addr+"\x00"+token {
			return c, nil
		}
		nc, err := sandbox.NewClient(addr, token)
		if err != nil {
			return nil, fmt.Errorf("%w (%s)", err, tokenEnv)
		}
		c, key = nc, addr+"\x00"+token
		return c, nil
	}
}

func httpServer(addr string, h http.Handler) *http.Server {
	return &http.Server{Addr: addr, Handler: h, ReadHeaderTimeout: 10 * time.Second, WriteTimeout: writeTimeout}
}

// oidcVerifier builds the authentik token check from the environment, or
// returns nil when ORBIT_OIDC_ISSUER is unset. Setting the issuer without
// the client ids or the group is an error, not a looser check.
func oidcVerifier() (*auth.Verifier, error) {
	issuers := auth.SplitList(os.Getenv("ORBIT_OIDC_ISSUER"))
	clients := auth.SplitList(os.Getenv("ORBIT_OIDC_CLIENT_IDS"))
	group := strings.TrimSpace(os.Getenv("ORBIT_OIDC_GROUP"))
	// The MCP sign-in's provider issues the tokens /mcp is called with, so they are accepted wherever an authentik token is.
	if is, id := mcpOAuthEnv(); is != "" && id != "" {
		if !slices.Contains(issuers, is) {
			issuers = append(issuers, is)
		}
		if !slices.Contains(clients, id) {
			clients = append(clients, id)
		}
	}
	if len(issuers) == 0 {
		if len(clients) > 0 || group != "" {
			return nil, errors.New("ORBIT_OIDC_CLIENT_IDS or ORBIT_OIDC_GROUP is set without ORBIT_OIDC_ISSUER")
		}
		return nil, nil
	}
	if len(clients) == 0 || group == "" {
		return nil, errors.New("ORBIT_OIDC_ISSUER needs ORBIT_OIDC_CLIENT_IDS and ORBIT_OIDC_GROUP")
	}
	return auth.New(auth.Config{Issuers: issuers, ClientIDs: clients, Group: group})
}

func mcpOAuthEnv() (issuer, clientID string) {
	issuer = strings.TrimSpace(os.Getenv("ORBIT_MCP_OAUTH_ISSUER"))
	if issuer != "" && !strings.HasSuffix(issuer, "/") {
		issuer += "/"
	}
	return issuer, strings.TrimSpace(os.Getenv("ORBIT_MCP_OAUTH_CLIENT_ID"))
}

// mcpOAuth builds the MCP sign-in proxy from the environment, or nil when ORBIT_MCP_OAUTH_ISSUER is unset.
func mcpOAuth(token string) (*api.OAuthProxy, error) {
	issuer, id := mcpOAuthEnv()
	secret := os.Getenv("ORBIT_MCP_OAUTH_CLIENT_SECRET")
	if issuer == "" {
		if id != "" || secret != "" {
			return nil, errors.New("ORBIT_MCP_OAUTH_CLIENT_ID or _SECRET is set without ORBIT_MCP_OAUTH_ISSUER")
		}
		return nil, nil
	}
	if id == "" || secret == "" {
		return nil, errors.New("ORBIT_MCP_OAUTH_ISSUER needs ORBIT_MCP_OAUTH_CLIENT_ID and ORBIT_MCP_OAUTH_CLIENT_SECRET")
	}
	if os.Getenv("ORBIT_OIDC_GROUP") == "" {
		return nil, errors.New("ORBIT_MCP_OAUTH_ISSUER needs ORBIT_OIDC_GROUP, the group a person must be in")
	}
	o := &api.OAuthProxy{Issuer: issuer, ClientID: id, ClientSecret: secret,
		RedirectHosts: auth.SplitList(os.Getenv("ORBIT_MCP_REDIRECT_HOSTS")), Client: &http.Client{Timeout: 30 * time.Second}}
	o.SetKey([]byte(token))
	return o, nil
}

// definitionGeneration fingerprints the database's agent definitions, so a
// definition another instance wrote reloads this one too.
func definitionGeneration(ctx context.Context, s *store.Store) string {
	n, err := s.DefinitionGeneration(ctx)
	if err != nil {
		return "db?"
	}
	return "db" + strconv.FormatInt(n, 10)
}

// configHash fingerprints every file under dir. A deploy's git pull swaps
// files in place, so polling the content is the dependable signal.
func configHash(dir string) string {
	h := sha256.New()
	_ = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		b, err := os.ReadFile(p) //nolint:gosec // orbit's own read-only config mount, not attacker-controlled
		if err != nil {
			return nil
		}
		_, _ = fmt.Fprintf(h, "%s\x00%d\x00", p, len(b))
		h.Write(b)
		return nil
	})
	return fmt.Sprintf("%x", h.Sum(nil))
}

func seedMetrics(ctx context.Context, s *store.Store) error {
	last, err := s.LastSuccessTimes(ctx)
	if err != nil {
		return err
	}
	for agent, t := range last {
		metrics.SeedLastSuccess(agent, t)
	}
	return nil
}

func refreshGauges(ctx context.Context, s *store.Store, log *slog.Logger) {
	pending, queued, err := s.GaugeCounts(ctx)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("gauges", "err", err)
		}
		return
	}
	metrics.Pending.Set(float64(pending))
	metrics.Queued.Set(float64(queued))
	if age, err := s.OldestQueuedAge(ctx); err != nil {
		if ctx.Err() == nil {
			log.Warn("gauges", "err", err)
		}
	} else {
		metrics.QueuedOldest.Set(age.Seconds())
	}
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func waitFor(ctx context.Context, ch <-chan struct{}, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-ch:
	case <-time.After(d):
	}
}
