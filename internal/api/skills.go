package api

import (
	"net/http"
	"slices"
	"strconv"

	"github.com/kensac/orbit/internal/store"
)

// skillsRoutes are the skills library and agents' skills (docs/api.md, Skills).
func (s *Server) skillsRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/v1/skills", s.listSkills)
	api("POST /api/v1/skills", s.createSkill)
	api("POST /api/v1/skills/import", s.importSkills)
	api("GET /api/v1/skills/{name}", s.getSkill)
	api("PUT /api/v1/skills/{name}", s.putSkill)
	api("GET /api/v1/skills/{name}/versions/{version}", s.getSkillVersion)
	api("POST /api/v1/skills/{name}/restore", s.restoreSkill)
	api("POST /api/v1/skills/{name}/retire", s.retireSkill)
	api("PUT /api/v1/agents/{name}/skills/{skill}", s.attach(store.AttachSkill, "skill"))
	api("DELETE /api/v1/agents/{name}/skills/{skill}", s.detach(store.AttachSkill, "skill"))
}

// skillBodyLimit bounds a request carrying one skill: its bounds (store) at base64's four thirds, with room for JSON.
const skillBodyLimit = 24 << 20

// skillIn is a skill version in a request; files' content is base64, as JSON carries bytes.
type skillIn struct {
	Name    string            `json:"name"`
	Content string            `json:"content"`
	Files   []store.SkillFile `json:"files"`
}

// skillAgents maps each skill to the agents that run with it, from their spec or attached.
func (s *Server) skillAgents(r *http.Request) (map[string][]string, error) {
	out := map[string][]string{}
	for _, a := range s.Engine.Current().Config.Agents {
		names := append([]string{}, a.Skills...)
		attached, err := s.Store.AttachedNames(r.Context(), a.Name, store.AttachSkill)
		if err != nil {
			return nil, err
		}
		for _, n := range append(names, attached...) {
			if !slices.Contains(out[n], a.Name) {
				out[n] = append(out[n], a.Name)
			}
		}
	}
	for n := range out {
		slices.Sort(out[n])
	}
	return out, nil
}

type skillOut struct {
	*store.Skill
	Agents []string `json:"agents"`
}

func (s *Server) listSkills(w http.ResponseWriter, r *http.Request) {
	list, err := s.Store.ListSkills(r.Context(), r.URL.Query().Get("status"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	agents, err := s.skillAgents(r)
	out := make([]skillOut, 0, len(list))
	for i := range list {
		out = append(out, skillOut{&list[i], orEmpty(agents[list[i].Name])})
	}
	respond(w, map[string]any{"skills": out}, err)
}

func (s *Server) createSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		skillIn
		Note   string `json:"note"`
		Author string `json:"author"`
	}
	if !decode(w, r, skillBodyLimit, &body) {
		return
	}
	k, _, err := s.Store.PutSkill(r.Context(), store.SkillWrite{Name: body.Name, Content: body.Content, Files: body.Files, Create: true},
		store.SkillProvenance{Source: store.SkillByPerson, Author: actor(r, body.Author), Note: body.Note})
	respondStatus(w, http.StatusCreated, map[string]any{"skill": k}, err)
}

func (s *Server) getSkill(w http.ResponseWriter, r *http.Request) {
	k, err := s.Store.GetSkill(r.Context(), r.PathValue("name"), 0, true)
	if err != nil {
		respond(w, nil, err)
		return
	}
	versions, err := s.Store.SkillVersions(r.Context(), k.Name)
	if err != nil {
		respond(w, nil, err)
		return
	}
	agents, err := s.skillAgents(r)
	respond(w, map[string]any{"skill": skillOut{k, orEmpty(agents[k.Name])}, "versions": orEmpty(versions)}, err)
}

func (s *Server) getSkillVersion(w http.ResponseWriter, r *http.Request) {
	v, err := strconv.Atoi(r.PathValue("version"))
	if err != nil || v <= 0 {
		writeErr(w, http.StatusBadRequest, "version must be a positive integer")
		return
	}
	k, err := s.Store.GetSkill(r.Context(), r.PathValue("name"), v, true)
	respond(w, map[string]any{"skill": k}, err)
}

func (s *Server) putSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Content string            `json:"content"`
		Files   []store.SkillFile `json:"files"`
		Version int               `json:"version"`
		Note    string            `json:"note"`
		Author  string            `json:"author"`
	}
	if !decode(w, r, skillBodyLimit, &body) {
		return
	}
	k, changed, err := s.Store.PutSkill(r.Context(), store.SkillWrite{Name: r.PathValue("name"), Content: body.Content, Files: body.Files, Expect: body.Version},
		store.SkillProvenance{Source: store.SkillByPerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"skill": k, "changed": changed}, err)
}

func (s *Server) restoreSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Restore int    `json:"restore"`
		Version int    `json:"version"`
		Note    string `json:"note"`
		Author  string `json:"author"`
	}
	if !decode(w, r, 16<<10, &body) {
		return
	}
	k, err := s.Store.RestoreSkill(r.Context(), r.PathValue("name"), body.Restore, body.Version,
		store.SkillProvenance{Source: store.SkillByPerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"skill": k}, err)
}

func (s *Server) retireSkill(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version int    `json:"version"`
		Note    string `json:"note"`
		Author  string `json:"author"`
	}
	if r.ContentLength != 0 && !decode(w, r, 16<<10, &body) {
		return
	}
	k, err := s.Store.RetireSkill(r.Context(), r.PathValue("name"), body.Version,
		store.SkillProvenance{Source: store.SkillByPerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"skill": k}, err)
}

// importBodySkillsLimit bounds an import: store.MaxImportSkills skills, most far under their bound.
const importBodySkillsLimit = 64 << 20

func (s *Server) importSkills(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun bool      `json:"dry_run"`
		Skills []skillIn `json:"skills"`
		Note   string    `json:"note"`
		Author string    `json:"author"`
	}
	if !decode(w, r, importBodySkillsLimit, &body) {
		return
	}
	writes := make([]store.SkillWrite, 0, len(body.Skills))
	for _, k := range body.Skills {
		writes = append(writes, store.SkillWrite{Name: k.Name, Content: k.Content, Files: k.Files})
	}
	res, err := s.Store.ImportSkills(r.Context(), writes,
		store.SkillProvenance{Source: store.SkillByImport, Author: actor(r, body.Author), Note: body.Note}, body.DryRun)
	respond(w, map[string]any{"dry_run": body.DryRun, "results": orEmpty(res)}, err)
}
