// Command orbit is the whole service: the scheduler, the executors, the API,
// and the operator commands.
//
//	orbit serve              control: API, scheduler, delivery, actions, config sync, and executors
//	orbit worker             executors only: claim and run what this host's capabilities cover
//	orbit migrate            apply database migrations and exit
//	orbit validate [dir]     load and check a configuration directory
//	orbit run <agent> [prompt]   queue a manual run (a running serve picks it up)
//	orbit approve <action-id> / orbit reject <action-id>
//	orbit healthcheck [--ready]  exit 0 if the local server answers /healthz (/readyz)
//	orbit drain              tell the local server to drain (a deploy replacing it)
//	orbit import claude-memory <dir> --scope <s> [--apply]   seed memory entries through the API
//	orbit mcp --root <dir> --state <dir>   orbit's tool server, started by Claude Code
//	orbit hook --root <dir> --state <dir> [--figures]   orbit's hook, run by Claude Code
//	orbit tls-cert --dir <d> [--owner uid:gid]   orbit-db's self-signed TLS certificate
//	orbit sandboxd           the build sandbox service (holds the docker socket)
//	orbit egress             the allowlisting proxy, a sandbox's only route out
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
	"github.com/kensac/orbit/internal/build"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/harness"
	"github.com/kensac/orbit/internal/mail"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/scheduler"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/tickets"
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
	case "validate":
		err = validate(os.Args[2:])
	case "run":
		err = enqueue(ctx, os.Args[2:])
	case "approve", "reject":
		err = decide(ctx, os.Args[1] == "approve", os.Args[2:])
	case "healthcheck":
		err = healthcheck(len(os.Args) > 2 && os.Args[2] == "--ready")
	case "drain":
		err = drainCmd()
	case "import":
		err = importCmd(ctx, os.Args[2:])
	case "mcp", "hook":
		err = harnessCmd(os.Args[1], os.Args[2:])
	case "tls-cert":
		err = tlsCertCmd(os.Args[2:])
	case "sandboxd":
		err = sandboxdCmd(ctx, log)
	case "egress":
		err = egressCmd(ctx, log)
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
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *root == "" {
		return errors.New(name + ": --root is required")
	}
	if name == "mcp" {
		return harness.ServeMCP(os.Stdin, os.Stdout, *root, *state, revision)
	}
	return harness.Hook(os.Stdin, os.Stdout, harness.HookOptions{Root: *root, Figures: *figures, State: *state})
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: orbit serve | worker | migrate | validate [dir] | run <agent> [prompt] | approve <id> | reject <id> | healthcheck [--ready] | drain | import claude-memory <dir> | version | mcp | hook | tls-cert | sandboxd | egress")
	os.Exit(2)
}

// healthcheck is the image's HEALTHCHECK (liveness, /healthz), and with
// --ready the rollout's readiness wait (/readyz): the image has no curl.
func healthcheck(ready bool) error {
	path := "/healthz"
	if ready {
		path = "/readyz"
	}
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + local(env("ORBIT_LISTEN", ":8080")) + path)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s: %s", path, resp.Status)
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
	}
	if g := cfg.Deployment.GitHub; g != nil {
		refs[g.PrivateKeyEnv] = true
	}
	if m := cfg.Deployment.Mail; m != nil {
		refs[m.IMAP.UsernameEnv], refs[m.IMAP.PasswordEnv] = true, true
	}
	if b := cfg.Deployment.Build; b != nil {
		refs[b.TokenEnv] = true
		refs["CLAUDE_CODE_OAUTH_TOKEN"] = true
	}
	names := make([]string, 0, len(cfg.Agents))
	for n := range cfg.Agents {
		names = append(names, n)
	}
	sort.Strings(names)
	fmt.Printf("ok: %d agent(s): %s\n", len(names), strings.Join(names, ", "))
	var env []string
	for r := range refs {
		env = append(env, r)
	}
	sort.Strings(env)
	if len(env) > 0 {
		fmt.Printf("environment referenced: %s\n", strings.Join(env, ", "))
	}
	return nil
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

		// SIGTERM, or POST /api/v1/drain on the internal listener, drains:
		// no new claims, not ready, the API and streams still served, the
		// runs held here finish under their own timeouts. life outlives
		// the signal until the drain is over; runCtx is cancelled only at
		// the drain's cap, which hands the remaining runs back.
		drain := engine.NewDrain()
		eng.Drain = drain
		eng.Builder = &build.Runner{WorkRoot: filepath.Join(eng.DataDir, "builds")}
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

		stopStreams := make(chan struct{})
		apiSrv := &api.Server{Store: s, Engine: eng, Token: token, Verifier: verifier, Log: log, Wake: nudge, Stop: stopStreams,
			ConfigState: configState, Drain: drain, Reload: reloadNow, OAuth: oauth}
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
		// The ticket loop starts builds and proposes merges, so it stops
		// with claims: a draining instance leaves both to the ready one.
		loops.Add(1)
		go func() {
			defer loops.Done()
			(&tickets.Loop{
				Store: s, Config: func() *spec.Config { return eng.Current().Config }, Env: os.LookupEnv,
				Client: &http.Client{Timeout: 2 * time.Minute}, Revision: revision, Log: log.With("component", "tickets"),
				Approval: eng.NotifyApproval, Wake: nudge,
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
				if n, err := s.BackfillJournal(life, all); err != nil {
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
					deliverOutbox(claimCtx, eng, log)
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
				if err := eng.ExecuteActions(claimCtx); err != nil && claimCtx.Err() == nil {
					log.Error("actions", "err", err)
				}
			}
		}()

		loops.Add(1)
		go func() {
			defer loops.Done()
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			last := configHash(dir) + definitionGeneration(life, s)
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
	rows, err := s.Pool.Query(ctx, `SELECT agent, max(finished_at) FROM runs WHERE status IN ('succeeded', 'skipped') GROUP BY agent`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var agent string
		var t time.Time
		if err := rows.Scan(&agent, &t); err != nil {
			return err
		}
		metrics.SeedLastSuccess(agent, t)
	}
	return rows.Err()
}

func refreshGauges(ctx context.Context, s *store.Store, log *slog.Logger) {
	var pending, queued int
	err := s.Pool.QueryRow(ctx, `SELECT
		(SELECT count(*) FROM actions WHERE status = 'pending'),
		(SELECT count(*) FROM runs WHERE status = 'queued' AND (run_after IS NULL OR run_after <= now()))`).Scan(&pending, &queued)
	if err != nil {
		if ctx.Err() == nil {
			log.Warn("gauges", "err", err)
		}
		return
	}
	metrics.Pending.Set(float64(pending))
	metrics.Queued.Set(float64(queued))
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
