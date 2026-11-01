// Package github is orbit's only access to GitHub, as one GitHub App. The
// App's private key signs a short JWT; the JWT is exchanged for an
// installation token limited to one repository and one named permission set,
// minted per operation and revoked after it. The harness holds every token;
// no model is ever given one (docs/tickets.md).
package github

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/spec"
)

// DefaultAPIURL is github.com's REST API.
const DefaultAPIURL = "https://api.github.com"

// apiVersion pins the REST API version every request asks for, so a GitHub
// default change cannot alter a response shape orbit parses.
const apiVersion = "2022-11-28"

// Permissions is an installation token's permission set, as GitHub names
// them ("contents": "read").
type Permissions map[string]string

// The permission sets orbit mints, one per use. None includes workflows, so
// GitHub itself refuses a push that changes .github/workflows.
var (
	// ReadContents clones a repository for an agent to read.
	ReadContents = Permissions{"contents": "read"}
	// Deliver pushes orbit/* branches and opens or closes their PRs.
	Deliver = Permissions{"contents": "write", "pull_requests": "write"}
	// MergePermissions reads a PR and its checks, rewrites its body and
	// merges it.
	MergePermissions = Permissions{"contents": "write", "pull_requests": "write", "checks": "read", "statuses": "read"}
)

// App mints installation tokens for one GitHub App.
type App struct {
	ID      int64
	Key     *rsa.PrivateKey
	BaseURL string
	HTTP    *http.Client
	// Now is time.Now, replaceable in tests.
	Now func() time.Time
}

// NewApp builds the App a deployment configures, reading its key from the
// environment. The key never leaves this process.
func NewApp(g *spec.GitHub, env func(string) (string, bool), client *http.Client) (*App, error) {
	if g == nil {
		return nil, errors.New("github: no github block in orbit.yaml")
	}
	raw, ok := env(g.PrivateKeyEnv)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil, fmt.Errorf("github: %s is not set", g.PrivateKeyEnv)
	}
	key, err := ParsePrivateKey(raw)
	if err != nil {
		return nil, fmt.Errorf("github: %s: %w", g.PrivateKeyEnv, err)
	}
	base := g.APIURL
	if base == "" {
		base = DefaultAPIURL
	}
	return &App{ID: g.AppID, Key: key, BaseURL: strings.TrimRight(base, "/"), HTTP: client, Now: time.Now}, nil
}

// ParsePrivateKey reads an RSA key as GitHub issues it (PKCS#1 PEM), as
// PKCS#8 PEM, or as base64 of either PEM.
func ParsePrivateKey(s string) (*rsa.PrivateKey, error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "-----BEGIN") {
		dec, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, errors.New("neither a PEM key nor base64 of one")
		}
		s = string(dec)
	}
	block, _ := pem.Decode([]byte(s))
	if block == nil {
		return nil, errors.New("no PEM block")
	}
	if k, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return k, nil
	}
	k, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parse key: %w", err)
	}
	rk, ok := k.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("not an RSA key (GitHub App keys are RSA)")
	}
	return rk, nil
}

// jwtLifetime is under GitHub's ten-minute maximum for an App JWT, and
// jwtBackdate absorbs clock skew between orbit and GitHub (GitHub's own
// recommendation is 60 seconds).
const (
	jwtLifetime = 9 * time.Minute
	jwtBackdate = 60 * time.Second
)

// JWT returns a token that authenticates as the App itself. One algorithm,
// three claims: the standard library is enough, and there is no algorithm
// negotiation to get wrong.
func (a *App) JWT() (string, error) {
	now := a.Now()
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iat": now.Add(-jwtBackdate).Unix(),
		"exp": now.Add(jwtLifetime).Unix(),
		"iss": strconv.FormatInt(a.ID, 10),
	})
	signing := header + "." + base64.RawURLEncoding.EncodeToString(claims)
	sum := sha256.Sum256([]byte(signing))
	sig, err := rsa.SignPKCS1v15(rand.Reader, a.Key, crypto.SHA256, sum[:])
	if err != nil {
		return "", err
	}
	return signing + "." + base64.RawURLEncoding.EncodeToString(sig), nil
}

// Token is an installation token for one repository.
type Token struct {
	Value     string
	ExpiresAt time.Time
	Repo      string
	app       *App
}

// Token mints an installation token for repo ("owner/name") with exactly
// perms. GitHub refuses a permission the App was not granted, and the token
// reaches no other repository whatever the installation covers.
func (a *App) Token(ctx context.Context, repo string, perms Permissions) (*Token, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || !spec.RepoRE.MatchString(repo) {
		return nil, fmt.Errorf("github: %q is not owner/name", repo)
	}
	if len(perms) == 0 {
		// An empty set would mean "everything the installation has".
		return nil, errors.New("github: a token needs an explicit permission set")
	}
	jwt, err := a.JWT()
	if err != nil {
		return nil, err
	}
	app := &Client{base: a.BaseURL, auth: "Bearer " + jwt, http: a.HTTP}
	var inst struct {
		ID int64 `json:"id"`
	}
	if err := app.do(ctx, http.MethodGet, "/repos/"+url.PathEscape(owner)+"/"+url.PathEscape(name)+"/installation", nil, &inst); err != nil {
		return nil, fmt.Errorf("github: find the installation for %s: %w", repo, err)
	}
	var tok struct {
		Token     string      `json:"token"`
		ExpiresAt time.Time   `json:"expires_at"`
		Perms     Permissions `json:"permissions"`
	}
	body := map[string]any{"repositories": []string{name}, "permissions": perms}
	if err := app.do(ctx, http.MethodPost, fmt.Sprintf("/app/installations/%d/access_tokens", inst.ID), body, &tok); err != nil {
		return nil, fmt.Errorf("github: mint a token for %s: %w", repo, err)
	}
	if tok.Token == "" {
		return nil, fmt.Errorf("github: mint a token for %s: empty token", repo)
	}
	// GitHub answers with the permissions it granted; anything beyond the
	// request means the down-scoping did not happen.
	for k, v := range tok.Perms {
		if k != "metadata" && perms[k] != v {
			t := &Token{Value: tok.Token, Repo: repo, app: a}
			_ = t.Revoke(context.WithoutCancel(ctx))
			return nil, fmt.Errorf("github: token for %s came back with %s=%s, which was not asked for", repo, k, v)
		}
	}
	return &Token{Value: tok.Token, ExpiresAt: tok.ExpiresAt, Repo: repo, app: a}, nil
}

// Client returns an API client acting with this token.
func (t *Token) Client() *Client {
	return &Client{base: t.app.BaseURL, auth: "token " + t.Value, http: t.app.HTTP}
}

// Revoke ends the token before it expires.
func (t *Token) Revoke(ctx context.Context) error {
	return t.Client().do(ctx, http.MethodDelete, "/installation/token", nil, nil)
}

// GitAuthEnv is the environment that makes git authenticate to github.com
// with this token without writing it anywhere: not in .git/config, not in a
// remote URL, not in a file a sandbox could be handed.
func (t *Token) GitAuthEnv() []string {
	basic := base64.StdEncoding.EncodeToString([]byte("x-access-token:" + t.Value))
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.https://github.com/.extraheader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// Client calls the REST API with one credential.
type Client struct {
	base string
	auth string
	http *http.Client
}

// APIError is a non-2xx answer from GitHub.
type APIError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("%s %s: %d %s", e.Method, e.Path, e.Status, e.Message)
}

// IsStatus reports whether err is an APIError with that status.
func IsStatus(err error, status int) bool {
	var ae *APIError
	return errors.As(err, &ae) && ae.Status == status
}

// maxResponse bounds one API response; the largest orbit reads is a page of
// 100 check runs, well under 1 MB.
const maxResponse = 4 << 20

func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", c.auth)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", apiVersion)
	req.Header.Set("User-Agent", "orbit")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var m struct {
			Message string `json:"message"`
		}
		_ = json.Unmarshal(b, &m)
		if m.Message == "" {
			m.Message = http.StatusText(resp.StatusCode)
		}
		return &APIError{Method: method, Path: path, Status: resp.StatusCode, Message: m.Message}
	}
	if out == nil || len(b) == 0 {
		return nil
	}
	if err := json.Unmarshal(b, out); err != nil {
		return fmt.Errorf("%s %s: decode: %w", method, path, err)
	}
	return nil
}
