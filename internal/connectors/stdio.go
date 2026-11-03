package connectors

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"slices"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/kensac/orbit/internal/extensions"
	"github.com/kensac/orbit/internal/sandbox"
)

const stdioLineLimit = 1 << 20

// StdioExecutor is the extension sandbox surface used by one pinned MCP server.
type StdioExecutor interface {
	CreateProfile(context.Context, string, string, io.Reader) (sandbox.CreateResponse, error)
	Attach(context.Context, string, sandbox.AttachRequest, io.Reader, io.Writer, io.Writer) (int, error)
	Destroy(context.Context, string) error
}

type extensionCreator interface {
	CreateExtension(context.Context, string, int64, io.Reader) (sandbox.CreateResponse, error)
}

// ToolClient is one run's connector session.
type ToolClient interface {
	ListTools(context.Context) ([]Tool, error)
	Call(context.Context, string, json.RawMessage) (*Result, error)
	Close()
}

// StdioClient runs a package-pinned, networkless MCP server in sandboxd.
// The manifest, not the server's live tool metadata, supplies the schema and
// description offered to the model.
type StdioClient struct {
	Name      string
	Directory string
	Digest    string
	Package   *extensions.Package
	Sandbox   StdioExecutor

	mu      sync.Mutex
	session *mcp.ClientSession
}

// NewStdio returns a client for an operator-pinned stdio package.
func NewStdio(name, directory, digest string, pkg *extensions.Package, sb StdioExecutor) *StdioClient {
	return &StdioClient{Name: name, Directory: directory, Digest: digest, Package: pkg, Sandbox: sb}
}

func (c *StdioClient) connect(ctx context.Context) (*mcp.ClientSession, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		return c.session, nil
	}
	if c.Sandbox == nil || c.Package == nil || c.Package.Manifest == nil {
		return nil, errors.New("stdio connector sandbox or package is unavailable")
	}
	bundle, err := extensions.ReadDir(c.Directory)
	if err != nil {
		return nil, fmt.Errorf("connector %s: read package: %w", c.Name, err)
	}
	if bundle.Package.Digest != c.Digest || bundle.Package.Manifest.ID != c.Package.Manifest.ID {
		return nil, fmt.Errorf("connector %s: package changed from its deployment pin", c.Name)
	}
	if err := extensions.ValidateStdioServer(bundle.Package); err != nil {
		return nil, fmt.Errorf("connector %s: %w", c.Name, err)
	}
	archive, err := extensions.PackageArchive(bundle)
	if err != nil {
		return nil, fmt.Errorf("connector %s: archive package: %w", c.Name, err)
	}
	runID, err := stdioRunID()
	if err != nil {
		return nil, err
	}
	memoryBytes := int64(256 << 20)
	for _, op := range bundle.Package.Manifest.Operations {
		if op.Limits.MemoryBytes < memoryBytes {
			memoryBytes = op.Limits.MemoryBytes
		}
	}
	creator, ok := c.Sandbox.(extensionCreator)
	if !ok {
		return nil, errors.New("stdio connector sandbox does not support extension memory limits")
	}
	created, err := creator.CreateExtension(ctx, runID, memoryBytes, archive)
	if err != nil {
		return nil, fmt.Errorf("connector %s sandbox: %w", c.Name, err)
	}
	transport := &stdioTransport{executor: c.Sandbox, id: created.ID, entry: "/work/extension/" + bundle.Package.Manifest.Entrypoint}
	client := mcp.NewClient(&mcp.Implementation{Name: "orbit", Version: "1"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		_ = transport.Close()
		return nil, fmt.Errorf("connector %s: connect: %w", c.Name, err)
	}
	c.session = session
	return session, nil
}

// ListTools requires the running process to advertise exactly its pinned tool
// names. Its descriptions and schemas never override the package manifest.
func (c *StdioClient) ListTools(ctx context.Context) ([]Tool, error) {
	session, err := c.connect(ctx)
	if err != nil {
		return nil, err
	}
	declared := map[string]*extensions.Operation{}
	for i := range c.Package.Manifest.Operations {
		op := &c.Package.Manifest.Operations[i]
		declared[op.Name] = op
	}
	seen := map[string]bool{}
	for tool, err := range session.Tools(ctx, nil) {
		if err != nil {
			c.drop()
			return nil, fmt.Errorf("connector %s: list tools: %w", c.Name, err)
		}
		if len(seen) >= MaxTools {
			c.drop()
			return nil, fmt.Errorf("connector %s: more than %d tools", c.Name, MaxTools)
		}
		if _, ok := declared[tool.Name]; !ok || seen[tool.Name] {
			c.drop()
			return nil, fmt.Errorf("connector %s: server advertised undeclared or duplicate tool %q", c.Name, tool.Name)
		}
		seen[tool.Name] = true
	}
	if len(seen) != len(declared) {
		c.drop()
		return nil, fmt.Errorf("connector %s: server tool list does not match its pinned manifest", c.Name)
	}
	names := make([]string, 0, len(declared))
	for name := range declared {
		names = append(names, name)
	}
	slices.Sort(names)
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		op := declared[name]
		schema, _ := json.Marshal(op.InputSchema)
		out = append(out, Tool{Name: name, Description: op.Description, InputSchema: schema, ReadOnly: true})
	}
	return out, nil
}

// Call validates parameters and the structured result against the pinned
// manifest. Unstructured or oversized server responses are refused.
func (c *StdioClient) Call(ctx context.Context, tool string, args json.RawMessage) (*Result, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, CallTimeout)
		defer cancel()
	}
	var op *extensions.Operation
	for i := range c.Package.Manifest.Operations {
		if c.Package.Manifest.Operations[i].Name == tool {
			op = &c.Package.Manifest.Operations[i]
			break
		}
	}
	if op == nil {
		return nil, fmt.Errorf("connector %s: tool %q is not pinned", c.Name, tool)
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if int64(len(args)) > op.Limits.InputBytes {
		return nil, fmt.Errorf("connector %s tool %s: parameters exceed its manifest limit", c.Name, tool)
	}
	if err := extensions.ValidateInstance(op.InputSchema, args); err != nil {
		return nil, fmt.Errorf("connector %s tool %s parameters: %w", c.Name, tool, err)
	}
	callCtx, cancel := context.WithTimeout(ctx, time.Duration(op.Limits.TimeoutMS)*time.Millisecond)
	defer cancel()
	session, err := c.connect(callCtx)
	if err != nil {
		return nil, err
	}
	var parameters any
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.UseNumber()
	if err := dec.Decode(&parameters); err != nil {
		return nil, err
	}
	res, err := session.CallTool(callCtx, &mcp.CallToolParams{Name: tool, Arguments: parameters})
	if err != nil {
		c.drop()
		return nil, fmt.Errorf("connector %s tool %s: %w", c.Name, tool, err)
	}
	if res.StructuredContent == nil {
		c.drop()
		return nil, fmt.Errorf("connector %s tool %s: structured content is required", c.Name, tool)
	}
	structured, err := json.Marshal(res.StructuredContent)
	if err != nil || int64(len(structured)) > op.Limits.OutputBytes {
		c.drop()
		return nil, fmt.Errorf("connector %s tool %s: result exceeds its manifest limit", c.Name, tool)
	}
	if err := extensions.ValidateInstance(op.OutputSchema, structured); err != nil {
		c.drop()
		return nil, fmt.Errorf("connector %s tool %s result: %w", c.Name, tool, err)
	}
	raw, err := json.Marshal(res)
	if err != nil || len(raw) > MaxResponse {
		c.drop()
		return nil, fmt.Errorf("connector %s tool %s: result exceeds the response limit", c.Name, tool)
	}
	content, err := json.Marshal(res.Content)
	if err != nil {
		return nil, err
	}
	return &Result{Content: content, IsError: res.IsError, Raw: raw}, nil
}

// Close ends the MCP session and destroys its temporary sandbox.
func (c *StdioClient) Close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.session != nil {
		_ = c.session.Close()
		c.session = nil
	}
}

func (c *StdioClient) drop() {
	c.mu.Lock()
	s := c.session
	c.session = nil
	c.mu.Unlock()
	if s != nil {
		_ = s.Close()
	}
}

type stdioTransport struct {
	executor StdioExecutor
	id       string
	entry    string
	mu       sync.Mutex
	conn     *stdioConnection
}

func (t *stdioTransport) Connect(ctx context.Context) (mcp.Connection, error) {
	stdinR, stdinW := io.Pipe()
	stdoutR, stdoutW := io.Pipe()
	// The listing/call context is only for MCP initialization. A connector
	// session must survive its caller's short listing or operation timeout.
	attachCtx, cancel := context.WithCancel(context.Background())
	stopOnSetupCancel := context.AfterFunc(ctx, cancel)
	stderr := &stdioStderr{}
	done := make(chan error, 1)
	go func() {
		_, err := t.executor.Attach(attachCtx, t.id, sandbox.AttachRequest{Argv: []string{t.entry}}, stdinR, stdoutW, stderr)
		_ = stdinR.Close()
		_ = stdoutW.CloseWithError(err)
		done <- err
	}()
	base, err := (&mcp.IOTransport{Reader: stdoutR, Writer: stdinW, MaxLineLength: stdioLineLimit}).Connect(ctx)
	if err != nil {
		stopOnSetupCancel()
		cancel()
		_ = stdinW.Close()
		_ = stdoutR.Close()
		cleanupStdio(t.executor, t.id)
		return nil, err
	}
	if !stopOnSetupCancel() && ctx.Err() != nil {
		cancel()
		_ = base.Close()
		cleanupStdio(t.executor, t.id)
		return nil, ctx.Err()
	}
	conn := &stdioConnection{Connection: base, executor: t.executor, id: t.id, cancel: cancel, done: done}
	t.mu.Lock()
	t.conn = conn
	t.mu.Unlock()
	return conn, nil
}

func (t *stdioTransport) Close() error {
	t.mu.Lock()
	conn := t.conn
	t.mu.Unlock()
	if conn != nil {
		return conn.Close()
	}
	cleanupStdio(t.executor, t.id)
	return nil
}

type stdioConnection struct {
	mcp.Connection
	executor StdioExecutor
	id       string
	cancel   context.CancelFunc
	done     chan error
	once     sync.Once
	err      error
}

func (c *stdioConnection) Close() error {
	c.once.Do(func() {
		c.cancel()
		c.err = c.Connection.Close()
		cleanupStdio(c.executor, c.id)
		select {
		case <-c.done:
		case <-time.After(5 * time.Second):
		}
	})
	return c.err
}

type stdioStderr struct {
	mu sync.Mutex
	n  int
}

func (s *stdioStderr) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(p) > 64<<10-s.n {
		return 0, errors.New("stdio connector stderr exceeds 64 KiB")
	}
	s.n += len(p)
	return len(p), nil
}

func cleanupStdio(sb StdioExecutor, id string) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = sb.Destroy(ctx, id)
}

func stdioRunID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", fmt.Errorf("connector sandbox id: %w", err)
	}
	return "connector-" + hex.EncodeToString(b[:]), nil
}
