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

// egressCmd is `orbit egress`: the allowlisting proxy that is a sandbox's
// only route out (internal/egress).
func egressCmd(ctx context.Context, log *slog.Logger) error {
	allow, err := egress.ParseAllowlist(os.Getenv("ORBIT_EGRESS_ALLOW"))
	if err != nil {
		return err
	}
	return egress.New(allow, log).Serve(ctx, env("ORBIT_EGRESS_LISTEN", ":3128"))
}
