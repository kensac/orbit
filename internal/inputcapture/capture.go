// Package inputcapture captures model-visible source artifacts without replaying a run.
package inputcapture

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/client"
)

// Limits bound metadata, artifact count, individual content and total content.
const (
	MaxMetadata      = 2 << 20
	MaxArtifacts     = 128
	MaxArtifactBytes = 16 << 20
	MaxTotalBytes    = 64 << 20
)

// Fetcher exposes only the public request interface; Capture uses GET exclusively.
type Fetcher interface {
	Request(context.Context, string, string, io.Reader, io.Writer) error
}

// Artifact retains the API's source identity and claimed digest.
type Artifact struct {
	ID        string    `json:"id"`
	RunID     string    `json:"run_id"`
	Attempt   int       `json:"attempt"`
	Direction string    `json:"direction"`
	Name      string    `json:"name"`
	MediaType string    `json:"media_type"`
	SHA256    string    `json:"sha256"`
	Bytes     int64     `json:"bytes"`
	CreatedAt time.Time `json:"created_at"`
}

// Input maps a verified source artifact to a fixed local filename.
type Input struct {
	Artifact Artifact `json:"artifact"`
	File     string   `json:"file"`
}

// Manifest states capture integrity separately from reproducibility.
type Manifest struct {
	Version  int     `json:"version"`
	RunID    string  `json:"run_id"`
	Attempt  int     `json:"attempt"`
	Complete bool    `json:"complete"`
	Error    string  `json:"error,omitempty"`
	Inputs   []Input `json:"inputs"`
	Limits   string  `json:"limits"`
}

// ErrIncomplete indicates a retained capture whose manifest explains a sanitized failure.
var ErrIncomplete = errors.New("input capture incomplete; inspect manifest.json")

type bounded struct {
	bytes.Buffer
	max int
}

func (b *bounded) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("response limit")
	}
	return b.Buffer.Write(p)
}

func fetch(ctx context.Context, f Fetcher, endpoint string, max int) ([]byte, error) {
	b := bounded{max: max}
	err := f.Request(ctx, "GET", endpoint, nil, &b)
	return b.Bytes(), err
}

// Capture creates a new private directory, retaining verified inputs and a sanitized
// manifest on remote failure. It never fetches withheld sources or context artifacts.
func Capture(ctx context.Context, f Fetcher, run string, attempt int, target string) (m Manifest, err error) {
	segment, e := client.Segment(run)
	if e != nil || attempt < 1 || target == "" {
		return m, errors.New("valid run, positive attempt and new output directory required")
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if e = os.Mkdir(target, 0700); e != nil {
		return m, e
	}
	root, e := os.OpenRoot(target)
	if e != nil {
		return m, e
	}
	defer root.Close()
	m = Manifest{Version: 1, RunID: run, Attempt: attempt, Inputs: []Input{}, Limits: "Only direction=in inputs/ artifacts excluding reserved _memory.md from the selected attempt. Historical charter, memory, messages, skills, session state, output and withheld sources are not captured. Separate API reads are not an atomic snapshot. This fixture does not reproduce a complete model request or run a backend."}
	defer func() {
		data, _ := json.MarshalIndent(m, "", "  ")
		if e := write(root, "manifest.json", data); e != nil {
			err = e
			return
		}
		if !m.Complete {
			err = ErrIncomplete
			return
		}

	}()
	fail := func(reason string) { m.Error = reason }
	data, e := fetch(ctx, f, "runs/"+segment+"/artifacts", MaxMetadata)
	if e != nil {
		fail("artifact listing unavailable or exceeds limit")
		return
	}
	var listing struct {
		Artifacts []Artifact `json:"artifacts"`
	}
	if e = json.Unmarshal(data, &listing); e != nil || len(listing.Artifacts) >= MaxArtifacts {
		fail("invalid or oversized artifact listing")
		return
	}
	selected := []Artifact{}
	ids := map[string]bool{}
	names := map[string]bool{}
	var total int64
	for _, a := range listing.Artifacts {
		if a.Attempt != attempt || a.Direction != "in" {
			continue
		}
		// Reject malformed candidate paths before any content request.
		if !strings.HasPrefix(a.Name, "inputs/") || path.Base(a.Name) == "_memory.md" {
			continue
		}
		_, idErr := client.Segment(a.ID)
		digest, hashErr := hex.DecodeString(a.SHA256)
		if idErr != nil || a.RunID != run || path.Clean(a.Name) != a.Name || strings.ContainsAny(a.Name, "\\\x00") || strings.HasSuffix(a.Name, "/") || len(a.Name) > 1024 || len(a.MediaType) > 256 || hashErr != nil || len(digest) != 32 || strings.ToLower(a.SHA256) != a.SHA256 || a.Bytes < 0 || a.Bytes > MaxArtifactBytes || ids[a.ID] || names[a.Name] {
			fail("invalid input artifact metadata")
			return
		}
		ids[a.ID] = true
		names[a.Name] = true
		total += a.Bytes
		if total > MaxTotalBytes {
			fail("input total exceeds limit")
			return
		}
		selected = append(selected, a)
	}
	if len(selected) == 0 {
		fail("selected attempt has no model-visible source inputs")
		return
	}
	for _, a := range selected {
		content, e := fetch(ctx, f, "artifacts/"+a.ID, int(a.Bytes))
		if e != nil {
			fail("input content unavailable or exceeds declared size")
			return
		}
		sum := sha256.Sum256(content)
		if int64(len(content)) != a.Bytes || hex.EncodeToString(sum[:]) != a.SHA256 {
			fail("input content size or digest mismatch")
			return
		}
		file := fmt.Sprintf("input-%03d.bin", len(m.Inputs)+1)
		if e = write(root, file, content); e != nil {
			fail("local input write failed")
			return
		}
		m.Inputs = append(m.Inputs, Input{Artifact: a, File: file})
	}
	m.Complete = true
	return
}

func write(root *os.Root, name string, data []byte) error {
	f, e := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return e
	}
	if _, e = f.Write(data); e != nil {
		_ = f.Close()
		return e
	}
	return f.Close()
}
