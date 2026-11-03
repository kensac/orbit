package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/kensac/orbit/internal/credproxy"
	"github.com/kensac/orbit/internal/egress"
	"github.com/kensac/orbit/internal/metrics"
	"github.com/kensac/orbit/internal/sandbox"
	"github.com/kensac/orbit/internal/sandboxd"
	"github.com/kensac/orbit/internal/spec"
)

// sandboxdCmd is `orbit sandboxd`: the build sandbox service. It is the only
// orbit process that holds a docker socket (internal/sandboxd).
func sandboxdCmd(ctx context.Context, log *slog.Logger) error {
	cfg, err := sandboxd.ConfigFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	return sandboxd.New(cfg, log).Run(ctx)
}

// egressCmd is `orbit egress`: the proxy that is a sandbox's only route out, with a build's allowlist or a workstation's open web (internal/egress).
func egressCmd(ctx context.Context, log *slog.Logger) error {
	pol, err := egress.PolicyFromEnv(os.Getenv)
	if err != nil {
		return err
	}
	return egress.NewPolicy(pol, log).Serve(ctx, env("ORBIT_EGRESS_LISTEN", ":3128"))
}

// credproxyCmd is `orbit credproxy`: the sandboxes' route to their backend, adding the credential their token seals (internal/credproxy).
func credproxyCmd(ctx context.Context, log *slog.Logger) error {
	sealer, err := credproxy.NewSealer(os.Getenv("ORBIT_CREDPROXY_KEY"))
	if err != nil {
		return err
	}
	return credproxy.New(sealer, log).Serve(ctx, env("ORBIT_CREDPROXY_LISTEN", ":3129"))
}

// sandboxProbeEvery is how often control asks sandboxd's /readyz for orbit_sandboxd_ready and orbit_egress_ready.
const sandboxProbeEvery = 30 * time.Second

// probeSandboxd asks the configured sandboxd whether it is ready. addr "" means none is configured; an error means sandboxd did not answer.
func probeSandboxd(ctx context.Context, addr, token string) (sandbox.Readiness, error) {
	c, err := sandbox.NewClient(addr, token)
	if err != nil {
		return sandbox.Readiness{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	return c.Ready(ctx)
}

// watchSandboxd keeps the sandbox gauges current on control: absent without a sandboxd, sandboxd's readiness and each egress proxy's as sandboxd reports them otherwise.
func watchSandboxd(ctx context.Context, cfg func() *spec.Config, log *slog.Logger) {
	t := time.NewTicker(sandboxProbeEvery)
	defer t.Stop()
	wasReady := true
	for {
		addr, tokenEnv := cfg().Deployment.SandboxService()
		if addr == "" {
			metrics.ClearSandbox()
		} else {
			rd, err := probeSandboxd(ctx, addr, os.Getenv(tokenEnv))
			switch {
			case err != nil:
				metrics.SetSandbox(false, nil)
			default:
				metrics.SetSandbox(rd.Ready, rd.Proxies())
			}
			ready := err == nil && rd.Ready
			if ready != wasReady && ctx.Err() == nil {
				if ready {
					log.Info("sandboxd ready", "sandboxd", addr)
				} else {
					log.Warn("sandboxd not ready", "sandboxd", addr, "err", err, "checks", rd.Checks)
				}
			}
			wasReady = ready
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
