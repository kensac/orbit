// orbitctl is a terminal client for Orbit's authenticated public API.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"

	"github.com/kensac/orbit/internal/client"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("orbitctl", flag.ContinueOnError)
	endpoint := fs.String("url", os.Getenv("ORBIT_URL"), "public Orbit HTTPS URL")
	issuer := fs.String("issuer", os.Getenv("ORBIT_OIDC_ISSUER"), "authentik device provider issuer")
	id := fs.String("client-id", os.Getenv("ORBIT_OIDC_CLIENT_ID"), "public device provider client ID")
	fs.Usage = func() {
		_, _ = fmt.Fprintln(fs.Output(), "Usage: orbitctl [flags] agents | conversations AGENT | thread CONVERSATION | send CONVERSATION TEXT | send-agent AGENT TEXT | stream RUN | approve ACTION | reject ACTION\nEach invocation signs in through authentik; tokens are never saved.")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		return err
	}
	a := fs.Args()
	if len(a) == 0 {
		fs.Usage()
		return errors.New("command required")
	}
	method, path, body, err := command(a)
	if err != nil {
		return err
	}
	if err := client.ValidateURL(*endpoint); err != nil {
		return err
	}
	hc := client.HTTPClient()
	token, err := client.Login(ctx, hc, *issuer, *id, os.Stderr)
	if err != nil {
		return err
	}
	c := client.Client{BaseURL: *endpoint, Token: token, HTTP: hc}
	if a[0] != "send" && a[0] != "send-agent" {
		return c.Request(ctx, method, path, strings.NewReader(body), os.Stdout)
	}
	var reply strings.Builder
	if err := c.Request(ctx, method, path, strings.NewReader(body), &reply); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, reply.String()); err != nil {
		return err
	}
	var posted struct {
		Run string `json:"run_id"`
	}
	if err := json.Unmarshal([]byte(reply.String()), &posted); err != nil {
		return err
	}
	if posted.Run != "" {
		run, err := client.Segment(posted.Run)
		if err != nil {
			return err
		}
		return c.Request(ctx, "GET", "runs/"+run+"/stream", nil, os.Stdout)
	}
	return nil
}

func command(a []string) (string, string, string, error) {
	if len(a) == 1 && a[0] == "agents" {
		return "GET", "agents", "", nil
	}
	if len(a) < 2 {
		return "", "", "", errors.New("command requires an identifier")
	}
	s, err := client.Segment(a[1])
	if err != nil {
		return "", "", "", err
	}
	if len(a) == 2 {
		switch a[0] {
		case "conversations":
			return "GET", "agents/" + s + "/conversations", "", nil
		case "thread":
			return "GET", "conversations/" + s + "/thread", "", nil
		case "stream":
			return "GET", "runs/" + s + "/stream", "", nil
		case "approve", "reject":
			return "POST", "actions/" + s + "/" + a[0], "{}", nil
		}
	}
	if len(a) == 3 && (a[0] == "send" || a[0] == "send-agent") {
		var b [16]byte
		if _, err := rand.Read(b[:]); err != nil {
			return "", "", "", err
		}
		r := client.Message(a[2], hex.EncodeToString(b[:]))
		var out strings.Builder
		_, err := io.Copy(&out, r)
		path := "conversations/" + s + "/messages"
		if a[0] == "send-agent" {
			path = "agents/" + s + "/messages"
		}
		return "POST", path, out.String(), err
	}
	return "", "", "", errors.New("invalid command or argument count")
}
