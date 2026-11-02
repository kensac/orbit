package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/kensac/orbit/internal/egress"
	"github.com/kensac/orbit/internal/sandboxd"
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
