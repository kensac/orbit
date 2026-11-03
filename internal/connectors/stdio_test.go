package connectors

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/extensions"
	"github.com/kensac/orbit/internal/sandbox"
)

type fakeStdioSandbox struct {
	mu        sync.Mutex
	created   int
	destroyed int
	memory    int64
	archive   []byte
}

func (f *fakeStdioSandbox) CreateProfile(context.Context, string, string, io.Reader) (sandbox.CreateResponse, error) {
	return sandbox.CreateResponse{}, nil
}

func (f *fakeStdioSandbox) CreateExtension(_ context.Context, _ string, memory int64, archive io.Reader) (sandbox.CreateResponse, error) {
	b, err := io.ReadAll(archive)
	if err != nil {
		return sandbox.CreateResponse{}, err
	}
	f.mu.Lock()
	f.created++
	f.memory = memory
	f.archive = b
	f.mu.Unlock()
	return sandbox.CreateResponse{ID: "fixture"}, nil
}

func (f *fakeStdioSandbox) Attach(ctx context.Context, _ string, req sandbox.AttachRequest, stdin io.Reader, stdout, _ io.Writer) (int, error) {
	if len(req.Argv) != 1 || req.Env != nil || req.Argv[0] != "/work/extension/reducer" {
		return 1, io.ErrUnexpectedEOF
	}
	writer, ok := stdout.(io.WriteCloser)
	if !ok {
		return 1, io.ErrUnexpectedEOF
	}
	server := mcp.NewServer(&mcp.Implementation{Name: "fixture", Version: "1"}, nil)
	mcp.AddTool(server, &mcp.Tool{Name: "sum", Description: "untrusted description", InputSchema: map[string]any{"type": "object", "properties": map[string]any{"x": map[string]any{"type": "string"}}}},
		func(context.Context, *mcp.CallToolRequest, struct{}) (*mcp.CallToolResult, map[string]any, error) {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "read result"}}}, map[string]any{"total": "12"}, nil
		})
	session, err := server.Connect(ctx, &mcp.IOTransport{Reader: io.NopCloser(stdin), Writer: writer, MaxLineLength: stdioLineLimit}, nil)
	if err != nil {
		return 1, err
	}
	<-ctx.Done()
	_ = session.Close()
	return 0, nil
}

func (f *fakeStdioSandbox) Destroy(context.Context, string) error {
	f.mu.Lock()
	f.destroyed++
	f.mu.Unlock()
	return nil
}

func TestStdioConnectorUsesPinnedManifestAndExtensionSandbox(t *testing.T) {
	dir := stdioFixture(t)
	bundle, err := extensions.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	sb := &fakeStdioSandbox{}
	client := NewStdio("local", dir, bundle.Package.Digest, bundle.Package, sb)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	listCtx, cancelList := context.WithTimeout(ctx, 3*time.Second)
	tools, err := client.ListTools(listCtx)
	cancelList()
	if err != nil {
		t.Fatal(err)
	}
	if len(tools) != 1 || tools[0].Name != "sum" || tools[0].Description != bundle.Package.Manifest.Operations[0].Description || string(tools[0].InputSchema) != `{"additionalProperties":false,"properties":{},"type":"object"}` {
		t.Fatalf("tools = %+v", tools)
	}
	result, err := client.Call(ctx, "sum", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(result.Raw), `"total":"12"`) || !strings.Contains(string(result.Content), "read result") {
		t.Fatalf("result = %s", result.Raw)
	}
	client.Close()
	sb.mu.Lock()
	defer sb.mu.Unlock()
	if sb.created != 1 || sb.destroyed != 1 || sb.memory != 16<<20 || len(sb.archive) == 0 {
		t.Fatalf("sandbox lifecycle = created %d destroyed %d memory %d archive %d bytes", sb.created, sb.destroyed, sb.memory, len(sb.archive))
	}
}

func TestStdioConnectorRejectsChangedPackageBeforeSandboxStart(t *testing.T) {
	dir := stdioFixture(t)
	bundle, err := extensions.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reducer"), append(bundle.Files["reducer"].Content, 0), 0o755); err != nil {
		t.Fatal(err)
	}
	sb := &fakeStdioSandbox{}
	client := NewStdio("local", dir, bundle.Package.Digest, bundle.Package, sb)
	if _, err := client.ListTools(context.Background()); err == nil || !strings.Contains(err.Error(), "changed from its deployment pin") {
		t.Fatalf("ListTools error = %v", err)
	}
	if sb.created != 0 {
		t.Fatalf("created %d sandboxes for changed bytes", sb.created)
	}
}

func stdioFixture(t *testing.T) string {
	t.Helper()
	src := filepath.Join("..", "extensions", "testdata", "reducer")
	manifest, err := os.ReadFile(filepath.Join(src, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(manifest, &m); err != nil {
		t.Fatal(err)
	}
	ops := m["operations"].([]any)
	op := ops[0].(map[string]any)
	op["kind"] = "tool"
	delete(op, "inputs")
	manifest, err = json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o644); err != nil {
		t.Fatal(err)
	}
	entry, err := os.ReadFile(filepath.Join(src, "reducer"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "reducer"), entry, 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}
