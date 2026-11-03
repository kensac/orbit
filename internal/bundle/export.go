// Package bundle exports a bounded public-API view into a portable archive.
package bundle

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/client"
	"github.com/kensac/orbit/internal/jsonstrict"
)

// ErrIncomplete means the published archive records an incomplete resource.
var ErrIncomplete = errors.New("export contains incomplete resources; inspect manifest.json")

// Fetcher is the read side of the authenticated API client.
type Fetcher interface {
	Request(context.Context, string, string, io.Reader, io.Writer) error
}

// Identity preserves API entity identifiers and revision numbers without float conversion.
type Identity struct {
	ID      json.RawMessage `json:"id,omitempty"`
	Name    string          `json:"name,omitempty"`
	Version json.RawMessage `json:"version,omitempty"`
	SpecSHA string          `json:"spec_sha,omitempty"`
}

// Resource describes one fetched API response and its coverage.
type Resource struct {
	Endpoint   string     `json:"endpoint"`
	File       string     `json:"file,omitempty"`
	SHA256     string     `json:"sha256,omitempty"`
	Bytes      int        `json:"bytes"`
	Coverage   string     `json:"coverage"`
	Cursor     string     `json:"cursor,omitempty"`
	NextCursor *string    `json:"next_cursor,omitempty"`
	Entities   []Identity `json:"entities,omitempty"`
	Error      string     `json:"error,omitempty"`
}

// Manifest states what the archive contains and what it cannot promise.
type Manifest struct {
	FormatVersion            int        `json:"format_version"`
	Agent                    string     `json:"agent"`
	Started                  time.Time  `json:"started_at"`
	Finished                 time.Time  `json:"finished_at"`
	ConsistentSnapshot       bool       `json:"consistent_snapshot"`
	BackupComplete           bool       `json:"backup_complete"`
	ResourceCoverageComplete bool       `json:"resource_coverage_complete"`
	Exclusions               []string   `json:"exclusions"`
	Resources                []Resource `json:"resources"`
}

// Limits protect the laptop from an unbounded API response or moving pagination.
// Each response is bounded to 16 MiB; one archive to 256 MiB and 1,000 responses.
const (
	maxResponseBytes = 16 << 20
	maxBundleBytes   = 256 << 20
	maxResponses     = 1000
)

type exporter struct {
	identityBytes int
	fetch         Fetcher
	zip           *zip.Writer
	manifest      Manifest
	bytes         int
	failed        bool
}

// Export publishes a new mode-0600 ZIP file without replacing any existing path.
// Partial API results are published with ErrIncomplete; local write failures publish nothing.
func Export(ctx context.Context, fetch Fetcher, agent, target string) (Manifest, error) {
	segment, err := segment(agent)
	if err != nil {
		return Manifest{}, err
	}
	if target == "" {
		return Manifest{}, errors.New("output path required")
	}
	if _, err := os.Lstat(target); err == nil {
		return Manifest{}, errors.New("output path already exists")
	} else if !errors.Is(err, os.ErrNotExist) {
		return Manifest{}, err
	}
	stage, err := os.CreateTemp(filepath.Dir(target), ".orbit-export-*.zip")
	if err != nil {
		return Manifest{}, err
	}
	defer func() { _ = stage.Close(); _ = os.Remove(stage.Name()) }()
	e := exporter{fetch: fetch, zip: zip.NewWriter(stage), manifest: Manifest{FormatVersion: 1, Agent: agent, Started: time.Now().UTC(), ResourceCoverageComplete: true, Resources: []Resource{}, Exclusions: []string{
		"Responses are separate reads, not a transactionally consistent snapshot; concurrent writes may move or duplicate conversations between pages.",
		"Current selected-agent definition, legacy memory and visible current profile/agent/topic entries only; historical definitions, memory versions and retired entries are excluded.",
		"Only skills attached to the selected agent at context-read time are fetched at their recorded versions; other skills and historical versions are excluded.",
		"Conversation thread summaries are included; upload bytes, run artifacts, journals, transcripts, source inputs, actions outside threads and server configuration are excluded.",
		"Persistent conversation workspace file listings/content, snapshot metadata/archives, retained sandbox HOME and hidden .orbit session state are excluded and are not fetched.",
		"This is a portable API export, not a database backup or a tested restore bundle.",
		"Identifiers and revisions are preserved in payload JSON; the manifest identity index is bounded and may omit entries.",
	}}}
	if err := e.run(ctx, segment); err != nil && !errors.Is(err, ErrIncomplete) {
		_ = e.zip.Close()
		return e.manifest, err
	}
	e.manifest.Finished = time.Now().UTC()
	e.manifest.ResourceCoverageComplete = !e.failed
	data, err := json.MarshalIndent(e.manifest, "", "  ")
	if err != nil {
		return e.manifest, err
	}
	if err = e.write("manifest.json", data); err != nil {
		return e.manifest, err
	}
	if err = e.zip.Close(); err != nil {
		return e.manifest, err
	}
	if err = stage.Sync(); err != nil {
		return e.manifest, err
	}
	if err = stage.Close(); err != nil {
		return e.manifest, err
	}
	// A same-directory hard link publishes the completed file atomically and fails if the target exists.
	if err = os.Link(stage.Name(), target); err != nil {
		return e.manifest, err
	}
	if e.failed {
		return e.manifest, ErrIncomplete
	}
	return e.manifest, nil
}

func (e *exporter) write(name string, data []byte) error {
	h := &zip.FileHeader{Name: name, Method: zip.Deflate}
	h.SetMode(0o600)
	w, err := e.zip.CreateHeader(h)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}

// Keep the buffer named so io.Copy cannot bypass Write through a promoted ReaderFrom.
type boundedBuffer struct{ buffer bytes.Buffer }

func (b *boundedBuffer) Len() int      { return b.buffer.Len() }
func (b *boundedBuffer) Bytes() []byte { return b.buffer.Bytes() }

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > maxResponseBytes-b.Len() {
		return 0, errors.New("response exceeds export bound")
	}
	return b.buffer.Write(p)
}

func (e *exporter) get(ctx context.Context, endpoint, coverage, cursor string) (map[string]json.RawMessage, *Resource, error) {
	r := Resource{Endpoint: endpoint, Coverage: coverage, Cursor: cursor}
	if ctx.Err() != nil {
		r.Coverage = "error"
		r.Error = "export canceled"
		e.failed = true
		e.manifest.Resources = append(e.manifest.Resources, r)
		return nil, nil, ErrIncomplete
	}
	if len(e.manifest.Resources) >= maxResponses || e.bytes >= maxBundleBytes {
		r.Coverage = "error"
		r.Error = "export resource or byte limit reached"
		e.failed = true
		e.manifest.Resources = append(e.manifest.Resources, r)
		return nil, nil, ErrIncomplete
	}
	c, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var body boundedBuffer
	if err := e.fetch.Request(c, "GET", endpoint, nil, &body); err != nil {
		r.Coverage = "error"
		r.Error = "resource fetch failed"
		e.failed = true
		e.manifest.Resources = append(e.manifest.Resources, r)
		return nil, nil, nil
	}
	var obj map[string]json.RawMessage
	if json.Unmarshal(body.Bytes(), &obj) != nil || obj == nil {
		r.Coverage = "error"
		r.Error = "invalid JSON resource"
		e.failed = true
		e.manifest.Resources = append(e.manifest.Resources, r)
		return nil, nil, nil
	}
	if body.Len() > maxBundleBytes-e.bytes {
		r.Coverage = "error"
		r.Error = "export byte limit reached"
		e.failed = true
		e.manifest.Resources = append(e.manifest.Resources, r)
		return nil, nil, ErrIncomplete
	}
	e.bytes += body.Len()
	r.File = fmt.Sprintf("resources/%06d.json", len(e.manifest.Resources)+1)
	r.Bytes = body.Len()
	sum := sha256.Sum256(body.Bytes())
	r.SHA256 = hex.EncodeToString(sum[:])
	r.Entities = identities(obj)
	indexed, _ := json.Marshal(r.Entities)
	if len(indexed) > maxResponseBytes/2-e.identityBytes {
		r.Entities = nil
	} else {
		e.identityBytes += len(indexed)
	}
	if err := e.write(r.File, body.Bytes()); err != nil {
		return nil, nil, err
	}
	e.manifest.Resources = append(e.manifest.Resources, r)
	return obj, &e.manifest.Resources[len(e.manifest.Resources)-1], nil
}

func identities(obj map[string]json.RawMessage) []Identity {
	var out []Identity
	for _, key := range []string{"agent", "skill", "entries", "items", "conversations", "versions"} {
		raw, ok := obj[key]
		if !ok {
			continue
		}
		var rows []json.RawMessage
		if len(raw) > 0 && raw[0] == '[' {
			_ = json.Unmarshal(raw, &rows)
		} else {
			rows = []json.RawMessage{raw}
		}
		for _, row := range rows {
			var id Identity
			if json.Unmarshal(row, &id) == nil && (len(id.ID) > 0 || id.Name != "") && len(id.ID) <= 256 && len(id.Name) <= 256 && len(id.Version) <= 32 && len(id.SpecSHA) <= 128 {
				out = append(out, id)
			}
		}
	}
	return out
}

func (e *exporter) problem(r *Resource, detail string) {
	e.failed = true
	if r != nil {
		r.Coverage = "partial"
		r.Error = detail
	}
}

func (e *exporter) run(ctx context.Context, agent string) error {
	agentObj, agentResource, err := e.get(ctx, "agents/"+agent, "current", "")
	if err != nil {
		return err
	}
	if agentObj != nil {
		var row struct {
			Name string          `json:"name"`
			Spec json.RawMessage `json:"spec"`
		}
		if json.Unmarshal(agentObj["agent"], &row) != nil || row.Name == "" || len(row.Spec) == 0 {
			e.problem(agentResource, "invalid agent definition")
		}
	}
	memoryObj, memoryResource, err := e.get(ctx, "agents/"+agent+"/memory?limit=1", "current", "")
	if err != nil {
		return err
	}
	if memoryObj != nil {
		var versions []json.RawMessage
		if json.Unmarshal(memoryObj["versions"], &versions) != nil || len(versions) > 1 {
			e.problem(memoryResource, "invalid current memory version")
		}
	}
	obj, contextResource, err := e.get(ctx, "agents/"+agent+"/context", "current", "")
	if err != nil {
		return err
	}
	contextIndex := len(e.manifest.Resources) - 1
	scopes := []url.Values{{"scope": {"profile"}}, {"agent": {agent}}}
	if obj != nil {
		var topics []struct {
			Name string `json:"name"`
		}
		if json.Unmarshal(obj["topics"], &topics) != nil {
			e.problem(contextResource, "invalid context topics list")
		}
		for _, t := range topics {
			if _, err := segment(t.Name); err != nil {
				e.problem(contextResource, "invalid context topic")
				continue
			}
			scopes = append(scopes, url.Values{"topic": {t.Name}})
		}
		var skills []struct {
			Name    string `json:"name"`
			Version *int   `json:"version"`
		}
		if json.Unmarshal(obj["skills"], &skills) != nil {
			e.problem(contextResource, "invalid context skills list")
		}
		for _, s := range skills {
			seg, err := segment(s.Name)
			if err != nil || s.Version == nil || *s.Version <= 0 {
				e.problem(&e.manifest.Resources[contextIndex], "context skill has no valid name or version")
				continue
			}
			skillObj, skillResource, err := e.get(ctx, "skills/"+seg+"/versions/"+strconv.Itoa(*s.Version), "pinned-current", "")
			if err != nil {
				return err
			}
			if skillObj != nil {
				var skill struct {
					Name    string `json:"name"`
					Version int    `json:"version"`
				}
				if json.Unmarshal(skillObj["skill"], &skill) != nil || skill.Name != s.Name || skill.Version != *s.Version {
					e.problem(skillResource, "pinned skill revision mismatch")
				}
			}
		}
	}
	for _, q := range scopes {
		q.Set("limit", "1000")
		q.Set("status", "active")
		obj, r, err := e.get(ctx, "memory/entries?"+q.Encode(), "current", "")
		if err != nil {
			return err
		}
		if obj == nil {
			continue
		}
		var entries []json.RawMessage
		if json.Unmarshal(obj["entries"], &entries) != nil {
			e.problem(r, "invalid entries list")
		} else if len(entries) >= 1000 {
			e.problem(r, "possibly truncated: API has limit 1000 without a pagination cursor")
		}
	}
	seen := map[string]bool{}
	for _, archived := range []string{"false", "true"} {
		endpoint := "agents/" + agent + "/conversations?archived=" + archived + "&limit=200"
		if err := e.pages(ctx, endpoint, "conversations", func(rows []json.RawMessage) error {
			for _, raw := range rows {
				var c struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(raw, &c) != nil {
					e.failed = true
					continue
				}
				id, err := segment(c.ID)
				if err != nil {
					e.failed = true
					continue
				}
				if seen[id] {
					continue
				}
				seen[id] = true
				if err = e.pages(ctx, "conversations/"+id+"/thread?limit=200", "items", nil); err != nil {
					return err
				}
			}
			return nil
		}); err != nil {
			return err
		}
	}
	return nil
}

func (e *exporter) pages(ctx context.Context, endpoint, key string, visit func([]json.RawMessage) error) error {
	cursor := ""
	seen := map[string]bool{}
	for {
		path := endpoint
		if cursor != "" {
			path += "&before=" + url.QueryEscape(cursor)
		}
		obj, r, err := e.get(ctx, path, "page", cursor)
		if err != nil {
			return err
		}
		if obj == nil {
			return nil
		}
		var rows []json.RawMessage
		var more *bool
		var next *string
		if json.Unmarshal(obj[key], &rows) != nil || json.Unmarshal(obj["has_more"], &more) != nil || more == nil || json.Unmarshal(obj["next_before"], &next) != nil {
			e.problem(r, "invalid pagination envelope")
			return nil
		}
		r.NextCursor = next
		if next != nil && !validCursor(*next) {
			r.NextCursor = nil
			e.problem(r, "pagination cursor exceeds export bound")
			return nil
		}
		if (*more && (next == nil || *next == "")) || (!*more && next != nil) {
			e.problem(r, "inconsistent pagination cursor")
			return nil
		}
		if next != nil && seen[*next] {
			e.problem(r, "pagination cursor repeated")
			return nil
		}
		if key == "conversations" {
			for _, raw := range rows {
				var c struct {
					ID string `json:"id"`
				}
				if json.Unmarshal(raw, &c) != nil {
					e.problem(r, "invalid conversation identifier")
					return nil
				}
				if _, err := segment(c.ID); err != nil {
					e.problem(r, "invalid conversation identifier")
					return nil
				}
			}
		}
		if visit != nil {
			if err := visit(rows); err != nil {
				return err
			}
		}
		if !*more {
			return nil
		}
		cursor = *next
		seen[cursor] = true
	}
}

// Verify checks the exact member set, bounded metadata, sizes and SHA256 without extracting files.
func Verify(filename string) error {
	_, err := Inspect(filename)
	return err
}

// Inspect returns the manifest only after all Verify integrity checks succeed.
// Resource coverage describes bounded API reads, not completeness or restorability.
func Inspect(filename string) (Manifest, error) {
	var m Manifest
	err := verify(filename, &m)
	return m, err
}

func verify(filename string, validated *Manifest) error {
	file, err := os.Open(filename)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return err
	}
	if info.Size() < 0 || info.Size() > maxBundleBytes+maxResponseBytes+maxResponses*1024 {
		return errors.New("archive size exceeds format bounds")
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBundleBytes+maxResponseBytes+maxResponses*1024+1))
	if err != nil {
		return err
	}
	if int64(len(data)) != info.Size() {
		return errors.New("archive size changed while reading")
	}
	return verifyData(data, validated)
}

func verifyData(data []byte, validated *Manifest) error {
	reader := bytes.NewReader(data)
	if err := preflight(reader, int64(len(data))); err != nil {
		return err
	}
	z, err := zip.NewReader(reader, int64(len(data)))
	if err != nil {
		return err
	}
	if len(z.File) > maxResponses+1 {
		return errors.New("too many archive members")
	}
	files := map[string]*zip.File{}
	var total uint64
	for _, f := range z.File {
		if f.Name != "manifest.json" && !validMember(f.Name) {
			return errors.New("unsafe archive member")
		}
		if f.FileInfo().Mode()&os.ModeType != 0 {
			return errors.New("nonregular archive member")
		}
		if _, ok := files[f.Name]; ok {
			return errors.New("duplicate archive member")
		}
		if f.UncompressedSize64 > maxResponseBytes {
			return errors.New("archive member exceeds bound")
		}
		total += f.UncompressedSize64
		if total > maxBundleBytes+maxResponseBytes {
			return errors.New("archive exceeds size bound")
		}
		files[f.Name] = f
	}
	mf := files["manifest.json"]
	if mf == nil {
		return errors.New("missing manifest")
	}
	r, err := mf.Open()
	if err != nil {
		return err
	}
	manifestData, err := io.ReadAll(io.LimitReader(r, maxResponseBytes+1))
	_ = r.Close()
	if err != nil {
		return err
	}
	if err := jsonstrict.ValidateJSON(manifestData, maxResponseBytes); err != nil {
		return fmt.Errorf("manifest: %w", err)
	}
	var m Manifest
	dec := json.NewDecoder(bytes.NewReader(manifestData))
	dec.DisallowUnknownFields()
	err = dec.Decode(&m)
	if err == nil {
		var extra any
		if dec.Decode(&extra) != io.EOF {
			err = errors.New("trailing manifest data")
		}
	}
	if err != nil {
		return err
	}
	if m.BackupComplete || m.ConsistentSnapshot || m.FormatVersion != 1 || len(m.Resources) > maxResponses+1 || m.Started.IsZero() || m.Finished.Before(m.Started) {
		return errors.New("invalid bundle metadata")
	}
	if _, err = client.Segment(m.Agent); err != nil {
		return err
	}
	used := map[string]bool{"manifest.json": true}
	for _, res := range m.Resources {
		if res.Endpoint == "" {
			return errors.New("missing resource endpoint")
		}
		switch res.Coverage {
		case "current", "pinned-current", "page", "error", "partial":
		default:
			return errors.New("invalid resource coverage")
		}
		if m.ResourceCoverageComplete && (res.Coverage == "error" || res.Coverage == "partial") {
			return errors.New("inconsistent resource coverage")
		}
		if res.File == "" {
			if res.Coverage != "error" || res.Bytes != 0 || res.SHA256 != "" {
				return errors.New("invalid empty resource metadata")
			}
			continue
		}
		if !validMember(res.File) || used[res.File] {
			return errors.New("invalid or duplicate resource path")
		}
		used[res.File] = true
		if res.Bytes < 0 || res.Bytes > maxResponseBytes {
			return errors.New("invalid resource size")
		}
		digest, err := hex.DecodeString(res.SHA256)
		if err != nil || len(digest) != sha256.Size {
			return errors.New("invalid resource digest")
		}
		f := files[res.File]
		if f == nil || f.UncompressedSize64 != uint64(res.Bytes) {
			return errors.New("missing resource or size mismatch")
		}
		r, err := f.Open()
		if err != nil {
			return err
		}
		h := sha256.New()
		n, err := io.Copy(h, io.LimitReader(r, maxResponseBytes+1))
		_ = r.Close()
		if err != nil {
			return err
		}
		if n != int64(res.Bytes) || hex.EncodeToString(h.Sum(nil)) != res.SHA256 {
			return errors.New("resource checksum mismatch")
		}
	}
	if len(used) != len(files) {
		return errors.New("unlisted archive member")
	}
	*validated = m
	return nil
}

func validMember(name string) bool {
	if path.Clean(name) != name || !strings.HasPrefix(name, "resources/") || !strings.HasSuffix(name, ".json") {
		return false
	}
	n, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, "resources/"), ".json"))
	return err == nil && n > 0 && n <= maxResponses && name == fmt.Sprintf("resources/%06d.json", n)
}

func segment(s string) (string, error) {
	if len(s) > 256 {
		return "", errors.New("identifier exceeds export bound")
	}
	return client.Segment(s)
}

func validCursor(s string) bool {
	if len(s) > 1024 {
		return false
	}
	for _, b := range []byte(s) {
		if b < 0x20 || b > 0x7e {
			return false
		}
	}
	return true
}

// The emitted format uses a single-disk non-ZIP64 directory with no archive comment.
// Validate its actual central-record count before archive/zip allocates file objects.
func preflight(file io.ReaderAt, size int64) error {
	if size < 22 || size > maxBundleBytes+maxResponseBytes+maxResponses*1024 {
		return errors.New("archive size exceeds format bounds")
	}
	var end [22]byte
	if _, err := file.ReadAt(end[:], size-22); err != nil {
		return err
	}
	if string(end[:4]) != "PK\x05\x06" || binary.LittleEndian.Uint16(end[4:6]) != 0 || binary.LittleEndian.Uint16(end[6:8]) != 0 || binary.LittleEndian.Uint16(end[20:22]) != 0 {
		return errors.New("unsupported ZIP directory")
	}
	count := int(binary.LittleEndian.Uint16(end[10:12]))
	if count > maxResponses+1 || int(binary.LittleEndian.Uint16(end[8:10])) != count {
		return errors.New("invalid ZIP member count")
	}
	length := int64(binary.LittleEndian.Uint32(end[12:16]))
	offset := int64(binary.LittleEndian.Uint32(end[16:20]))
	if length > int64((maxResponses+1)*(46+21+1024)) || offset+length != size-22 {
		return errors.New("invalid ZIP directory size")
	}
	pos := offset
	actual := 0
	for pos < offset+length {
		var header [46]byte
		if _, err := file.ReadAt(header[:], pos); err != nil {
			return err
		}
		if string(header[:4]) != "PK\x01\x02" {
			return errors.New("invalid ZIP central record")
		}
		name := int64(binary.LittleEndian.Uint16(header[28:30]))
		extra := int64(binary.LittleEndian.Uint16(header[30:32]))
		comment := int64(binary.LittleEndian.Uint16(header[32:34]))
		if name > 21 || extra > 1024 || comment != 0 {
			return errors.New("ZIP record metadata exceeds bound")
		}
		pos += 46 + name + extra
		actual++
		if actual > maxResponses+1 || pos > offset+length {
			return errors.New("invalid ZIP central record count")
		}
	}
	if actual != count {
		return errors.New("ZIP member count mismatch")
	}
	return nil
}
