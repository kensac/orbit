// Command orbit is the whole service: the scheduler, the executors, the API,
// and the operator commands.
//
//	orbit serve              run everything (the container's command)
//	orbit migrate            apply database migrations and exit
//	orbit validate [dir]     load and check a configuration directory
//	orbit run <agent> [prompt]   queue a manual run (a running serve picks it up)
//	orbit approve <action-id> / orbit reject <action-id>
//	orbit healthcheck        exit 0 if the local server answers /healthz
package main

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/kensac/orbit/internal/api"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/runner"
	"github.com/kensac/orbit/internal/scheduler"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
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
	case "migrate":
		err = withStore(ctx, func(s *store.Store) error { return s.Migrate(ctx) })
	case "validate":
		err = validate(os.Args[2:])
	case "run":
		err = enqueue(ctx, os.Args[2:])
	case "approve", "reject":
		err = decide(ctx, os.Args[1] == "approve", os.Args[2:])
	case "healthcheck":
		err = healthcheck()
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

func usage() {
	fmt.Fprintln(os.Stderr, "usage: orbit serve | migrate | validate [dir] | run <agent> [prompt] | approve <id> | reject <id> | healthcheck | version")
	os.Exit(2)
}

// healthcheck is the image's HEALTHCHECK: the image has no curl.
func healthcheck() error {
	addr := env("ORBIT_LISTEN", ":8080")
	if strings.HasPrefix(addr, ":") {
		addr = "127.0.0.1" + addr
	}
	c := &http.Client{Timeout: 4 * time.Second}
	resp, err := c.Get("http://" + addr + "/healthz")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("healthz: %s", resp.Status)
	}
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
	cfg, err := spec.Load(configDir(args))
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
	cfg, err := spec.Load(configDir(nil))
	if err != nil {
		return err
	}
	a, ok := cfg.Agents[args[0]]
	if !ok {
		return fmt.Errorf("no agent %q", args[0])
	}
	return withStore(ctx, func(s *store.Store) error {
		if err := syncAgents(ctx, s, cfg); err != nil {
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

func syncAgents(ctx context.Context, s *store.Store, cfg *spec.Config) error {
	specs := map[string]store.AgentSpec{}
	for name, a := range cfg.Agents {
		b, err := json.Marshal(a)
		if err != nil {
			return err
		}
		specs[name] = store.AgentSpec{SHA: a.SHA, JSON: b, Enabled: a.IsEnabled()}
	}
	return s.SyncAgents(ctx, specs)
}

// ---------------------------------------------------------------- serve

func serve(ctx context.Context, log *slog.Logger) error {
	token := os.Getenv("ORBIT_API_TOKEN")
	if len(token) < 32 {
		// It also signs approval links, so a short one is a forgeable one.
		return errors.New("ORBIT_API_TOKEN must be set, at least 32 characters (openssl rand -hex 32)")
	}
	workers, err := strconv.Atoi(env("ORBIT_WORKERS", "2"))
	if err != nil || workers < 1 {
		return fmt.Errorf("ORBIT_WORKERS: want a positive integer")
	}
	dir := configDir(nil)
	cfg, err := spec.Load(dir)
	if err != nil {
		return err
	}
	hostname, _ := os.Hostname()
	workerName := env("ORBIT_WORKER_NAME", hostname)

	return withStore(ctx, func(s *store.Store) error {
		if err := s.Migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
		if err := syncAgents(ctx, s, cfg); err != nil {
			return err
		}
		eng := &engine.Engine{
			Store: s, DataDir: env("ORBIT_DATA_DIR", "/data"), Env: os.LookupEnv,
			Client: &http.Client{Timeout: 2 * time.Minute}, Secret: []byte(token),
			NewRunner: runner.New, Log: log,
		}
		if err := eng.SetConfig(cfg); err != nil {
			return err
		}
		metrics.ConfigValid.Set(1)
		if err := seedMetrics(ctx, s); err != nil {
			return err
		}
		if n, err := s.FailOrphanedActions(ctx); err != nil {
			return err
		} else if n > 0 {
			log.Warn("failed orphaned actions", "count", n)
		}

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

		var wg sync.WaitGroup
		// runCtx outlives ctx by the drain window, so a run in progress at
		// SIGTERM can finish if it is close; anything still going after it
		// is requeued.
		runCtx, cancelRuns := context.WithCancel(context.WithoutCancel(ctx))
		defer cancelRuns()

		srv := &http.Server{
			Addr:              env("ORBIT_LISTEN", ":8080"),
			Handler:           (&api.Server{Store: s, Engine: eng, Token: token, Log: log, Wake: nudge}).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		go func() {
			if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("http", "err", err)
			}
		}()

		sched := &scheduler.Scheduler{
			Store: s, Log: log.With("component", "scheduler"), Reload: reload, Now: time.Now,
			Config: func() *spec.Config { return eng.Current().Config },
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			for ctx.Err() == nil {
				if err := sched.Run(ctx); err != nil {
					log.Error("scheduler", "err", err)
					sleep(ctx, 30*time.Second)
				}
			}
		}()

		for i := 0; i < workers; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				name := fmt.Sprintf("%s/%d", workerName, i)
				for ctx.Err() == nil {
					c, err := s.ClaimRun(ctx, name)
					if err != nil {
						if ctx.Err() == nil {
							log.Error("claim", "err", err)
						}
						sleep(ctx, 5*time.Second)
						continue
					}
					if c == nil {
						waitFor(ctx, wakeRuns, 5*time.Second)
						continue
					}
					if err := eng.Execute(runCtx, c); err != nil {
						log.Error("execute", "run", c.ID, "err", err)
					}
					// Its actions are ready the moment it finishes.
					if err := eng.ExecuteActions(ctx); err != nil && ctx.Err() == nil {
						log.Error("actions", "err", err)
					}
				}
			}(i)
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			t := time.NewTicker(30 * time.Second)
			defer t.Stop()
			last := configHash(dir)
			for {
				select {
				case <-ctx.Done():
					return
				case <-wakeActions:
				case <-t.C:
				}
				if err := eng.ExecuteActions(ctx); err != nil && ctx.Err() == nil {
					log.Error("actions", "err", err)
				}
				if failed, err := s.ReapStale(ctx, engine.StaleAfter); err != nil {
					log.Error("reap", "err", err)
				} else if len(failed) > 0 {
					log.Warn("failed runs whose worker was lost", "runs", failed)
				}
				refreshGauges(ctx, s, log)
				if h := configHash(dir); h != last {
					if err := reloadConfig(ctx, s, eng, dir); err != nil {
						metrics.ConfigValid.Set(0)
						log.Error("config reload failed; keeping the previous configuration", "err", err)
					} else {
						last = h
						metrics.ConfigValid.Set(1)
						log.Info("config reloaded")
						select {
						case reload <- struct{}{}:
						default:
						}
					}
				}
			}
		}()

		log.Info("orbit serving", "addr", srv.Addr, "workers", workers, "agents", len(cfg.Agents))
		<-ctx.Done()
		log.Info("shutting down")
		shutCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		_ = srv.Shutdown(shutCtx)
		cancel()
		// Runs in progress get the drain window, then are cancelled and
		// requeued by the engine.
		drained := make(chan struct{})
		go func() { wg.Wait(); close(drained) }()
		drain, _ := time.ParseDuration(env("ORBIT_DRAIN", "20s"))
		select {
		case <-drained:
		case <-time.After(drain):
			cancelRuns()
			<-drained
		}
		return nil
	})
}

func reloadConfig(ctx context.Context, s *store.Store, eng *engine.Engine, dir string) error {
	cfg, err := spec.Load(dir)
	if err != nil {
		return err
	}
	if err := syncAgents(ctx, s, cfg); err != nil {
		return err
	}
	return eng.SetConfig(cfg)
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
	rows, err := s.Pool.Query(ctx, `SELECT agent, max(finished_at) FROM runs WHERE status = 'succeeded' GROUP BY agent`)
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
		(SELECT count(*) FROM runs WHERE status = 'queued')`).Scan(&pending, &queued)
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
