package extensions

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"time"

	"github.com/kensac/orbit/internal/extensionproto"
	"github.com/kensac/orbit/internal/sandbox"
)

// Input is a fetched source made available to one declared extension slot.
type Input struct {
	MediaType string
	Content   []byte
}

// Invocation names one operator-pinned source operation and its inputs.
type Invocation struct {
	PackageID  string
	Directory  string
	Digest     string
	Operation  string
	Parameters json.RawMessage
	Inputs     map[string]Input
}

// SandboxExecutor is the restricted sandboxd surface used for one invocation.
type SandboxExecutor interface {
	CreateExtension(context.Context, string, int64, io.Reader) (sandbox.CreateResponse, error)
	Attach(context.Context, string, sandbox.AttachRequest, io.Reader, io.Writer, io.Writer) (int, error)
	Destroy(context.Context, string) error
}

// Invoke runs one native source operation in sandboxd's isolated profile.
func Invoke(ctx context.Context, sb SandboxExecutor, in Invocation) (json.RawMessage, error) {
	if sb == nil {
		return nil, errors.New("extension sandboxd is unavailable")
	}
	bundle, err := ReadDir(in.Directory)
	if err != nil {
		return nil, fmt.Errorf("extension %s: read package: %w", in.PackageID, err)
	}
	pkg := bundle.Package
	if pkg.Manifest.ID != in.PackageID || pkg.Digest != in.Digest {
		return nil, fmt.Errorf("extension %s package digest or id changed", in.PackageID)
	}
	var op *Operation
	for i := range pkg.Manifest.Operations {
		if pkg.Manifest.Operations[i].Name == in.Operation {
			op = &pkg.Manifest.Operations[i]
			break
		}
	}
	if op == nil || op.Kind != "source" {
		return nil, fmt.Errorf("extension %s operation %s is not a pinned source operation", in.PackageID, in.Operation)
	}
	if len(in.Parameters) == 0 {
		in.Parameters = json.RawMessage("{}")
	}
	if err := ValidateInstance(op.InputSchema, in.Parameters); err != nil {
		return nil, fmt.Errorf("extension %s operation %s parameters: %w", in.PackageID, in.Operation, err)
	}
	if len(in.Inputs) != len(op.Inputs) {
		return nil, errors.New("extension input slots do not match the manifest")
	}
	slots := append([]string(nil), op.Inputs...)
	sort.Strings(slots)
	var total int64
	inputFiles := make(map[string]Input, len(in.Inputs))
	var descriptors []extensionproto.File
	for _, slot := range slots {
		data, ok := in.Inputs[slot]
		if !ok {
			return nil, fmt.Errorf("extension input slot %q is missing", slot)
		}
		if len(data.Content) > extensionproto.MaxFileBytes {
			return nil, fmt.Errorf("extension input %q exceeds the file limit", slot)
		}
		total += int64(len(data.Content))
		if total > op.Limits.InputBytes {
			return nil, fmt.Errorf("extension inputs exceed the operation limit")
		}
		inputFiles[slot] = data
		descriptors = append(descriptors, extensionproto.Describe(slot, "inputs/"+slot, data.MediaType, data.Content))
	}
	invocationID, runID, err := invocationIDs()
	if err != nil {
		return nil, err
	}
	request, err := extensionproto.EncodeRequest(extensionproto.Request{Protocol: extensionproto.Protocol, InvocationID: invocationID,
		Operation: in.Operation, Parameters: in.Parameters, Inputs: descriptors}, in.Operation, slots)
	if err != nil {
		return nil, fmt.Errorf("extension request: %w", err)
	}
	archive, err := invocationArchive(bundle, inputFiles)
	if err != nil {
		return nil, err
	}
	timeout := time.Duration(op.Limits.TimeoutMS) * time.Millisecond
	callCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	created, err := sb.CreateExtension(callCtx, runID, op.Limits.MemoryBytes, archive)
	if err != nil {
		return nil, fmt.Errorf("extension %s sandbox: %w", in.PackageID, err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 30*time.Second)
		defer stop()
		_ = sb.Destroy(cleanup, created.ID)
	}()
	stdout := &boundedBuffer{max: extensionproto.MaxJSONBytes}
	stderr := &boundedBuffer{max: 64 << 10}
	exit, err := sb.Attach(callCtx, created.ID, sandbox.AttachRequest{Argv: []string{"/work/extension/" + pkg.Manifest.Entrypoint}},
		bytes.NewReader(request), stdout, stderr)
	if err != nil {
		if errors.Is(callCtx.Err(), context.DeadlineExceeded) {
			return nil, fmt.Errorf("extension %s operation %s timed out", in.PackageID, in.Operation)
		}
		return nil, fmt.Errorf("extension %s operation %s: %w", in.PackageID, in.Operation, err)
	}
	if exit != 0 {
		return nil, fmt.Errorf("extension %s operation %s exited with code %d", in.PackageID, in.Operation, exit)
	}
	response, err := extensionproto.DecodeResponse(bytes.NewReader(stdout.Bytes()), invocationID)
	if err != nil {
		return nil, fmt.Errorf("extension %s operation %s response: %w", in.PackageID, in.Operation, err)
	}
	if response.Status != "ok" {
		return nil, fmt.Errorf("extension %s operation %s failed with code %s", in.PackageID, in.Operation, response.Error.Code)
	}
	if len(response.Outputs) != 0 {
		return nil, errors.New("source operation may not return output files in this runtime")
	}
	if int64(len(response.Result)) > op.Limits.OutputBytes {
		return nil, errors.New("extension result exceeds the operation output limit")
	}
	if err := ValidateInstance(op.OutputSchema, response.Result); err != nil {
		return nil, fmt.Errorf("extension %s operation %s result: %w", in.PackageID, in.Operation, err)
	}
	return response.Result, nil
}

func invocationIDs() (string, string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", "", fmt.Errorf("extension invocation id: %w", err)
	}
	id := hex.EncodeToString(b[:])
	return "inv_" + id, "ext-" + id, nil
}

func invocationArchive(bundle *Bundle, inputs map[string]Input) (io.Reader, error) {
	var out bytes.Buffer
	tw := tar.NewWriter(&out)
	write := func(name string, body []byte, mode int64) error {
		h := &tar.Header{Name: path.Clean(name), Mode: mode, Size: int64(len(body)), Typeflag: tar.TypeReg, Uid: 10001, Gid: 10001, Format: tar.FormatPAX}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		_, err := tw.Write(body)
		return err
	}
	names := make([]string, 0, len(bundle.Files))
	for name := range bundle.Files {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := write("extension/"+name, bundle.Files[name].Content, int64(bundle.Files[name].Mode.Perm())); err != nil {
			return nil, err
		}
	}
	slots := make([]string, 0, len(inputs))
	for slot := range inputs {
		slots = append(slots, slot)
	}
	sort.Strings(slots)
	for _, slot := range slots {
		if err := write("inputs/"+slot, inputs[slot].Content, 0o400); err != nil {
			return nil, err
		}
	}
	if err := tw.Close(); err != nil {
		return nil, err
	}
	if int64(out.Len()) > sandbox.MaxArchiveBytes {
		return nil, errors.New("extension invocation archive exceeds sandbox limit")
	}
	return bytes.NewReader(out.Bytes()), nil
}

type boundedBuffer struct {
	bytes.Buffer
	max int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("extension output exceeds limit")
	}
	return b.Buffer.Write(p)
}
