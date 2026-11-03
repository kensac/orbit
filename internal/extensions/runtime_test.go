package extensions

import (
	"archive/tar"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/extensionproto"
	"github.com/kensac/orbit/internal/sandbox"
)

type fakeExtensionSandbox struct {
	created, destroyed bool
	memory             int64
	timeout            bool
	directories        map[string]bool
}

func (f *fakeExtensionSandbox) CreateExtension(_ context.Context, _ string, memory int64, archive io.Reader) (sandbox.CreateResponse, error) {
	f.created, f.memory = true, memory
	tr := tar.NewReader(archive)
	f.directories = map[string]bool{}
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return sandbox.CreateResponse{}, err
		}
		if h.Typeflag == tar.TypeDir {
			f.directories[h.Name] = true
		}
	}
	return sandbox.CreateResponse{ID: "container123"}, nil
}
func (f *fakeExtensionSandbox) Attach(_ context.Context, _ string, req sandbox.AttachRequest, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	if f.timeout {
		time.Sleep(1100 * time.Millisecond)
		return 137, nil
	}
	if len(req.Argv) != 1 || req.Argv[0] != sandbox.ExtensionWorkDir+"/extension/reducer" || req.Env != nil {
		return 1, io.ErrUnexpectedEOF
	}
	request, err := extensionproto.DecodeRequest(stdin, "sum", []string{"transactions"})
	if err != nil {
		return 1, err
	}
	response, err := json.Marshal(extensionproto.Response{Protocol: extensionproto.Protocol, InvocationID: request.InvocationID, Status: "ok", Result: json.RawMessage(`{"total":"3"}`)})
	if err != nil {
		return 1, err
	}
	_, err = stdout.Write(response)
	return 0, err
}
func (f *fakeExtensionSandbox) Destroy(context.Context, string) error { f.destroyed = true; return nil }

func TestInvokeUsesPinnedPackageAndDestroysSandbox(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"manifest.json", "reducer"} {
		b, err := os.ReadFile(filepath.Join("testdata", "reducer", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	bundle, err := ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fake := &fakeExtensionSandbox{}
	got, err := Invoke(context.Background(), fake, Invocation{PackageID: "daily-total", Directory: dir, Digest: bundle.Package.Digest, Operation: "sum", Parameters: json.RawMessage(`{}`), Inputs: map[string]Input{"transactions": {MediaType: "application/json", Content: []byte(`[]`)}}})
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"total":"3"}` || !fake.created || !fake.destroyed || fake.memory != 16<<20 || !fake.directories["extension/"] || !fake.directories["inputs/"] {
		t.Fatalf("result %s, sandbox created=%t destroyed=%t memory=%d", got, fake.created, fake.destroyed, fake.memory)
	}
	if _, err := Invoke(context.Background(), fake, Invocation{PackageID: "daily-total", Directory: dir, Digest: "wrong", Operation: "sum"}); err == nil {
		t.Fatal("accepted package digest mismatch")
	}
	timeoutFake := &fakeExtensionSandbox{timeout: true}
	if _, err := Invoke(context.Background(), timeoutFake, Invocation{PackageID: "daily-total", Directory: dir, Digest: bundle.Package.Digest, Operation: "sum", Parameters: json.RawMessage(`{}`), Inputs: map[string]Input{"transactions": {MediaType: "application/json", Content: []byte(`[]`)}}}); err == nil || err.Error() != "extension daily-total operation sum timed out" {
		t.Fatalf("late nonzero exit error = %v, want timeout", err)
	}
}
