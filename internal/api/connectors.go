package api

import (
	"errors"
	"html"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/connectors"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// connectorInfo is one connector as GET /connectors shows it: its configuration (never a credential), which agents have it, and for an oauth one whether a person has signed it in.
type connectorInfo struct {
	Name         string                `json:"name"`
	Transport    string                `json:"transport"`
	URL          string                `json:"url"`
	Description  string                `json:"description"`
	Auth         string                `json:"auth"`
	WriteTools   []string              `json:"write_tools"`
	Agents       []string              `json:"agents"`
	Connected    bool                  `json:"connected"`
	Token        *store.ConnectorToken `json:"token"`
	CatalogAdded bool                  `json:"catalog_added,omitempty"`
	DefaultRule  string                `json:"default_tool_rule,omitempty"`
}

func (s *Server) connectorRoutes(api func(string, http.HandlerFunc), mux *http.ServeMux) {
	api("GET /api/v1/connectors", s.listConnectors)
	api("GET /api/v1/connectors/catalog", s.listConnectorCatalog)
	api("POST /api/v1/connectors/catalog", s.addConnectorCatalog)
	api("POST /api/v1/connectors/catalog/{name}/revoke", s.revokeConnectorGrant)
	api("GET /api/v1/connectors/{name}/tools", s.connectorTools)
	api("POST /api/v1/connectors/{name}/authorize", s.authorizeConnector)
	// The sealed state is the credential: the authorization server sends the person's browser here.
	mux.HandleFunc("GET "+engine.ConnectorCallbackPath, s.connectorCallback)
}

// revokeConnectorGrant revokes the current OAuth grant without removing the
// catalog record or any sealed token history.
func (s *Server) revokeConnectorGrant(w http.ResponseWriter, r *http.Request) {
	person := principalOf(r).Person
	if person == "" {
		writeErr(w, http.StatusForbidden, "revoking a connector grant requires a signed-in person")
		return
	}
	name := r.PathValue("name")
	entry, err := s.Store.CatalogConnector(r.Context(), name)
	if err != nil {
		respond(w, nil, err)
		return
	}
	if entry == nil {
		writeErr(w, http.StatusNotFound, "catalog connector not found")
		return
	}
	if entry.CreatedBy != person {
		writeErr(w, http.StatusForbidden, "only the person who added this connector can revoke its grant")
		return
	}
	version, err := s.Store.RevokeConnectorToken(r.Context(), name, person)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusConflict, "connector has no active grant")
		return
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	respond(w, map[string]any{"connector": name, "revoked_version": version}, nil)
}

func (s *Server) listConnectors(w http.ResponseWriter, r *http.Request) {
	cfg := s.Engine.Current().Config
	tokens, err := s.Store.ConnectorTokens(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	names := make([]string, 0, len(cfg.Deployment.Connectors))
	for n := range cfg.Deployment.Connectors {
		names = append(names, n)
	}
	sort.Strings(names)
	out := make([]connectorInfo, 0, len(names))
	for _, n := range names {
		c := cfg.Deployment.Connectors[n]
		info := connectorInfo{Name: n, Transport: spec.ConnectorTransportHTTP, URL: c.URL, Description: c.Description,
			Auth: c.AuthKind(), WriteTools: orEmpty(c.WriteTools), Agents: []string{}, CatalogAdded: c.CatalogManaged,
			DefaultRule: string(c.DefaultToolRule)}
		for name, a := range cfg.Agents {
			if slices.Contains(a.Connectors, n) {
				info.Agents = append(info.Agents, name)
			}
		}
		sort.Strings(info.Agents)
		if t, ok := tokens[n]; ok && c.AuthKind() == spec.ConnectorAuthOAuth {
			info.Token, info.Connected = &t, true
		}
		if c.AuthKind() != spec.ConnectorAuthOAuth {
			info.Connected = true
		}
		out = append(out, info)
	}
	respond(w, map[string]any{"connectors": out}, nil)
}

func (s *Server) listConnectorCatalog(w http.ResponseWriter, r *http.Request) {
	rows, err := s.Store.CatalogConnectors(r.Context())
	respond(w, map[string]any{"connectors": rows}, err)
}

type addConnectorCatalogRequest struct {
	Name        string   `json:"name"`
	URL         string   `json:"url"`
	Description string   `json:"description"`
	Scopes      []string `json:"scopes"`
}

type addConnectorCatalogResponse struct {
	Connector     store.CatalogConnector `json:"connector"`
	AuthorizeURL  string                 `json:"authorize_url,omitempty"`
	SignInError   string                 `json:"sign_in_error,omitempty"`
	PendingReload string                 `json:"pending_reload,omitempty"`
}

// addConnectorCatalog discovers an OAuth-protected MCP server, then records
// its public endpoint without storing credentials. Every offered tool asks
// for approval until an agent names a narrower rule.
func (s *Server) addConnectorCatalog(w http.ResponseWriter, r *http.Request) {
	var body addConnectorCatalogRequest
	if !decode(w, r, 16<<10, &body, bodyRequired) {
		return
	}
	person := principalOf(r).Person
	if person == "" {
		writeErr(w, http.StatusForbidden, "adding a connector requires a signed-in person")
		return
	}
	if !spec.ConnectorNameRE.MatchString(body.Name) || slices.Contains(spec.ReservedConnectorNames, body.Name) {
		writeErr(w, http.StatusBadRequest, "name must be a non-reserved connector name")
		return
	}
	if len(body.Description) > 500 || strings.ContainsAny(body.Description, "\r\n\x00") {
		writeErr(w, http.StatusBadRequest, "description must be at most 500 bytes without line breaks")
		return
	}
	if err := validateCatalogScopes(body.Scopes); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, exists := s.Engine.Current().Config.Deployment.Connectors[body.Name]; exists {
		writeErr(w, http.StatusConflict, "a connector with that name is already configured")
		return
	}
	if err := validateCatalogURL(body.URL); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	hc := connectors.PublicClient(&http.Client{Timeout: 15 * time.Second})
	if s.LinkClient != nil {
		hc = s.LinkClient
	}
	md, err := connectors.Discover(r.Context(), hc, body.URL)
	if err != nil {
		writeErr(w, http.StatusBadGateway, "OAuth discovery failed: "+err.Error())
		return
	}
	if err := validateCatalogMetadata(md); err != nil {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	if md.Registration == "" {
		writeErr(w, http.StatusBadRequest, "the authorization server does not support dynamic client registration")
		return
	}
	scopes := body.Scopes
	if len(scopes) == 0 {
		scopes = md.Scopes
	}
	if err := validateCatalogScopes(scopes); err != nil {
		writeErr(w, http.StatusBadGateway, "OAuth metadata has invalid scopes: "+err.Error())
		return
	}
	entry := store.CatalogConnector{Name: body.Name, URL: body.URL, Description: body.Description, Scopes: scopes, CreatedBy: person}
	created, err := s.Store.AddCatalogConnector(r.Context(), entry)
	if err != nil {
		if errors.Is(err, store.ErrConflict) {
			writeErr(w, http.StatusConflict, err.Error())
		} else {
			respond(w, nil, err)
		}
		return
	}
	entry = *created
	if s.Engine.Reloader != nil {
		err = s.Engine.Reloader(r.Context())
	} else {
		err = s.Engine.Reload(r.Context())
	}
	out := addConnectorCatalogResponse{Connector: entry}
	if err != nil {
		out.PendingReload = "connector stored; this instance will retry the config reload: " + err.Error()
	} else if u, authErr := s.Engine.ConnectorAuthorize(r.Context(), body.Name, person); authErr != nil {
		out.SignInError = authErr.Error()
	} else {
		out.AuthorizeURL = u
	}
	respondStatus(w, http.StatusCreated, out, nil)
}

func validateCatalogURL(raw string) error {
	if len(raw) > 2048 {
		return errors.New("url is over 2048 bytes")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || u.String() != raw {
		return errors.New("url must be a canonical HTTPS MCP endpoint without userinfo, query or fragment")
	}
	return nil
}

func validateCatalogMetadata(md *connectors.Metadata) error {
	for label, raw := range map[string]string{"issuer": md.Issuer, "resource": md.Resource, "authorization endpoint": md.Authorization,
		"token endpoint": md.Token, "registration endpoint": md.Registration} {
		if raw == "" && label == "registration endpoint" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.Fragment != "" {
			return errors.New("OAuth metadata has an invalid public HTTPS " + label)
		}
	}
	return nil
}

func validateCatalogScopes(scopes []string) error {
	if len(scopes) > 32 {
		return errors.New("scopes must contain at most 32 values")
	}
	seen := map[string]bool{}
	for _, scope := range scopes {
		if scope == "" || len(scope) > 128 || strings.ContainsAny(scope, " \t\r\n\x00\"\\") {
			return errors.New("each OAuth scope must be 1 to 128 printable non-space characters")
		}
		if seen[scope] {
			return errors.New("OAuth scopes must be unique")
		}
		seen[scope] = true
	}
	return nil
}

func (s *Server) connectorTools(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	tools, err := s.Engine.ConnectorTools(r.Context(), name)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusBadGateway, err.Error())
		return
	}
	type tool struct {
		connectors.Tool
		Write bool `json:"write"`
	}
	list := []tool{}
	c := s.Engine.Current().Config.Deployment.Connectors[name]
	for _, t := range tools {
		list = append(list, tool{Tool: t, Write: slices.Contains(c.WriteTools, t.Name)})
	}
	respond(w, map[string]any{"tools": list}, err)
}

func (s *Server) authorizeConnector(w http.ResponseWriter, r *http.Request) {
	u, err := s.Engine.ConnectorAuthorize(r.Context(), r.PathValue("name"), actorOrOperator(r))
	switch {
	case errors.Is(err, engine.ErrNotOAuth):
		writeErr(w, http.StatusConflict, err.Error())
	case err != nil && !errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusBadGateway, err.Error())
	default:
		respond(w, map[string]any{"authorize_url": u}, err)
	}
}

// connectorCallback finishes a sign-in and shows the person a page saying how it went.
func (s *Server) connectorCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page := func(status int, title, body string) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><meta name="viewport" content="width=device-width"><title>` +
			html.EscapeString(title) + `</title><body style="font-family:system-ui;margin:2rem"><h1>` + html.EscapeString(title) +
			`</h1><p>` + html.EscapeString(body) + `</p></body>`))
	}
	if e := q.Get("error"); e != "" {
		page(http.StatusBadRequest, "Not connected", "The sign-in was refused: "+e+" "+q.Get("error_description"))
		return
	}
	name, err := s.Engine.ConnectorCallback(r.Context(), q.Get("state"), q.Get("code"))
	if err != nil {
		s.Log.Warn("connector sign-in", "connector", name, "err", err)
		page(http.StatusBadRequest, "Not connected", err.Error())
		return
	}
	page(http.StatusOK, "Connected", "orbit is signed in to "+name+". You can close this page.")
}
