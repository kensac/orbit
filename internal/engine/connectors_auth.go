package engine

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/connectors"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// A person signs an oauth connector in through orbit (docs/api.md, Connectors): orbit reads the server's authorization metadata, registers itself as a client unless one is configured, and returns the authorization URL; the server's authorization server sends the person back to orbit's callback, which exchanges the code and stores the token as the connector's next version. Everything the callback needs rides in the state, sealed under the connector key, so any instance finishes a sign-in another started.

// ConnectorCallbackPath is the callback's path under the public URL.
const ConnectorCallbackPath = "/api/v1/connectors/oauth/callback"

// connectorStateTTL bounds one sign-in.
const connectorStateTTL = 15 * time.Minute

const connectorStateKind = "connector-state"

type connectorState struct {
	Conn         string              `json:"conn"`
	Verifier     string              `json:"verifier"`
	ClientID     string              `json:"client_id"`
	ClientSecret string              `json:"client_secret,omitempty"`
	Metadata     connectors.Metadata `json:"metadata"`
	Redirect     string              `json:"redirect"`
	Person       string              `json:"person"`
}

// ErrNotOAuth is a sign-in asked for a connector that does not use oauth.
var ErrNotOAuth = errors.New("this connector does not sign in with oauth")

// ConnectorAuthorize starts person's sign-in to connector name and returns the URL to open.
func (e *Engine) ConnectorAuthorize(ctx context.Context, name, person string) (string, error) {
	d := e.Current().Config.Deployment
	cn, ok := d.Connectors[name]
	if !ok {
		return "", store.ErrNotFound
	}
	if cn.AuthKind() != spec.ConnectorAuthOAuth {
		return "", ErrNotOAuth
	}
	sealer, err := e.connectorSealer()
	if err != nil {
		return "", err
	}
	if d.PublicURL == "" {
		return "", errors.New("orbit.yaml has no public_url, so there is no callback to sign in to")
	}
	redirect := strings.TrimRight(d.PublicURL, "/") + ConnectorCallbackPath
	client := e.Client
	if cn.CatalogManaged {
		client = connectors.PublicClient(client)
	}
	md, err := connectors.Discover(ctx, client, cn.URL)
	if err != nil {
		return "", fmt.Errorf("connector %s: %w", name, err)
	}
	reg := connectors.Registration{ClientID: cn.Auth.ClientID}
	if reg.ClientID == "" {
		r, err := connectors.Register(ctx, client, md, redirect)
		if err != nil {
			return "", fmt.Errorf("connector %s: %w", name, err)
		}
		reg = *r
	}
	verifier, challenge := connectors.PKCE()
	state, err := sealer.SealString(connectorStateKind, connectorState{Conn: name, Verifier: verifier, ClientID: reg.ClientID,
		ClientSecret: reg.ClientSecret, Metadata: *md, Redirect: redirect, Person: person}, connectorStateTTL)
	if err != nil {
		return "", err
	}
	return connectors.AuthorizeURL(md, reg.ClientID, redirect, state, challenge, connectors.Scopes(cn, md)), nil
}

// ConnectorCallback finishes a sign-in: the code becomes the connector's next token version, recorded as the person's. It returns the connector's name.
func (e *Engine) ConnectorCallback(ctx context.Context, state, code string) (string, error) {
	sealer, err := e.connectorSealer()
	if err != nil {
		return "", err
	}
	var st connectorState
	if err := sealer.OpenString(connectorStateKind, state, &st); err != nil {
		return "", fmt.Errorf("the sign-in's state: %w", err)
	}
	cn, ok := e.Current().Config.Deployment.Connectors[st.Conn]
	if !ok || cn.AuthKind() != spec.ConnectorAuthOAuth {
		return "", fmt.Errorf("connector %q is no longer an oauth connector", st.Conn)
	}
	client := e.Client
	if cn.CatalogManaged {
		client = connectors.PublicClient(client)
	}
	secret := st.ClientSecret
	if cn.Auth.ClientSecretEnv != "" {
		secret, _ = e.Env(cn.Auth.ClientSecretEnv)
	}
	tok, err := connectors.Exchange(ctx, client, &st.Metadata, connectors.Registration{ClientID: st.ClientID, ClientSecret: secret}, code, st.Verifier, st.Redirect)
	if err != nil {
		return st.Conn, fmt.Errorf("connector %s: %w", st.Conn, err)
	}
	if cn.Auth.ClientSecretEnv == "" {
		// A registered client's secret is kept with its token; a configured one is read from its variable each time.
		tok.ClientSecret = st.ClientSecret
	}
	sealed, err := sealer.Seal(connectors.SealKind, tok, 0)
	if err != nil {
		return st.Conn, err
	}
	var exp *time.Time
	if !tok.ExpiresAt.IsZero() {
		exp = &tok.ExpiresAt
	}
	if _, err := e.Store.PutConnectorToken(ctx, st.Conn, sealed, exp, st.Person); err != nil {
		return st.Conn, err
	}
	return st.Conn, nil
}

// ConnectorTools lists connector name's tools as an agent with every default rule would see them, for a person choosing rules.
func (e *Engine) ConnectorTools(ctx context.Context, name string) ([]connectors.Tool, error) {
	cfg := e.Current().Config
	d := cfg.Deployment
	cn, ok := d.Connectors[name]
	if !ok {
		return nil, store.ErrNotFound
	}
	cl, err := e.connectorClient(d, cfg.ExtensionPackages, name, cn)
	if err != nil {
		return nil, err
	}
	defer cl.Close()
	lctx, cancel := context.WithTimeout(ctx, connectorListTimeout)
	defer cancel()
	return cl.ListTools(lctx)
}
