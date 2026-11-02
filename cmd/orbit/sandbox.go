package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/kensac/orbit/internal/credproxy"
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

// credproxyCmd is `orbit credproxy`: the sandboxes' route to their backend, adding the credential their token seals (internal/credproxy).
func credproxyCmd(ctx context.Context, log *slog.Logger) error {
	sealer, err := credproxy.NewSealer(os.Getenv("ORBIT_CREDPROXY_KEY"))
	if err != nil {
		return err
	}
	return credproxy.New(sealer, log).Serve(ctx, env("ORBIT_CREDPROXY_LISTEN", ":3129"))
}
