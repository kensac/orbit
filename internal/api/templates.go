package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
	"github.com/kensac/orbit/internal/templates"
)

// templatesRoutes are the templates library and creating agents from it (docs/api.md, Templates).
func (s *Server) templatesRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/v1/templates", s.listTemplates)
	api("POST /api/v1/templates", s.createTemplate)
	api("GET /api/v1/templates/{name}", s.getTemplate)
	api("PUT /api/v1/templates/{name}", s.putTemplate)
	api("GET /api/v1/templates/{name}/versions/{version}", s.getTemplateVersion)
	api("POST /api/v1/templates/{name}/restore", s.restoreTemplate)
	api("POST /api/v1/templates/{name}/retire", s.retireTemplate)
	api("POST /api/v1/templates/{name}/agents", s.agentFromTemplate)
	api("POST /api/v1/templates/share", s.shareTemplates)
	api("POST /api/v1/templates/import", s.importTemplates)
}

// templateBodyLimit bounds a request carrying a template: its own bound with room for the request around it.
const templateBodyLimit = templates.MaxBytes + 16<<10

// decodeTemplate reads a template document from a request body's field, strictly, and checks its name against the path's when there is one.
func decodeTemplate(raw json.RawMessage, path string) (*templates.Template, error) {
	if len(raw) == 0 {
		return nil, errors.New("template is required")
	}
	t, err := templates.Decode(raw)
	if err != nil {
		return nil, errors.New("template: " + err.Error())
	}
	if path != "" {
		if t.Name == "" {
			t.Name = path
		}
		if t.Name != path {
			return nil, errors.New("template names " + strconv.Quote(t.Name) + ", the path " + strconv.Quote(path))
		}
	}
	return t, nil
}

func (s *Server) listTemplates(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListTemplates(r.Context(), r.URL.Query().Get("status"))
	respond(w, map[string]any{"templates": orEmpty(list)}, err)
}

func (s *Server) createTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Template json.RawMessage `json:"template"`
		Note     string          `json:"note"`
		Author   string          `json:"author"`
	}
	if !decode(w, r, templateBodyLimit, &body, bodyRequired) {
		return
	}
	doc, err := decodeTemplate(body.Template, "")
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	k, _, err := s.Store.PutTemplate(r.Context(), store.TemplateWrite{Doc: doc, Create: true},
		store.TemplateProvenance{Source: store.TemplateByPerson, Author: actor(r, body.Author), Note: body.Note})
	respondStatus(w, http.StatusCreated, map[string]any{"template": k}, err)
}

func (s *Server) getTemplate(w http.ResponseWriter, r *http.Request) {
	k, err := s.Store.GetTemplate(r.Context(), r.PathValue("name"), 0, true)
	if err != nil {
		respond(w, nil, err)
		return
	}
	versions, err := s.Store.TemplateVersions(r.Context(), k.Name)
	respond(w, map[string]any{"template": k, "versions": orEmpty(versions)}, err)
}

func (s *Server) getTemplateVersion(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || v <= 0 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	k, err := s.Store.GetTemplate(r.Context(), r.PathValue("name"), v, true)
	respond(w, map[string]any{"template": k}, err)
}

func (s *Server) putTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Template json.RawMessage `json:"template"`
		Version  int             `json:"version"`
		Note     string          `json:"note"`
		Author   string          `json:"author"`
	}
	if !decode(w, r, templateBodyLimit, &body, bodyRequired) {
		return
	}
	doc, err := decodeTemplate(body.Template, r.PathValue("name"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if _, err := s.Store.GetTemplate(r.Context(), doc.Name, 0, false); err != nil {
		respond(w, nil, err)
		return
	}
	k, changed, err := s.Store.PutTemplate(r.Context(), store.TemplateWrite{Doc: doc, Expect: body.Version},
		store.TemplateProvenance{Source: store.TemplateByPerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"template": k, "changed": changed}, err)
}

func (s *Server) restoreTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Restore int    `json:"restore"`
		Version int    `json:"version"`
		Note    string `json:"note"`
		Author  string `json:"author"`
	}
	if !decode(w, r, 16<<10, &body, bodyRequired) {
		return
	}
	k, err := s.Store.RestoreTemplate(r.Context(), r.PathValue("name"), body.Restore, body.Version,
		store.TemplateProvenance{Source: store.TemplateByPerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"template": k}, err)
}

func (s *Server) retireTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version int    `json:"version"`
		Note    string `json:"note"`
		Author  string `json:"author"`
	}
	if !decode(w, r, 16<<10, &body, bodyOptional) {
		return
	}
	k, err := s.Store.RetireTemplate(r.Context(), r.PathValue("name"), body.Version,
		store.TemplateProvenance{Source: store.TemplateByPerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"template": k}, err)
}

// agentFromTemplate is POST /templates/{name}/agents: render, dry run, then (unless dry_run) define.
func (s *Server) agentFromTemplate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version        int                        `json:"version"`
		Name           string                     `json:"name"`
		Parameters     map[string]json.RawMessage `json:"parameters"`
		ConversationID string                     `json:"conversation_id"`
		DryRun         bool                       `json:"dry_run"`
	}
	if !decode(w, r, templateBodyLimit, &body, bodyRequired) {
		return
	}
	res, err := s.Engine.CreateFromTemplate(r.Context(), engine.TemplateRequest{Template: r.PathValue("name"), Version: body.Version,
		Name: body.Name, Values: body.Parameters, DryRun: body.DryRun, Author: actor(r, ""), ConversationID: body.ConversationID})
	var need *engine.NeedsValues
	if errors.As(err, &need) {
		respondStatus(w, http.StatusBadRequest, map[string]any{"error": need.Error(), "needs": need.Fields}, nil)
		return
	}
	var out *DefineResult
	if err == nil {
		out, err = s.defineResult(r.Context(), res.Defined, nil)
	}
	if err != nil {
		s.respondDefine(w, http.StatusCreated, nil, err)
		return
	}
	resp := map[string]any{"valid": out.Valid, "spec": out.Agent, "next_fires": orEmpty(out.NextFires), "widens": orEmpty(res.Widens),
		"template": map[string]any{"name": res.Template.Name, "version": res.Template.Version}}
	if out.Definition == nil {
		respond(w, resp, nil)
		return
	}
	resp["definition"], resp["agent"] = out.Definition, out.Row
	respondStatus(w, http.StatusCreated, resp, nil)
}
