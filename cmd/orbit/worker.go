package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/kensac/orbit/internal/claudeproto"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// outboxEvery is how often control sends workers' notifications and executes the actions their runs proposed: the latency a worker's run adds over one executed on control, which wakes both at once.
const outboxEvery = 5 * time.Second

// pool is a host's executors: N goroutines claiming what Claim allows.
type pool struct {
	Store  *store.Store
	Engine *engine.Engine
	Log    *slog.Logger
	N      int
	Name   string
	Wake   <-chan struct{}
	Claim  store.Claim
	// Before runs after a claim and before the run (a worker refreshes its configuration from the database); After runs once the run ends.
	Before func(context.Context)
	After  func()
}

// start runs the executors until claimCtx ends; runs get runCtx, cancelled only at a drain's cap. The returned group is done when every executor has stopped.
func (p *pool) start(claimCtx, runCtx context.Context) *sync.WaitGroup {
	var wg sync.WaitGroup
	for i := 0; i < p.N; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cl := p.Claim
			cl.Worker = fmt.Sprintf("%s/%d", p.Name, i)
			for claimCtx.Err() == nil {
				c, err := p.Store.ClaimRunFor(claimCtx, cl)
				if err != nil {
					if claimCtx.Err() == nil {
						p.Log.Error("claim", "err", err)
					}
					sleep(claimCtx, 5*time.Second)
					continue
				}
				if c == nil {
					waitFor(claimCtx, p.Wake, 5*time.Second)
					continue
				}
				if p.Before != nil {
					p.Before(claimCtx)
				}
				if err := p.Engine.Execute(runCtx, c); err != nil {
					p.Log.Error("execute", "run", c.ID, "err", err)
				}
				if p.After != nil {
					p.After()
				}
			}
		}(i)
	}
	return &wg
}

// awaitDrain waits for the executors to finish the runs they hold, up to capAfter, or a second signal, and hands the rest back.
func awaitDrain(executors *sync.WaitGroup, capAfter time.Duration, cancelRuns context.CancelFunc, log *slog.Logger) {
	log.Info("draining: waiting for the runs held here", "cap", capAfter.String())
	// A second signal is an operator who will not wait: hand back now.
	again := make(chan os.Signal, 1)
	signal.Notify(again, syscall.SIGINT, syscall.SIGTERM)
	defer signal.Stop(again)
	drained := make(chan struct{})
	go func() { executors.Wait(); close(drained) }()
	select {
	case <-drained:
	case <-time.After(capAfter):
		log.Warn("draining: cap reached; handing the remaining runs back")
		cancelRuns()
		<-drained
	case <-again:
		log.Warn("draining: second signal; handing the remaining runs back")
		cancelRuns()
		<-drained
	}
}

func setHarness(eng *engine.Engine, log *slog.Logger) {
	if bin, err := os.Executable(); err == nil {
		eng.Harness = bin
	} else {
		log.Warn("orbit's tools and hooks are off: cannot find this binary", "err", err)
	}
}

func deliverOutbox(ctx context.Context, eng *engine.Engine, log *slog.Logger) {
	if err := eng.DeliverOutbox(ctx); err != nil && ctx.Err() == nil {
		log.Error("outbox", "err", err)
	}
}

// storedConfig is the configuration control recorded, as a worker runs it.
func storedConfig(ctx context.Context, s *store.Store) (*spec.Config, error) {
	dep, rows, err := s.StoredConfig(ctx)
	if err != nil {
		return nil, err
	}
	agents := make([]spec.StoredAgent, len(rows))
	for i, r := range rows {
		agents[i] = spec.StoredAgent{Name: r.Name, SHA: r.SHA, JSON: r.JSON}
	}
	return spec.FromStored(dep, agents)
}

// claudeBackend names the claude-code runner's credential: either variable will do (claudeproto.Credential picks the token when both are set).
const claudeBackend = claudeproto.OAuthToken + " or " + claudeproto.APIKey

// missingEnv lists the variables the agents this host may run reference and its environment lacks: a source or connector credential for a capability it advertises, or the backend's.
func missingEnv(cfg *spec.Config, caps spec.Capabilities, lookup func(string) (string, bool)) []string {
	want := map[string]bool{}
	var backend bool
	for _, a := range cfg.Agents {
		if !a.IsEnabled() || !caps.Covers(a) {
			continue
		}
		for _, s := range a.Sources {
			for _, r := range s.EnvRefs() {
				want[r] = true
			}
		}
		for _, n := range a.Connectors {
			for _, r := range connectorEnv(cfg.Deployment.Connectors[n]) {
				want[r] = true
			}
		}
		rs := cfg.Deployment.Runners[a.Runner]
		switch {
		case rs.APIKeyEnv != "":
			want[rs.APIKeyEnv] = true
		case rs.Kind == "claude-code":
			backend = true
		}
	}
	var out []string
	for k := range want {
		if v, ok := lookup(k); !ok || v == "" {
			out = append(out, k)
		}
	}
	if backend && claudeproto.Credential(lookup) == "" {
		out = append(out, claudeBackend)
	}
	sort.Strings(out)
	return out
}

func workerSettings(lookup func(string) (string, bool)) (int, spec.Capabilities, error) {
	workers, ok := lookup("ORBIT_WORKERS")
	if !ok || workers == "" {
		workers = "2"
	}
	n, err := strconv.Atoi(workers)
	if err != nil || n < 1 {
		return 0, spec.Capabilities{}, errors.New("ORBIT_WORKERS: want a positive integer")
	}
	capabilityText, _ := lookup("ORBIT_CAPABILITIES")
	caps, err := spec.ParseCapabilities(capabilityText)
	if err != nil {
		return 0, spec.Capabilities{}, fmt.Errorf("ORBIT_CAPABILITIES: %w", err)
	}
	return n, caps, nil
}

func workerPreflight(ctx context.Context, s *store.Store, w io.Writer, lookup func(string) (string, bool)) error {
	workers, caps, err := workerSettings(lookup)
	if err != nil {
		return fmt.Errorf("worker preflight: %w", err)
	}
	current, err := s.SchemaCurrent(ctx)
	if err != nil {
		return fmt.Errorf("worker preflight: checking database schema: %w", err)
	}
	if !current {
		return errors.New("worker preflight: database schema is older than this image; control must migrate it first")
	}
	cfg, err := storedConfig(ctx, s)
	if errors.Is(err, store.ErrNotFound) {
		return errors.New("worker preflight: control has not recorded a configuration")
	}
	if err != nil {
		return fmt.Errorf("worker preflight: reading control configuration: %w", err)
	}
	missing := missingEnv(cfg, caps, lookup)
	if len(missing) > 0 {
		return fmt.Errorf("worker preflight: required environment variables are unset: %s", strings.Join(missing, ", "))
	}
	covered := 0
	for _, agent := range cfg.Agents {
		if agent.IsEnabled() && caps.Covers(agent) {
			covered++
		}
	}
	capabilityText := caps.String()
	if capabilityText == "" {
		capabilityText = "none"
	}
	_, _ = fmt.Fprintf(w, "worker preflight: ok: schema current; %d agent(s) covered; %d worker(s); capabilities %s; required environment is present\n", covered, workers, capabilityText)
	return nil
}

// worker is `orbit worker`: executors only. It claims the runs whose agents this host's capabilities cover, runs them from the specs control recorded in the database, and writes everything to the database: no API, no scheduler, no delivery, no actions, no builds, no migrations. It holds the database URL, its backend credential, and the source credentials of its capabilities, nothing else (docs/agents/deployment.md, Worker fleet).
func worker(ctx context.Context, log *slog.Logger) error {
	workers, caps, err := workerSettings(os.LookupEnv)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	name := env("ORBIT_WORKER_NAME", hostname)
	log = log.With("worker", name)

	return withStore(ctx, func(s *store.Store) error {
		// Control migrates; a worker on a newer image waits for it rather than run against a schema older than its code.
		cfg, err := waitForControl(ctx, s, log)
		if err != nil {
			return err
		}
		eng := &engine.Engine{
			Store: s, DataDir: env("ORBIT_DATA_DIR", "/data"), Env: os.LookupEnv,
			Client: &http.Client{Timeout: 2 * time.Minute}, NewRunner: runner.New, Log: log,
			Live: runner.NewLive(runner.MaxLive), Outbox: true, Worker: name, SessionStore: true,
		}
		setHarness(eng, log)
		if err := eng.SetConfig(cfg); err != nil {
			return err
		}
		metrics.ConfigValid.Set(1)
		if miss := missingEnv(cfg, caps, os.LookupEnv); len(miss) > 0 {
			log.Warn("agents this worker may claim reference unset variables; their runs will fail here", "missing", miss)
		}

		drain := engine.NewDrain()
		eng.Drain = drain
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

		// /healthz (the image's HEALTHCHECK), /readyz and /metrics only.
		mux := http.NewServeMux()
		mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("ok\n")) })
		mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
			pctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
			defer cancel()
			if drain.Draining() {
				http.Error(w, "draining", http.StatusServiceUnavailable)
				return
			}
			if err := s.Pool.Ping(pctx); err != nil {
				http.Error(w, "database: "+err.Error(), http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ready\n"))
		})
		mux.Handle("GET /metrics", promhttp.Handler())
		hs := httpServer(env("ORBIT_LISTEN", ":8080"), mux)
		ln, err := net.Listen("tcp", hs.Addr)
		if err != nil {
			return fmt.Errorf("listen: %w", err)
		}
		go func() {
			if err := hs.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "addr", hs.Addr, "err", err)
			}
		}()

		var known string
		refresh := func(ctx context.Context) {
			cfg, err := storedConfig(ctx, s)
			if err != nil {
				metrics.ConfigValid.Set(0)
				log.Error("config from the database; keeping the previous one", "err", err)
				return
			}
			if err := eng.SetConfig(cfg); err != nil {
				log.Error("config from the database; keeping the previous one", "err", err)
				return
			}
			metrics.ConfigValid.Set(1)
			if fp := fingerprint(cfg); fp != known {
				known = fp
				log.Info("config loaded from the database", "agents", len(cfg.Agents))
				if miss := missingEnv(cfg, caps, os.LookupEnv); len(miss) > 0 {
					log.Warn("agents this worker may claim reference unset variables; their runs will fail here", "missing", miss)
				}
			}
		}
		var mu sync.Mutex
		executors := (&pool{
			Store: s, Engine: eng, Log: log, N: workers, Name: name, Wake: nil,
			Claim: store.Claim{Any: caps.Any, Capabilities: caps.List},
			// The run executes the agent's spec as control recorded it at this moment, the one the claim's needs were checked against.
			Before: func(ctx context.Context) { mu.Lock(); defer mu.Unlock(); refresh(ctx) },
		}).start(claimCtx, runCtx)

		log.Info("orbit worker", "addr", hs.Addr, "workers", workers, "capabilities", caps.String(), "agents", len(cfg.Agents))
		<-drain.Done()
		awaitDrain(executors, drainMax(eng.Current().Config, log), cancelRuns, log)
		eng.Live.Close()
		log.Info("drained; exiting")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = hs.Shutdown(shutCtx)
		cancel()
		return nil
	})
}

// waitForControl returns the stored configuration once the schema is current and control has recorded a deployment.
func waitForControl(ctx context.Context, s *store.Store, log *slog.Logger) (*spec.Config, error) {
	for {
		ok, err := s.SchemaCurrent(ctx)
		if err == nil && ok {
			var cfg *spec.Config
			cfg, err = storedConfig(ctx, s)
			if err == nil {
				return cfg, nil
			}
			if errors.Is(err, store.ErrNotFound) {
				err = errors.New("control has not recorded a configuration yet")
			}
		} else if err == nil {
			err = errors.New("the database schema is older than this binary; waiting for control to migrate it")
		}
		log.Warn("worker waiting", "reason", err.Error())
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Second):
		}
	}
}

func fingerprint(cfg *spec.Config) string {
	parts := make([]string, 0, len(cfg.Agents))
	for n, a := range cfg.Agents {
		parts = append(parts, n+"="+a.SHA)
	}
	sort.Strings(parts)
	return strings.Join(parts, ",")
}
