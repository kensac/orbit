// orbitexport writes, verifies and inspects portable public-API exports.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"time"

	"github.com/kensac/orbit/internal/bundle"
	"github.com/kensac/orbit/internal/client"
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
	fs := flag.NewFlagSet("orbitexport", flag.ContinueOnError)
	fs.SetOutput(notice)
	endpoint := fs.String("url", os.Getenv("ORBIT_URL"), "public Orbit HTTPS URL")
	issuer := fs.String("issuer", os.Getenv("ORBIT_OIDC_ISSUER"), "authentik device provider issuer")
	timeout := fs.Duration("timeout", 10*time.Minute, "overall sign-in and export timeout")
	id := fs.String("client-id", os.Getenv("ORBIT_OIDC_CLIENT_ID"), "public device provider client ID")
	dryRun := fs.Bool("dry-run", false, "validate portable bundle imports without writing")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(notice, "Usage: orbitexport [flags] export AGENT NEW_OUTPUT.zip\n       orbitexport [flags] import BUNDLE.zip TARGET_AGENT\n       orbitexport verify OUTPUT.zip\n       orbitexport inspect BUNDLE.zip")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	a := fs.Args()
	if len(a) == 2 && a[0] == "inspect" {
		m, err := bundle.Inspect(a[1])
		if err != nil {
			return err
		}
		// Match the verifier's 16 MiB manifest member bound even after JSON escaping.
		const maxInspectionJSONBytes = 16 << 20
		data, err := json.Marshal(m)
		if err != nil {
			return err
		}
		if len(data)+1 > maxInspectionJSONBytes {
			return errors.New("inspection JSON exceeds 16 MiB output bound")
		}
		data = append(data, '\n')
		_, err = out.Write(data)
		return err
	}
	if len(a) == 2 && a[0] == "verify" {
		if err := bundle.Verify(a[1]); err != nil {
			return err
		}
		_, err := fmt.Fprintln(out, "Bundle checksums verified. This does not prove completeness or restorability.")
		return err
	}
	if len(a) == 3 && a[0] == "import" {
		if _, err := bundle.Inspect(a[1]); err != nil {
			return err
		}
		if _, err := client.Segment(a[2]); err != nil {
			return err
		}
		if err := validateConnection(*endpoint, *timeout); err != nil {
			return err
		}
		ctx, cancel := context.WithTimeout(ctx, *timeout)
		defer cancel()
		hc := client.HTTPClient()
		token, err := client.Login(ctx, hc, *issuer, *id, notice)
		if err != nil {
			return err
		}
		api := client.Client{BaseURL: *endpoint, Token: token, HTTP: hc}
		result, err := bundle.Import(ctx, &api, a[1], a[2], *dryRun)
		if result.Agent != "" {
			if writeErr := json.NewEncoder(out).Encode(result); writeErr != nil {
				return writeErr
			}
		}
		return err
	}
	if len(a) != 3 || a[0] != "export" {
		fs.Usage()
		return errors.New("expected export AGENT NEW_OUTPUT.zip, import BUNDLE.zip TARGET_AGENT, verify OUTPUT.zip or inspect BUNDLE.zip")
	}
	if *dryRun {
		return errors.New("-dry-run is only valid with import")
	}
	if _, err := client.Segment(a[1]); err != nil {
		return err
	}
	if err := validateConnection(*endpoint, *timeout); err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	hc := client.HTTPClient()
	token, err := client.Login(ctx, hc, *issuer, *id, notice)
	if err != nil {
		return err
	}
	c := client.Client{BaseURL: *endpoint, Token: token, HTTP: hc}
	m, err := bundle.Export(ctx, &c, a[1], a[2])
	if err != nil && !errors.Is(err, bundle.ErrIncomplete) {
		return err
	}
	if _, writeErr := fmt.Fprintf(out, "Exported %d resource records to %s; consistent snapshot: false; database backup: false.\n", len(m.Resources), a[2]); writeErr != nil {
		return writeErr
	}
	return err
}

func validateConnection(endpoint string, timeout time.Duration) error {
	if err := client.ValidateURL(endpoint); err != nil {
		return err
	}
	if timeout <= 0 {
		return errors.New("timeout must be positive")
	}
	return nil
}
