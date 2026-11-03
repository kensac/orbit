package api

import (
	"fmt"
	"net/http"
	"slices"
	"strings"

	"github.com/kensac/orbit/internal/engine"
	"github.com/kensac/orbit/internal/store"
)

// memoryRoutes are memory entries, topics and what an agent is shown (docs/api.md, Memory entries).
func (s *Server) memoryRoutes(api func(string, http.HandlerFunc)) {
	api("GET /api/v1/memory/entries", s.listEntries)
	api("POST /api/v1/memory/entries", s.createEntry)
	api("GET /api/v1/memory/entries/{id}", s.getEntry)
	api("PATCH /api/v1/memory/entries/{id}", s.updateEntry)
	api("POST /api/v1/memory/entries/{id}/retire", s.retireEntry)
	api("POST /api/v1/memory/entries/{id}/restore", s.restoreEntry)
	api("POST /api/v1/agents/{name}/memory", s.writeMemory)
	api("POST /api/v1/memory/import", s.importEntries)
	api("GET /api/v1/memory/topics", s.listTopics)
	api("GET /api/v1/agents/{name}/context", s.agentContext)
	api("POST /api/v1/conversations/{id}/learn", s.learnNow)
	api("PUT /api/v1/agents/{name}/topics/{topic}", s.attach(store.AttachTopic, "topic"))
	api("DELETE /api/v1/agents/{name}/topics/{topic}", s.detach(store.AttachTopic, "topic"))
}

// entryBodyLimit bounds a request carrying one entry: its fields' bounds (store) and JSON's escaping.
const entryBodyLimit = 128 << 10

// importBodyLimit bounds an import: store.MaxImportEntries at a typical few KB each.
const importBodyLimit = 8 << 20

// entryIn is an entry in a request: its place (scope with topic or agent), key and content.
type entryIn struct {
	Scope string `json:"scope"`
	Topic string `json:"topic"`
	Agent string `json:"agent"`
	Key   string `json:"key"`
	store.EntryFields
}

func (e entryIn) toNew() (store.NewEntry, error) {
	n := store.NewEntry{EntryRef: store.EntryRef{Scope: e.Scope}, Key: e.Key, EntryFields: e.EntryFields}
	switch e.Scope {
	case store.ScopeTopic:
		if e.Agent != "" {
			return n, fmt.Errorf("%w: a topic entry names a topic, not an agent", store.ErrInvalid)
		}
		n.Owner = e.Topic
	case store.ScopeAgent:
		if e.Topic != "" {
			return n, fmt.Errorf("%w: an agent entry names an agent, not a topic", store.ErrInvalid)
		}
		n.Owner = e.Agent
	default:
		if e.Topic != "" || e.Agent != "" {
			return n, fmt.Errorf("%w: only topic and agent entries name a topic or an agent", store.ErrInvalid)
		}
	}
	return n, nil
}

func (s *Server) listEntries(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.EntryFilter{Scope: q.Get("scope"), Type: q.Get("type"), Status: q.Get("status"), Query: q.Get("q"), RunID: q.Get("run_id")}
	if !queryInt(w, r, "limit", &f.Limit) {
		return
	}
	if f.RunID != "" && !store.IsUUID(f.RunID) {
		writeErr(w, http.StatusBadRequest, "run_id is not a UUID")
		return
	}
	switch {
	case q.Get("topic") != "" && q.Get("agent") != "":
		writeErr(w, http.StatusBadRequest, "name a topic or an agent, not both")
		return
	case q.Get("topic") != "":
		f.Scope, f.Owner = store.ScopeTopic, q.Get("topic")
	case q.Get("agent") != "":
		f.Scope, f.Owner = store.ScopeAgent, q.Get("agent")
	}
	if f.Scope != "" && !slices.Contains([]string{store.ScopeProfile, store.ScopeTopic, store.ScopeAgent}, f.Scope) {
		writeErr(w, http.StatusBadRequest, "scope must be profile, topic or agent")
		return
	}
	list, err := s.Store.ListEntries(r.Context(), f)
	respond(w, map[string]any{"entries": orEmpty(list)}, err)
}

func (s *Server) createEntry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		entryIn
		Note   string `json:"note"`
		Author string `json:"author"`
	}
	if !decode(w, r, entryBodyLimit, &body, bodyRequired) {
		return
	}
	n, err := body.toNew()
	if err == nil && n.Scope == store.ScopeAgent {
		if _, ok := s.Engine.Current().Config.Agents[n.Owner]; !ok {
			err = fmt.Errorf("%w: no agent %q is configured", store.ErrInvalid, n.Owner)
		}
	}
	if err != nil {
		respond(w, nil, err)
		return
	}
	e, err := s.Store.CreateEntry(r.Context(), n, store.Provenance{Source: store.SourcePerson, Author: actor(r, body.Author), Note: body.Note})
	respondStatus(w, http.StatusCreated, map[string]any{"entry": e}, err)
}

func (s *Server) getEntry(w http.ResponseWriter, r *http.Request) {
	e, versions, err := s.Store.GetEntry(r.Context(), r.PathValue("id"))
	respond(w, map[string]any{"entry": e, "versions": orEmpty(versions)}, err)
}

func (s *Server) updateEntry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		store.EntryEdit
		Version int    `json:"version"`
		Note    string `json:"note"`
		Author  string `json:"author"`
	}
	if !decode(w, r, entryBodyLimit, &body, bodyRequired) {
		return
	}
	e, err := s.Store.UpdateEntry(r.Context(), r.PathValue("id"), body.Version, body.EntryEdit,
		store.Provenance{Source: store.SourcePerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"entry": e}, err)
}

func (s *Server) retireEntry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Version      int     `json:"version"`
		SupersededBy *string `json:"superseded_by"`
		Note         string  `json:"note"`
		Author       string  `json:"author"`
	}
	if !decode(w, r, 16<<10, &body, bodyOptional) {
		return
	}
	e, err := s.Store.RetireEntry(r.Context(), r.PathValue("id"), body.Version, body.SupersededBy,
		store.Provenance{Source: store.SourcePerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"entry": e}, err)
}

func (s *Server) importEntries(w http.ResponseWriter, r *http.Request) {
	var body struct {
		DryRun  bool      `json:"dry_run"`
		Entries []entryIn `json:"entries"`
		Note    string    `json:"note"`
		Author  string    `json:"author"`
	}
	if !decode(w, r, importBodyLimit, &body, bodyRequired) {
		return
	}
	news := make([]store.NewEntry, 0, len(body.Entries))
	for i, e := range body.Entries {
		n, err := e.toNew()
		if err != nil {
			respond(w, nil, fmt.Errorf("entry %d: %w", i, err))
			return
		}
		news = append(news, n)
	}
	res, err := s.Store.ImportEntries(r.Context(), news,
		store.Provenance{Source: store.SourceImport, Author: actor(r, body.Author), Note: body.Note}, body.DryRun)
	respond(w, map[string]any{"dry_run": body.DryRun, "results": orEmpty(res)}, err)
}

func (s *Server) listTopics(w http.ResponseWriter, r *http.Request) {
	stored, err := s.Store.ListTopics(r.Context())
	if err != nil {
		respond(w, nil, err)
		return
	}
	// A topic only an agent's spec names is listed too, with its agents.
	byName := map[string]store.Topic{}
	for _, t := range stored {
		byName[t.Name] = t
	}
	cfg := s.Engine.Current().Config
	for _, a := range cfg.Agents {
		names := a.Topics
		if a.ReadsAllTopics() {
			var err error
			if names, err = engine.AgentTopics(r.Context(), s.Store, cfg, a); err != nil {
				respond(w, nil, err)
				return
			}
		}
		for _, name := range names {
			t, ok := byName[name]
			if !ok {
				t = store.Topic{Name: name, Agents: []string{}}
			}
			if !slices.Contains(t.Agents, a.Name) {
				t.Agents = append(t.Agents, a.Name)
			}
			byName[name] = t
		}
	}
	topics := make([]store.Topic, 0, len(byName))
	for _, t := range byName {
		slices.Sort(t.Agents)
		topics = append(topics, t)
	}
	slices.SortFunc(topics, func(x, y store.Topic) int { return strings.Compare(x.Name, y.Name) })
	respond(w, map[string]any{"topics": topics}, nil)
}

// agentContext is what a run of the agent is shown beside its charter and memory document: its topics, by where they come from, and the index of entries exactly as a prompt carries it.
func (s *Server) agentContext(w http.ResponseWriter, r *http.Request) {
	a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	attached, err := s.Store.AttachedNames(r.Context(), a.Name, store.AttachTopic)
	if err != nil {
		respond(w, nil, err)
		return
	}
	type topic struct {
		Name string   `json:"name"`
		From []string `json:"from"`
	}
	var topics []topic
	var names []string
	add := func(name, from string) {
		for i := range topics {
			if topics[i].Name == name {
				topics[i].From = append(topics[i].From, from)
				return
			}
		}
		topics = append(topics, topic{Name: name, From: []string{from}})
		names = append(names, name)
	}
	reads, err := engine.AgentTopics(r.Context(), s.Store, s.Engine.Current().Config, a)
	if err != nil {
		respond(w, nil, err)
		return
	}
	for _, t := range reads {
		if a.ReadsAllTopics() || slices.Contains(a.Topics, t) {
			add(t, "spec")
		}
		if slices.Contains(attached, t) {
			add(t, "api")
		}
	}
	slices.SortFunc(topics, func(x, y topic) int { return strings.Compare(x.Name, y.Name) })
	entries, err := s.Store.EntriesFor(r.Context(), a.Name, names)
	if err != nil {
		respond(w, nil, err)
		return
	}
	index := ""
	if len(entries) > 0 {
		index = engine.KnownIndex(entries, s.Engine.Searchable(a))
	}
	attachedSkills, err := s.Store.AttachedNames(r.Context(), a.Name, store.AttachSkill)
	if err != nil {
		respond(w, nil, err)
		return
	}
	type skill struct {
		Name    string   `json:"name"`
		From    []string `json:"from"`
		Version *int     `json:"version"`
		Status  string   `json:"status"`
	}
	var skills []skill
	addSkill := func(name, from string) {
		for i := range skills {
			if skills[i].Name == name {
				skills[i].From = append(skills[i].From, from)
				return
			}
		}
		skills = append(skills, skill{Name: name, From: []string{from}, Status: "missing"})
	}
	for _, k := range a.Skills {
		addSkill(k, "spec")
	}
	for _, k := range attachedSkills {
		addSkill(k, "api")
	}
	for i := range skills {
		if k, err := s.Store.GetSkill(r.Context(), skills[i].Name, 0, false); err == nil {
			v := k.Version
			skills[i].Version, skills[i].Status = &v, k.Status
		}
	}
	slices.SortFunc(skills, func(x, y skill) int { return strings.Compare(x.Name, y.Name) })
	respond(w, map[string]any{"agent": a.Name, "topics": orEmpty(topics), "entries": len(entries), "index": index, "skills": orEmpty(skills)}, nil)
}

func (s *Server) attach(kind, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
		if !ok {
			respond(w, nil, store.ErrNotFound)
			return
		}
		if kind == store.AttachSkill {
			if _, err := s.Store.GetSkill(r.Context(), r.PathValue(param), 0, false); err != nil {
				respond(w, nil, err)
				return
			}
		}
		created, err := s.Store.Attach(r.Context(), a.Name, kind, r.PathValue(param), actor(r, ""))
		respond(w, map[string]any{"attached": err == nil, "created": created}, err)
	}
}

func (s *Server) detach(kind, param string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a, ok := s.Engine.Current().Config.Agents[r.PathValue("name")]
		if !ok {
			respond(w, nil, store.ErrNotFound)
			return
		}
		err := s.Store.Detach(r.Context(), a.Name, kind, r.PathValue(param), actor(r, ""))
		respond(w, map[string]any{"detached": err == nil}, err)
	}
}

// learnNow queues a learning run of one conversation at once (store.LearnNow).
func (s *Server) learnNow(w http.ResponseWriter, r *http.Request) {
	c, err := s.Store.GetConversation(r.Context(), r.PathValue("id"))
	if err != nil {
		respond(w, nil, err)
		return
	}
	a, ok := s.Engine.Current().Config.Agents[c.Agent]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	id, err := s.Store.LearnNow(r.Context(), c.ID, a.SHA)
	if err == nil {
		s.Wake()
	}
	respondStatus(w, http.StatusAccepted, map[string]any{"run_id": id}, err)
}

func (s *Server) restoreEntry(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Restore int    `json:"restore"`
		Version int    `json:"version"`
		Note    string `json:"note"`
		Author  string `json:"author"`
	}
	if !decode(w, r, 16<<10, &body, bodyRequired) {
		return
	}
	e, err := s.Store.RestoreEntry(r.Context(), r.PathValue("id"), body.Restore, body.Version,
		store.Provenance{Source: store.SourcePerson, Author: actor(r, body.Author), Note: body.Note})
	respond(w, map[string]any{"entry": e}, err)
}

// writeMemory writes the next version of an agent's memory document, from content or from an earlier version.
func (s *Server) writeMemory(w http.ResponseWriter, r *http.Request) {
	snap := s.Engine.Current()
	a, ok := snap.Config.Agents[r.PathValue("name")]
	if !ok {
		respond(w, nil, store.ErrNotFound)
		return
	}
	var body struct {
		Content *string `json:"content"`
		Restore int64   `json:"restore"`
		Author  string  `json:"author"`
	}
	if !decode(w, r, int64(snap.Config.Deployment.Defaults.MemoryMaxBytes)*2+4<<10, &body, bodyRequired) {
		return
	}
	switch {
	case (body.Content == nil) == (body.Restore == 0):
		writeErr(w, http.StatusBadRequest, "give content or restore, not both")
		return
	case body.Content != nil && len(*body.Content) > snap.Config.Deployment.Defaults.MemoryMaxBytes:
		writeErr(w, http.StatusBadRequest, fmt.Sprintf("content is longer than memory_max_bytes (%d)", snap.Config.Deployment.Defaults.MemoryMaxBytes))
		return
	}
	content := ""
	if body.Content != nil {
		content = *body.Content
	}
	v, err := s.Store.WriteMemory(r.Context(), a.Name, content, body.Restore, actor(r, body.Author))
	respondStatus(w, http.StatusCreated, map[string]any{"version": v}, err)
}
