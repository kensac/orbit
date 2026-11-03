package api

import (
	"errors"
	"html"
	"net/http"
	"slices"
	"sort"

	"github.com/kensac/orbit/internal/connectors"
	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/spec"
	"github.com/kensac/orbit/internal/store"
)

// connectorInfo is one connector as GET /connectors shows it: its configuration (never a credential), which agents have it, and for an oauth one whether a person has signed it in.
type connectorInfo struct {
	Name        string                `json:"name"`
	Transport   string                `json:"transport"`
	URL         string                `json:"url"`
	Description string                `json:"description"`
	Auth        string                `json:"auth"`
	WriteTools  []string              `json:"write_tools"`
	Agents      []string              `json:"agents"`
	Connected   bool                  `json:"connected"`
	Token       *store.ConnectorToken `json:"token"`
}

func (s *Server) connectorRoutes(api func(string, http.HandlerFunc), mux *http.ServeMux) {
	api("GET /api/v1/connectors", s.listConnectors)
	api("GET /api/v1/connectors/{name}/tools", s.connectorTools)
	api("POST /api/v1/connectors/{name}/authorize", s.authorizeConnector)
	// The sealed state is the credential: the authorization server sends the person's browser here.
	mux.HandleFunc("GET "+engine.ConnectorCallbackPath, s.connectorCallback)
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
			Auth: c.AuthKind(), WriteTools: orEmpty(c.WriteTools), Agents: []string{}}
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
