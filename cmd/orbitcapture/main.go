// orbitcapture saves selected model-visible source inputs through the public API.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"time"

	"github.com/kensac/orbit/internal/client"
	"github.com/kensac/orbit/internal/inputcapture"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:], os.Stdout, os.Stderr); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out, notice io.Writer) error {
	fs := flag.NewFlagSet("orbitcapture", flag.ContinueOnError)
	fs.SetOutput(notice)
	endpoint := fs.String("url", os.Getenv("ORBIT_URL"), "public Orbit HTTPS URL")
	issuer := fs.String("issuer", os.Getenv("ORBIT_OIDC_ISSUER"), "device provider issuer")
	id := fs.String("client-id", os.Getenv("ORBIT_OIDC_CLIENT_ID"), "public device provider client ID")
	timeout := fs.Duration("timeout", 5*time.Minute, "overall sign-in and capture timeout (at most 10 minutes)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	a := fs.Args()
	if len(a) != 4 || a[0] != "capture" {
		return errors.New("usage: orbitcapture [flags] capture RUN ATTEMPT NEW_DIRECTORY")
	}
	attempt, err := strconv.Atoi(a[2])
	if err != nil || attempt < 1 {
		return errors.New("attempt must be positive")
	}
	if _, err = client.Segment(a[1]); err != nil {
		return err
	}
	if err = client.ValidateURL(*endpoint); err != nil {
		return err
	}
	if *timeout <= 0 || *timeout > 10*time.Minute {
		return errors.New("timeout must be positive and at most 10 minutes")
	}
	if _, err = os.Lstat(a[3]); !errors.Is(err, os.ErrNotExist) {
		return errors.New("output must be a new directory")
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	hc := client.HTTPClient()
	token, err := client.Login(ctx, hc, *issuer, *id, notice)
	if err != nil {
		return err
	}
	c := client.Client{BaseURL: *endpoint, Token: token, HTTP: hc}
	m, err := inputcapture.Capture(ctx, &c, a[1], attempt, a[3])
	if m.Version != 0 {
		_, writeErr := fmt.Fprintf(out, "Captured %d verified inputs; complete: %t. Historical context and expectations are not captured.\n", len(m.Inputs), m.Complete)
		if writeErr != nil {
			return writeErr
		}
	}
	return err
}
