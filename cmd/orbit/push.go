package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/kensac/orbit/internal/push"
	"github.com/kensac/orbit/internal/webpush"
)

// deliverPush sends what needs attention to every device, when push is
// configured. Control only: it holds the VAPID key.
func deliverPush(ctx context.Context, p *push.Sender, log *slog.Logger) {
	if p == nil {
		return
	}
	if err := p.Deliver(ctx); err != nil && ctx.Err() == nil {
		log.Error("push", "err", err)
	}
}

// vapid prints a new VAPID key pair as the environment orbit reads.
func vapid() error {
	pub, priv, err := webpush.GenerateKeys()
	if err != nil {
		return err
	}
	fmt.Printf("ORBIT_VAPID_PUBLIC_KEY=%s\nORBIT_VAPID_PRIVATE_KEY=%s\n", pub, priv)
	return nil
}
