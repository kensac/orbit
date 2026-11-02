package api

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/kensac/orbit/internal/egress"
	"github.com/kensac/orbit/internal/secret"
	"github.com/kensac/orbit/internal/share"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/templates"
)

// Sharing templates (docs/api.md, Sharing templates): a bundle as JSON, or a signed link anyone holding it can fetch the bundle from, and importing either. An import is lowered by code (share.Read) so it grants nothing.

// Link lifetimes: a day unless asked, a week at most, so a link pasted somewhere stops working on its own.
const (
	DefaultLinkTTL = 24 * time.Hour
	MaxLinkTTL     = 7 * 24 * time.Hour
)

// linkFetchTimeout bounds importing from another orbit's link: one small GET.
const linkFetchTimeout = 10 * time.Second

// sharePath is where a signed link is served.
const sharePath = "/api/v1/templates/shared/"

// linkClaims is what a link names: exact template versions, and when it stops working.
type linkClaims struct {
	Templates []templateRef `json:"t"`
	Expires   int64         `json:"e"`
}

type templateRef struct {
	Name    string `json:"name"`
	Version int    `json:"version,omitempty"`
}

func (s *Server) linkSig(payload string) string {
	m := hmac.New(sha256.New, secret.Key(s.Token).Derive(secret.TemplateLink))
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))
}

// bundleOf reads the named versions (0: the current, which must be active) and bundles them.
func (s *Server) bundleOf(ctx context.Context, refs []templateRef) (*share.Bundle, []templateRef, error) {
	if len(refs) == 0 || len(refs) > share.MaxTemplates {
		return nil, nil, requestError(fmt.Sprintf("templates: 1 to %d", share.MaxTemplates))
	}
	var docs []*templates.Template
	var exact []templateRef
	for _, r := range refs {
		k, err := s.Store.GetTemplate(ctx, r.Name, r.Version, true)
		if err != nil {
			return nil, nil, fmt.Errorf("template %s: %w", r.Name, err)
		}
		if r.Version == 0 && k.Status != "active" {
			return nil, nil, requestError("template " + r.Name + " is retired; name a version to share it")
		}
		docs = append(docs, k.Doc())
		exact = append(exact, templateRef{Name: k.Name, Version: k.Version})
	}
	b, err := share.New(docs)
	return b, exact, err
}

// shareTemplates is POST /templates/share: the bundle, and with link a signed URL for it.
func (s *Server) shareTemplates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Templates      []templateRef `json:"templates"`
		Link           bool          `json:"link"`
		ExpiresInHours int           `json:"expires_in_hours"`
	}
	if !decode(w, r, 64<<10, &body, bodyRequired) {
		return
	}
	b, exact, err := s.bundleOf(r.Context(), body.Templates)
	if err != nil {
		respond(w, nil, err)
		return
	}
	out := map[string]any{"bundle": b}
	if body.Link {
		base := strings.TrimRight(s.Engine.Current().Config.Deployment.PublicURL, "/")
		if base == "" || len(s.Token) == 0 {
			writeErr(w, http.StatusServiceUnavailable, "template links need public_url in orbit.yaml")
			return
		}
		ttl := DefaultLinkTTL
		if body.ExpiresInHours != 0 {
			ttl = time.Duration(body.ExpiresInHours) * time.Hour
		}
		if ttl <= 0 || ttl > MaxLinkTTL {
			writeErr(w, http.StatusBadRequest, fmt.Sprintf("expires_in_hours: 1 to %d", int(MaxLinkTTL/time.Hour)))
			return
		}
		exp := time.Now().Add(ttl).Truncate(time.Second)
		claims, _ := json.Marshal(linkClaims{Templates: exact, Expires: exp.Unix()})
		payload := base64.RawURLEncoding.EncodeToString(claims)
		out["link"] = base + sharePath + payload + "." + s.linkSig(payload)
		out["expires_at"] = exp.UTC()
	}
	respond(w, out, nil)
}

// sharedTemplates is GET /templates/shared/{token}: the bundle a signed link names. The signature is the credential, as for signed uploads; the link names exact versions, so it always serves the same bundle.
func (s *Server) sharedTemplates(w http.ResponseWriter, r *http.Request) {
	payload, sig, ok := strings.Cut(r.PathValue("token"), ".")
	if !ok || len(s.Token) == 0 || !hmac.Equal([]byte(s.linkSig(payload)), []byte(sig)) {
		writeErr(w, http.StatusForbidden, "the template link is not valid")
		return
	}
	raw, err := base64.RawURLEncoding.DecodeString(payload)
	var c linkClaims
	if err != nil || json.Unmarshal(raw, &c) != nil {
		writeErr(w, http.StatusForbidden, "the template link is not valid")
		return
	}
	if time.Now().Unix() > c.Expires {
		writeErr(w, http.StatusForbidden, "the template link has expired")
		return
	}
	b, _, err := s.bundleOf(r.Context(), c.Templates)
	respond(w, b, err)
}

// importTemplates is POST /templates/import: a bundle given as JSON, or fetched from a link.
func (s *Server) importTemplates(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Bundle json.RawMessage `json:"bundle"`
		Link   string          `json:"link"`
		DryRun bool            `json:"dry_run"`
		Note   string          `json:"note"`
		Author string          `json:"author"`
	}
	if !decode(w, r, share.MaxBytes+16<<10, &body, bodyRequired) {
		return
	}
	raw := []byte(body.Bundle)
	switch {
	case len(raw) > 0 && body.Link != "":
		writeErr(w, http.StatusBadRequest, "give bundle or link, not both")
		return
	case body.Link != "":
		var err error
		if raw, err = fetchBundle(r.Context(), s.LinkClient, body.Link); err != nil {
			writeErr(w, http.StatusBadRequest, "link: "+err.Error())
			return
		}
	case len(raw) == 0:
		writeErr(w, http.StatusBadRequest, "bundle or link is required")
		return
	}
	lowered, err := share.Read(raw)
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	docs := make([]*templates.Template, len(lowered))
	for i, l := range lowered {
		docs[i] = l.Template
	}
	results, err := s.Store.ImportTemplates(r.Context(), docs, store.TemplateProvenance{Source: store.TemplateByImport,
		Author: actor(r, body.Author), Note: body.Note}, body.DryRun)
	if err != nil {
		respond(w, nil, err)
		return
	}
	type result struct {
		store.TemplateImportResult
		Lowered []string `json:"lowered"`
	}
	out := make([]result, len(results))
	for i, res := range results {
		out[i] = result{res, orEmpty(lowered[i].Changes)}
	}
	respond(w, map[string]any{"dry_run": body.DryRun, "results": out}, nil)
}

// fetchBundle GETs a bundle from another orbit's link: https only, a path under sharePath, no credential, no redirect, a public address only (checked on the connection itself, so a name that resolves to a private or tailnet address is refused, DNS rebinding included), within linkFetchTimeout and share.MaxBytes.
func fetchBundle(ctx context.Context, client *http.Client, link string) ([]byte, error) {
	u, err := url.Parse(link)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || !strings.HasPrefix(u.Path, sharePath) {
		return nil, errors.New("want an https link to another orbit's " + sharePath + "<token>")
	}
	if client == nil {
		client = guardedClient()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, share.MaxBytes+1))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(b, &e)
		return nil, fmt.Errorf("%s: %s", resp.Status, e.Error)
	}
	if len(b) > share.MaxBytes {
		return nil, fmt.Errorf("the bundle is over %d bytes", share.MaxBytes)
	}
	return bytes.TrimSpace(b), nil
}

// guardedClient connects only to public addresses, checked on the connection itself, and follows no redirect.
func guardedClient() *http.Client {
	dialer := &net.Dialer{Timeout: linkFetchTimeout, Control: func(_, address string, _ syscall.RawConn) error {
		host, _, err := net.SplitHostPort(address)
		if err != nil {
			return err
		}
		a, err := netip.ParseAddr(host)
		if err != nil || !egress.Public(a) {
			return fmt.Errorf("%s is not a public address", host)
		}
		return nil
	}}
	return &http.Client{
		Timeout:       linkFetchTimeout,
		Transport:     &http.Transport{DialContext: dialer.DialContext, Proxy: nil, TLSHandshakeTimeout: linkFetchTimeout},
		CheckRedirect: func(*http.Request, []*http.Request) error { return errors.New("a link does not redirect") },
	}
}
