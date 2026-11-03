// Package extensionproto validates inert ordinary-source invocation envelopes.
// It does not execute packages, validate schema instances or access artifacts.
package extensionproto

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kensac/orbit/internal/jsonstrict"
)

// Protocol is the only supported envelope version.
const Protocol = "orbit.extension.v1"

// Limits are the design's envelope and artifact ceilings, enforced before decoding.
const (
	MaxJSONBytes = 1 << 20
	MaxFiles     = 32
	MaxFileBytes = 16 << 20
)

var nameRE = regexp.MustCompile(`^[a-z][a-z0-9-]{0,62}$`)
var idRE = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)
var hashRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

// File describes bytes only. Paths are relative to the invocation input root
// for inputs, and /work/out for outputs. Validation never opens these paths.
type File struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	MediaType string `json:"media_type"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
}

// Describe returns a file descriptor bound to exact content bytes.
func Describe(name, path, mediaType string, content []byte) File {
	sum := sha256.Sum256(content)
	return File{Name: name, Path: path, MediaType: mediaType, Bytes: int64(len(content)), SHA256: hex.EncodeToString(sum[:])}
}

// Request carries one source operation and its explicitly named input files.
// Parameters retain decimal literals. A separate schema validator is required
// before any future execution; successful decoding supplies no authorization.
type Request struct {
	Protocol     string          `json:"protocol"`
	InvocationID string          `json:"invocation_id"`
	Operation    string          `json:"operation"`
	Parameters   json.RawMessage `json:"parameters"`
	Inputs       []File          `json:"inputs"`
}

// Failure is untrusted diagnostic data, never an instruction.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Response echoes request identity and contains exactly one result or error.
type Response struct {
	Protocol     string          `json:"protocol"`
	InvocationID string          `json:"invocation_id"`
	Status       string          `json:"status"`
	Result       json.RawMessage `json:"result,omitempty"`
	Error        *Failure        `json:"error,omitempty"`
	Outputs      []File          `json:"outputs,omitempty"`
}

// DecodeRequest reads at most the envelope ceiling plus one byte. Declared
// slots must match the selected source operation's input slots exactly.
func DecodeRequest(r io.Reader, operation string, slots []string) (*Request, error) {
	b, err := read(r)
	if err != nil {
		return nil, err
	}
	if err = shape(b, []string{"protocol", "invocation_id", "operation", "parameters", "inputs"}, nil); err != nil {
		return nil, err
	}
	if err = fileShapes(b, "inputs"); err != nil {
		return nil, err
	}
	var v Request
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.Protocol != Protocol || !idRE.MatchString(v.InvocationID) || !nameRE.MatchString(operation) || v.Operation != operation {
		return nil, errors.New("invalid request identity")
	}
	if err = value(v.Parameters); err != nil {
		return nil, fmt.Errorf("parameters: %w", err)
	}
	if err = files(v.Inputs); err != nil {
		return nil, err
	}
	if len(slots) > MaxFiles {
		return nil, errors.New("too many declared slots")
	}
	expected := map[string]bool{}
	for _, s := range slots {
		if !nameRE.MatchString(s) || expected[s] {
			return nil, errors.New("invalid declared slots")
		}
		expected[s] = true
	}
	if len(expected) != len(v.Inputs) {
		return nil, errors.New("input slots do not match declaration")
	}
	for _, f := range v.Inputs {
		if !expected[f.Name] {
			return nil, errors.New("undeclared input slot")
		}
	}
	return &v, nil
}

// DecodeResponse reads a bounded response and correlates it to one invocation.
func DecodeResponse(r io.Reader, invocationID string) (*Response, error) {
	b, err := read(r)
	if err != nil {
		return nil, err
	}
	if err = shape(b, []string{"protocol", "invocation_id", "status"}, []string{"result", "error", "outputs"}); err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err = json.Unmarshal(b, &raw); err != nil {
		return nil, err
	}
	if err = fileShapes(b, "outputs"); err != nil {
		return nil, err
	}
	var v Response
	if err = json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	if v.Protocol != Protocol || !idRE.MatchString(invocationID) || v.InvocationID != invocationID {
		return nil, errors.New("invalid response identity")
	}
	_, hasResult := raw["result"]
	_, hasError := raw["error"]
	switch v.Status {
	case "ok":
		if !hasResult || hasError {
			return nil, errors.New("ok requires only result")
		}
		if err = value(v.Result); err != nil {
			return nil, fmt.Errorf("result: %w", err)
		}
	case "error":
		if hasResult || !hasError || v.Error == nil || len(v.Outputs) > 0 {
			return nil, errors.New("error requires only error and no output files")
		}
		if err = shape(raw["error"], []string{"code", "message"}, nil); err != nil {
			return nil, err
		}
		if !nameRE.MatchString(v.Error.Code) || strings.TrimSpace(v.Error.Message) == "" || len(v.Error.Message) > 4096 {
			return nil, errors.New("invalid error code or message")
		}
	default:
		return nil, errors.New("invalid response status")
	}
	if err = files(v.Outputs); err != nil {
		return nil, err
	}
	return &v, nil
}

// EncodeRequest validates and returns deterministic JSON for one request.
func EncodeRequest(v Request, operation string, slots []string) ([]byte, error) {
	if len(v.Protocol) > 64 || len(v.InvocationID) > 128 {
		return nil, errors.New("identity exceeds byte limit")
	}
	if err := files(v.Inputs); err != nil {
		return nil, err
	}
	if len(v.Operation) > 63 || len(v.Parameters) > MaxJSONBytes {
		return nil, errors.New("request exceeds field limit")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if _, err = DecodeRequest(bytes.NewReader(b), operation, slots); err != nil {
		return nil, err
	}
	return b, nil
}

// EncodeResponse validates and returns deterministic JSON for one response.
func EncodeResponse(v Response, invocationID string) ([]byte, error) {
	if len(v.Protocol) > 64 || len(v.InvocationID) > 128 || len(v.Status) > 5 || len(v.Result) > MaxJSONBytes {
		return nil, errors.New("response exceeds field limit")
	}
	if err := files(v.Outputs); err != nil {
		return nil, err
	}
	if v.Error != nil && (len(v.Error.Code) > 63 || len(v.Error.Message) > 4096) {
		return nil, errors.New("error exceeds field limit")
	}
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if _, err = DecodeResponse(bytes.NewReader(b), invocationID); err != nil {
		return nil, err
	}
	return b, nil
}

func read(r io.Reader) ([]byte, error) {
	b, err := io.ReadAll(io.LimitReader(r, MaxJSONBytes+1))
	if err != nil {
		return nil, err
	}
	if err = jsonstrict.ValidateJSON(b, MaxJSONBytes); err != nil {
		return nil, err
	}
	return b, nil
}

func shape(b []byte, required, optional []string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	if m == nil {
		return errors.New("expected object")
	}
	allowed := map[string]bool{}
	for _, k := range required {
		allowed[k] = true
		if v, ok := m[k]; !ok || bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("missing or null %s", k)
		}
	}
	for _, k := range optional {
		allowed[k] = true
	}
	for k, v := range m {
		if !allowed[k] {
			return fmt.Errorf("unknown field %q", k)
		}
		if k != "result" && bytes.Equal(bytes.TrimSpace(v), []byte("null")) {
			return fmt.Errorf("null %s", k)
		}
	}
	return nil
}

func value(b []byte) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.UseNumber()
	var v any
	if err := d.Decode(&v); err != nil {
		return err
	}
	return numbers(v)
}

func numbers(v any) error {
	switch x := v.(type) {
	case json.Number:
		if _, ok := jsonstrict.ParseNumber(x); !ok {
			return errors.New("number outside decimal policy")
		}
	case []any:
		for _, a := range x {
			if err := numbers(a); err != nil {
				return err
			}
		}
	case map[string]any:
		for _, a := range x {
			if err := numbers(a); err != nil {
				return err
			}
		}
	}
	return nil
}

func files(fs []File) error {
	if len(fs) > MaxFiles {
		return errors.New("too many files")
	}
	names, paths := map[string]bool{}, map[string]bool{}
	var total int64
	for _, f := range fs {
		if !nameRE.MatchString(f.Name) || names[f.Name] || paths[f.Path] {
			return errors.New("invalid or duplicate file name/path")
		}
		names[f.Name] = true
		paths[f.Path] = true
		if f.Path == "" || f.Path == "." || len(f.Path) > 256 || path.IsAbs(f.Path) || path.Clean(f.Path) != f.Path || f.Path == ".." || strings.HasPrefix(f.Path, "../") || strings.ContainsAny(f.Path, "\\\x00") || !utf8.ValidString(f.Path) {
			return errors.New("invalid relative file path")
		}
		if f.MediaType == "" || len(f.MediaType) > 128 || strings.ContainsAny(f.MediaType, "\r\n\x00") || !utf8.ValidString(f.MediaType) {
			return errors.New("invalid media type")
		}
		if f.Bytes < 0 || f.Bytes > MaxFileBytes || !hashRE.MatchString(f.SHA256) {
			return errors.New("invalid file length or SHA-256")
		}
		total += f.Bytes
		if total > MaxFileBytes {
			return errors.New("file bytes exceed ceiling")
		}
	}
	return nil
}

func fileShapes(b []byte, key string) error {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	raw, ok := m[key]
	if !ok {
		return nil
	}
	var fs []json.RawMessage
	if err := json.Unmarshal(raw, &fs); err != nil {
		return err
	}
	for _, f := range fs {
		if err := shape(f, []string{"name", "path", "media_type", "bytes", "sha256"}, nil); err != nil {
			return err
		}
	}
	return nil
}
