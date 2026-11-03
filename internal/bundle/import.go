package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"slices"
	"strconv"
	"strings"

	"github.com/kensac/orbit/internal/client"
	"github.com/kensac/orbit/internal/store"
)

// Importer is the authenticated write side for portable state imports.
type Importer interface {
	Request(context.Context, string, string, io.Reader, io.Writer) error
}

// ImportResult reports each independently committed API operation. The API
// import endpoints are idempotent, so a failed import can be retried safely.
type ImportResult struct {
	Agent                        string          `json:"agent"`
	DryRun                       bool            `json:"dry_run"`
	MemoryDocument               string          `json:"memory_document"`
	Entries                      json.RawMessage `json:"entries,omitempty"`
	Skills                       json.RawMessage `json:"skills,omitempty"`
	Attachments                  []string        `json:"attachments,omitempty"`
	ConversationArchivesCount    int             `json:"conversation_archives_in_bundle"`
	ConversationArchivesImported bool            `json:"conversation_archives_imported"`
	ConversationImportResults    json.RawMessage `json:"conversation_archive_results,omitempty"`
}

type bundleResources struct {
	manifest Manifest
	values   map[string]json.RawMessage
}

type importedEntry struct {
	Scope       string `json:"scope"`
	Topic       string `json:"topic,omitempty"`
	Agent       string `json:"agent,omitempty"`
	Key         string `json:"key"`
	Type        string `json:"type"`
	Title       string `json:"title"`
	Description string `json:"description,omitempty"`
	Body        string `json:"body"`
	Why         string `json:"why,omitempty"`
	HowToApply  string `json:"how_to_apply,omitempty"`
	Pinned      bool   `json:"pinned,omitempty"`
}

type importedSkill struct {
	Name    string         `json:"name"`
	Content string         `json:"content"`
	Files   []importedFile `json:"files,omitempty"`
}

type importedFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

type contextIndex struct {
	Topics []struct {
		Name string   `json:"name"`
		From []string `json:"from"`
	} `json:"topics"`
	Skills []struct {
		Name string   `json:"name"`
		From []string `json:"from"`
	} `json:"skills"`
}

// Import restores the selected agent's current memory and skills to an
// already configured target agent. Conversation records remain in the
// archive; restoring them requires a transcript-preserving API operation.
func Import(ctx context.Context, api Importer, filename, target string, dryRun bool) (ImportResult, error) {
	var out ImportResult
	targetSegment, err := client.Segment(target)
	if err != nil {
		return out, fmt.Errorf("target agent: %w", err)
	}
	r, err := loadBundle(filename)
	if err != nil {
		return out, err
	}
	if !r.manifest.ResourceCoverageComplete {
		return out, errors.New("bundle has incomplete resource coverage")
	}
	bundleHash, err := bundleSHA256(filename)
	if err != nil {
		return out, err
	}
	sourceSegment, err := client.Segment(r.manifest.Agent)
	if err != nil {
		return out, errors.New("bundle has an invalid source agent")
	}
	for _, resource := range r.manifest.Resources {
		if resource.Coverage == "error" || resource.Coverage == "partial" {
			return out, fmt.Errorf("bundle resource %q is incomplete", resource.Endpoint)
		}
	}
	out.Agent, out.DryRun = target, dryRun
	source := r.manifest.Agent
	if _, err := requestJSON(ctx, api, "GET", "agents/"+targetSegment, nil); err != nil {
		return out, fmt.Errorf("target agent %q is not available: %w", target, err)
	}

	var memory string
	if obj, ok := r.values["agents/"+sourceSegment+"/memory?limit=1"]; ok {
		var response struct {
			Versions []struct {
				Content string `json:"content"`
			} `json:"versions"`
		}
		if err := json.Unmarshal(obj, &response); err != nil || len(response.Versions) > 1 {
			return out, errors.New("bundle has invalid current agent memory")
		}
		if len(response.Versions) == 1 {
			memory = response.Versions[0].Content
		}
	}
	if len(memory) > 16<<10 {
		return out, errors.New("current agent memory exceeds the API write limit")
	}
	entries, err := memoryEntries(r, source, target)
	if err != nil {
		return out, err
	}
	skills, err := skillVersions(r, source, sourceSegment)
	if err != nil {
		return out, err
	}
	var contextData contextIndex
	if raw, ok := r.values["agents/"+sourceSegment+"/context"]; ok {
		if err := json.Unmarshal(raw, &contextData); err != nil {
			return out, errors.New("bundle has invalid agent context")
		}
	}
	if len(skills) > 0 {
		out.Skills, err = importRecords(ctx, api, "skills/import", "skills", skills, 100, true)
		if err != nil {
			return out, fmt.Errorf("validate imported skills: %w", err)
		}
	}
	if len(entries) > 0 {
		out.Entries, err = importRecords(ctx, api, "memory/import", "entries", entries, 500, true)
		if err != nil {
			return out, fmt.Errorf("validate imported memory entries: %w", err)
		}
	}
	if memory != "" {
		out.MemoryDocument = "would import"
		var current struct {
			Versions []struct {
				Content string `json:"content"`
			} `json:"versions"`
		}
		raw, err := requestJSON(ctx, api, "GET", "agents/"+targetSegment+"/memory?limit=1", nil)
		if err != nil {
			return out, fmt.Errorf("read target memory: %w", err)
		}
		if err := json.Unmarshal(raw, &current); err != nil || len(current.Versions) > 1 {
			return out, errors.New("target agent returned invalid current memory")
		}
		if len(current.Versions) > 0 && current.Versions[0].Content == memory {
			out.MemoryDocument = "unchanged"
		}
	}
	for _, a := range contextData.Topics {
		if contains(a.From, "api") {
			if _, err := client.Segment(a.Name); err != nil {
				return out, fmt.Errorf("bundle has invalid topic attachment %q", a.Name)
			}
			out.Attachments = append(out.Attachments, "topic:"+a.Name)
		}
	}
	for _, a := range contextData.Skills {
		if contains(a.From, "api") {
			if _, err := client.Segment(a.Name); err != nil {
				return out, fmt.Errorf("bundle has invalid skill attachment %q", a.Name)
			}
			out.Attachments = append(out.Attachments, "skill:"+a.Name)
		}
	}
	archives, err := conversationArchives(r, source, bundleHash)
	if err != nil {
		return out, err
	}
	out.ConversationArchivesCount = len(archives)
	if len(archives) > 0 {
		out.ConversationImportResults, err = importConversationArchives(ctx, api, targetSegment, archives, true)
		if err != nil {
			return out, fmt.Errorf("validate imported conversation archives: %w", err)
		}
	}
	if dryRun {
		return out, nil
	}

	if len(skills) > 0 {
		out.Skills, err = importRecords(ctx, api, "skills/import", "skills", skills, 100, false)
		if err != nil {
			return out, fmt.Errorf("import skills: %w", err)
		}
	}
	if len(entries) > 0 {
		out.Entries, err = importRecords(ctx, api, "memory/import", "entries", entries, 500, false)
		if err != nil {
			return out, fmt.Errorf("import memory entries: %w", err)
		}
	}
	if memory != "" && out.MemoryDocument != "unchanged" {
		payload, _ := json.Marshal(map[string]string{"content": memory})
		if _, err := requestJSON(ctx, api, "POST", "agents/"+targetSegment+"/memory", payload); err != nil {
			return out, fmt.Errorf("import current agent memory: %w", err)
		}
		out.MemoryDocument = "imported"
	}
	for _, attachment := range out.Attachments {
		kind, name, _ := strings.Cut(attachment, ":")
		segment, err := client.Segment(name)
		if err != nil {
			return out, fmt.Errorf("invalid %s attachment: %w", kind, err)
		}
		if _, err := requestJSON(ctx, api, "PUT", "agents/"+targetSegment+"/"+kind+"s/"+segment, nil); err != nil {
			return out, fmt.Errorf("attach imported %s %q: %w", kind, name, err)
		}
	}
	if len(archives) > 0 {
		out.ConversationImportResults, err = importConversationArchives(ctx, api, targetSegment, archives, false)
		if err != nil {
			return out, fmt.Errorf("import conversation archives: %w", err)
		}
		out.ConversationArchivesImported = true
	}
	return out, nil
}

func bundleSHA256(filename string) (string, error) {
	f, err := os.Open(filename)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

type importedConversationArchive struct {
	SourceAgent          string          `json:"source_agent"`
	SourceConversationID string          `json:"source_conversation_id"`
	SourceBundleSHA256   string          `json:"source_bundle_sha256"`
	Snapshot             json.RawMessage `json:"snapshot"`
}

func conversationArchives(r bundleResources, source, bundleHash string) ([]importedConversationArchive, error) {
	type sourceConversation struct {
		ID string `json:"id"`
	}
	conversations := map[string]json.RawMessage{}
	conversationPrefix := "agents/" + source + "/conversations?"
	for _, resource := range r.manifest.Resources {
		if !strings.HasPrefix(resource.Endpoint, conversationPrefix) {
			continue
		}
		raw, ok := r.values[resource.Endpoint]
		if !ok {
			return nil, fmt.Errorf("bundle is missing conversations page %q", resource.Endpoint)
		}
		var page struct {
			Conversations []json.RawMessage `json:"conversations"`
		}
		if err := json.Unmarshal(raw, &page); err != nil {
			return nil, errors.New("bundle has an invalid conversations page")
		}
		for _, item := range page.Conversations {
			var c sourceConversation
			if err := json.Unmarshal(item, &c); err != nil || !store.IsUUID(c.ID) {
				return nil, errors.New("bundle has an invalid source conversation")
			}
			if _, exists := conversations[c.ID]; !exists {
				conversations[c.ID] = item
			}
		}
	}
	type threadArchive struct {
		Conversation json.RawMessage   `json:"conversation"`
		Pages        []json.RawMessage `json:"pages"`
	}
	threads := make(map[string]*threadArchive, len(conversations))
	for id, conversation := range conversations {
		threads[id] = &threadArchive{Conversation: conversation, Pages: []json.RawMessage{}}
	}
	for _, resource := range r.manifest.Resources {
		path, _, _ := strings.Cut(resource.Endpoint, "?")
		parts := strings.Split(path, "/")
		if len(parts) != 3 || parts[0] != "conversations" || parts[2] != "thread" {
			continue
		}
		id := parts[1]
		if threads[id] == nil {
			return nil, fmt.Errorf("bundle thread %q has no source conversation metadata", id)
		}
		page, ok := r.values[resource.Endpoint]
		if !ok {
			return nil, fmt.Errorf("bundle is missing thread page %q", resource.Endpoint)
		}
		threads[id].Pages = append(threads[id].Pages, page)
	}
	ids := make([]string, 0, len(threads))
	for id, thread := range threads {
		if len(thread.Pages) == 0 {
			return nil, fmt.Errorf("bundle has no thread pages for conversation %q", id)
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	out := make([]importedConversationArchive, 0, len(ids))
	for _, id := range ids {
		snapshot, err := json.Marshal(threads[id])
		if err != nil {
			return nil, err
		}
		if len(snapshot) > 8<<20 {
			return nil, fmt.Errorf("conversation %q transcript exceeds the 8 MiB archive limit", id)
		}
		out = append(out, importedConversationArchive{SourceAgent: source, SourceConversationID: id,
			SourceBundleSHA256: bundleHash, Snapshot: snapshot})
	}
	return out, nil
}

func importConversationArchives(ctx context.Context, api Importer, target string, archives []importedConversationArchive, dryRun bool) (json.RawMessage, error) {
	var results []json.RawMessage
	for _, archive := range archives {
		payload, err := json.Marshal(map[string]any{"dry_run": dryRun, "archives": []importedConversationArchive{archive}})
		if err != nil {
			return nil, err
		}
		response, err := requestJSON(ctx, api, "POST", "agents/"+target+"/conversation-archives/import", payload)
		if err != nil {
			return nil, err
		}
		var result struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(response, &result); err != nil || len(result.Results) != 1 {
			return nil, errors.New("conversation archive import API returned an invalid result")
		}
		results = append(results, result.Results[0])
	}
	return json.Marshal(map[string]any{"results": results})
}

func importRecords(ctx context.Context, api Importer, endpoint, field string, records any, limit int, dryRun bool) (json.RawMessage, error) {
	var rows []any
	switch values := records.(type) {
	case []importedEntry:
		for _, value := range values {
			rows = append(rows, value)
		}
	case []importedSkill:
		for _, value := range values {
			rows = append(rows, value)
		}
	default:
		return nil, errors.New("unsupported import record type")
	}
	var results []json.RawMessage
	for start := 0; start < len(rows); start += limit {
		end := min(start+limit, len(rows))
		payload, err := json.Marshal(map[string]any{
			"dry_run": dryRun,
			field:     rows[start:end],
			"note":    "Imported from a portable Orbit bundle.",
		})
		if err != nil {
			return nil, err
		}
		response, err := requestJSON(ctx, api, "POST", endpoint, payload)
		if err != nil {
			return nil, err
		}
		var result struct {
			Results []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(response, &result); err != nil || len(result.Results) != end-start {
			return nil, errors.New("import API returned an invalid result count")
		}
		results = append(results, result.Results...)
	}
	return json.Marshal(map[string]any{"results": results})
}

func loadBundle(filename string) (out bundleResources, retErr error) {
	manifest, err := Inspect(filename)
	if err != nil {
		return out, err
	}
	archive, err := zip.OpenReader(filename)
	if err != nil {
		return out, err
	}
	defer func() {
		if err := archive.Close(); err != nil && retErr == nil {
			retErr = err
		}
	}()
	out.manifest = manifest
	out.values = make(map[string]json.RawMessage, len(manifest.Resources))
	files := make(map[string]*zip.File, len(archive.File))
	for _, file := range archive.File {
		files[file.Name] = file
	}
	for _, resource := range manifest.Resources {
		if resource.File == "" {
			continue
		}
		file := files[resource.File]
		if file == nil {
			return out, fmt.Errorf("bundle resource %q is missing", resource.File)
		}
		reader, err := file.Open()
		if err != nil {
			return out, err
		}
		data, err := io.ReadAll(io.LimitReader(reader, maxResponseBytes+1))
		_ = reader.Close()
		if err != nil || len(data) > maxResponseBytes {
			return out, fmt.Errorf("bundle resource %q exceeds its read limit", resource.Endpoint)
		}
		out.values[resource.Endpoint] = data
	}
	return out, nil
}

func requestJSON(ctx context.Context, api Importer, method, path string, body []byte) (json.RawMessage, error) {
	var response bytes.Buffer
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(body)
	}
	if err := api.Request(ctx, method, path, input, &response); err != nil {
		return nil, err
	}
	if response.Len() == 0 {
		return json.RawMessage("{}"), nil
	}
	if response.Len() > maxResponseBytes {
		return nil, errors.New("API response exceeds 16 MiB")
	}
	if err := json.Unmarshal(response.Bytes(), new(any)); err != nil {
		return nil, errors.New("API returned invalid JSON")
	}
	return append(json.RawMessage(nil), response.Bytes()...), nil
}

func memoryEntries(r bundleResources, source, target string) ([]importedEntry, error) {
	var out []importedEntry
	for endpoint, raw := range r.values {
		if !strings.HasPrefix(endpoint, "memory/entries?") {
			continue
		}
		query, err := url.ParseQuery(strings.TrimPrefix(endpoint, "memory/entries?"))
		if err != nil {
			return nil, errors.New("bundle has an invalid memory endpoint")
		}
		if query.Get("agent") != source && query.Get("agent") != "" {
			continue
		}
		if query.Get("limit") != "1000" || query.Get("status") != "active" || len(query) < 2 || len(query) > 3 {
			return nil, errors.New("bundle has an unsupported memory entries query")
		}
		scope, owner := "", ""
		switch {
		case query.Get("scope") == "profile":
			scope = "profile"
		case query.Get("agent") == source:
			scope, owner = "agent", source
		case query.Get("topic") != "":
			scope, owner = "topic", query.Get("topic")
		default:
			return nil, errors.New("bundle has an unsupported memory entries scope")
		}
		var response struct {
			Entries []struct {
				Scope       string `json:"scope"`
				Topic       string `json:"topic"`
				Agent       string `json:"agent"`
				Key         string `json:"key"`
				Type        string `json:"type"`
				Title       string `json:"title"`
				Description string `json:"description"`
				Body        string `json:"body"`
				Why         string `json:"why"`
				HowToApply  string `json:"how_to_apply"`
				Pinned      bool   `json:"pinned"`
			} `json:"entries"`
		}
		if err := json.Unmarshal(raw, &response); err != nil {
			return nil, errors.New("bundle has invalid memory entries")
		}
		for _, entry := range response.Entries {
			item := importedEntry{Scope: entry.Scope, Topic: entry.Topic, Agent: entry.Agent, Key: entry.Key, Type: entry.Type, Title: entry.Title, Description: entry.Description, Body: entry.Body, Why: entry.Why, HowToApply: entry.HowToApply, Pinned: entry.Pinned}
			if item.Scope != scope {
				return nil, errors.New("bundle memory entry scope does not match its endpoint")
			}
			switch item.Scope {
			case "agent":
				if item.Agent != "" && item.Agent != source {
					return nil, errors.New("bundle contains an entry for a different agent")
				}
				item.Agent = target
			case "topic":
				if item.Topic != "" && item.Topic != owner {
					return nil, errors.New("bundle topic entry does not match its endpoint")
				}
				item.Topic = owner
			case "profile":
				if item.Agent != "" || item.Topic != "" {
					return nil, errors.New("bundle profile entry has an owner")
				}
			default:
				return nil, errors.New("bundle has an invalid memory entry scope")
			}
			out = append(out, item)
		}
	}
	return out, nil
}

func skillVersions(r bundleResources, source, sourceSegment string) ([]importedSkill, error) {
	var contextData contextIndex
	raw, ok := r.values["agents/"+sourceSegment+"/context"]
	if !ok {
		return nil, nil
	}
	if err := json.Unmarshal(raw, &contextData); err != nil {
		return nil, errors.New("bundle has invalid agent context")
	}
	var out []importedSkill
	for _, item := range contextData.Skills {
		seg, err := client.Segment(item.Name)
		if err != nil {
			return nil, errors.New("bundle has invalid skill name")
		}
		var version int
		for _, resource := range r.manifest.Resources {
			prefix := "skills/" + seg + "/versions/"
			if strings.HasPrefix(resource.Endpoint, prefix) {
				version, _ = strconv.Atoi(strings.TrimPrefix(resource.Endpoint, prefix))
				break
			}
		}
		if version == 0 {
			return nil, fmt.Errorf("bundle is missing the pinned version of skill %q", item.Name)
		}
		obj := r.values["skills/"+seg+"/versions/"+strconv.Itoa(version)]
		var wrapper struct {
			Skill struct {
				Name    string `json:"name"`
				Content string `json:"content"`
				Files   any    `json:"files"`
			} `json:"skill"`
		}
		if err := json.Unmarshal(obj, &wrapper); err != nil || wrapper.Skill.Name != item.Name {
			return nil, fmt.Errorf("bundle has invalid skill %q", item.Name)
		}
		files, err := skillFiles(wrapper.Skill.Files)
		if err != nil {
			return nil, fmt.Errorf("bundle skill %q: %w", item.Name, err)
		}
		out = append(out, importedSkill{Name: item.Name, Content: wrapper.Skill.Content, Files: files})
	}
	return out, nil
}

func skillFiles(value any) ([]importedFile, error) {
	if value == nil {
		return nil, nil
	}
	var out []importedFile
	switch files := value.(type) {
	case []any:
		for _, raw := range files {
			row, ok := raw.(map[string]any)
			if !ok {
				return nil, errors.New("invalid files list")
			}
			path, _ := row["path"].(string)
			content, _ := row["content"].(string)
			out = append(out, importedFile{Path: path, Content: content})
		}
	case map[string]any:
		for path, content := range files {
			value, ok := content.(string)
			if !ok {
				return nil, errors.New("invalid file content")
			}
			out = append(out, importedFile{Path: path, Content: value})
		}
		slices.SortFunc(out, func(a, b importedFile) int { return strings.Compare(a.Path, b.Path) })
	default:
		return nil, errors.New("invalid files value")
	}
	return out, nil
}

func contains(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
