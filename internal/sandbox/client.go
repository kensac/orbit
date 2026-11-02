package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Client speaks to sandboxd. It holds the sandbox token and nothing a
// sandbox could use: it is orbit's side of the boundary.
type Client struct {
	token string
	dial  func(ctx context.Context) (net.Conn, error)
	hc    *http.Client
	host  string
}

// NewClient returns a client for sandboxd at addr: "unix:/path/to.sock" or
// "host:port" (a tailnet address).
func NewClient(addr, token string) (*Client, error) {
	if token == "" {
		return nil, errors.New("sandbox token is empty")
	}
	var network, address string
	switch {
	case strings.HasPrefix(addr, "unix:"):
		network, address = "unix", strings.TrimPrefix(addr, "unix:")
	case addr != "":
		network, address = "tcp", addr
	default:
		return nil, errors.New("sandboxd address is empty")
	}
	d := &net.Dialer{Timeout: 10 * time.Second}
	dial := func(ctx context.Context) (net.Conn, error) { return d.DialContext(ctx, network, address) }
	tr := &http.Transport{
		DialContext:     func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) },
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{token: token, dial: dial, hc: &http.Client{Transport: tr}, host: "sandboxd"}, nil
}

// Error is a refusal from sandboxd.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	return fmt.Sprintf("sandboxd: %d %s", e.Status, e.Message)
}

func (c *Client) do(ctx context.Context, method, path string, body io.Reader, ctype string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, "http://"+c.host+path, body)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	return c.hc.Do(req)
}

func readError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var e struct {
		Error string `json:"error"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &e) == nil && e.Error != "" {
		msg = e.Error
	}
	return &Error{Status: resp.StatusCode, Message: msg}
}

// Create starts a build sandbox for runID with archive (a tar) as its
// workdir.
func (c *Client) Create(ctx context.Context, runID string, archive io.Reader) (CreateResponse, error) {
	return c.CreateProfile(ctx, runID, "", archive)
}

// CreateProfile starts a sandbox of a profile (ValidProfile; "" is the
// build one).
func (c *Client) CreateProfile(ctx context.Context, runID, profile string, archive io.Reader) (CreateResponse, error) {
	if !ValidRunID(runID) {
		return CreateResponse{}, fmt.Errorf("invalid run id %q", runID)
	}
	if !ValidProfile(profile) {
		return CreateResponse{}, fmt.Errorf("unknown sandbox profile %q", profile)
	}
	q := "?run_id=" + url.QueryEscape(runID)
	if profile != "" {
		q += "&profile=" + url.QueryEscape(profile)
	}
	resp, err := c.do(ctx, http.MethodPost, PathSandboxes+q, archive, "application/x-tar")
	if err != nil {
		return CreateResponse{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		return CreateResponse{}, readError(resp)
	}
	var out CreateResponse
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&out); err != nil {
		return CreateResponse{}, fmt.Errorf("sandboxd: create response: %w", err)
	}
	if !ValidID(out.ID) {
		return CreateResponse{}, fmt.Errorf("sandboxd: create returned id %q", out.ID)
	}
	return out, nil
}

// Archive returns the sandbox's workdir as a tar, entries relative to it.
// The caller closes it.
func (c *Client) Archive(ctx context.Context, id string) (io.ReadCloser, error) {
	if !ValidID(id) {
		return nil, fmt.Errorf("invalid sandbox id %q", id)
	}
	resp, err := c.do(ctx, http.MethodGet, PathSandboxes+"/"+id+"/archive", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp.Body, nil
}

// Destroy kills and removes a sandbox. A sandbox that is already gone is not
// an error.
func (c *Client) Destroy(ctx context.Context, id string) error {
	if !ValidID(id) {
		return fmt.Errorf("invalid sandbox id %q", id)
	}
	resp, err := c.do(ctx, http.MethodDelete, PathSandboxes+"/"+id, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusNotFound {
		return nil
	}
	return readError(resp)
}

// Attach runs one process in the sandbox: stdin is copied to it (and
// half-closed at EOF), its stdout and stderr to the writers. It returns the
// process's exit code once it exits. Cancelling ctx closes the stream; the
// process itself ends with the sandbox's lifetime or a Destroy. Attach may
// return before stdin is drained; the caller closes stdin when it returns.
func (c *Client) Attach(ctx context.Context, id string, ar AttachRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if !ValidID(id) {
		return -1, fmt.Errorf("invalid sandbox id %q", id)
	}
	if err := ar.Validate(); err != nil {
		return -1, err
	}
	body, err := json.Marshal(ar)
	if err != nil {
		return -1, err
	}
	conn, err := c.dial(ctx)
	if err != nil {
		return -1, err
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	req, err := http.NewRequest(http.MethodPost, "http://"+c.host+PathSandboxes+"/"+id+"/attach", bytes.NewReader(body))
	if err != nil {
		return -1, err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", AttachUpgrade)
	if err := req.Write(conn); err != nil {
		return -1, ctxErr(ctx, err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return -1, ctxErr(ctx, err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer resp.Body.Close()
		return -1, readError(resp)
	}

	go func() {
		if stdin != nil {
			_, _ = io.Copy(conn, stdin)
		}
		if cw, ok := conn.(interface{ CloseWrite() error }); ok {
			_ = cw.CloseWrite()
		}
	}()

	for {
		typ, p, err := ReadFrame(br)
		if err != nil {
			if errors.Is(err, io.EOF) {
				err = errors.New("stream ended without an exit status")
			}
			return -1, ctxErr(ctx, fmt.Errorf("sandboxd: attach: %w", err))
		}
		switch typ {
		case FrameStdout:
			if stdout != nil {
				if _, err := stdout.Write(p); err != nil {
					return -1, err
				}
			}
		case FrameStderr:
			if stderr != nil {
				if _, err := stderr.Write(p); err != nil {
					return -1, err
				}
			}
		case FrameExit:
			var ex Exit
			if err := json.Unmarshal(p, &ex); err != nil {
				return -1, fmt.Errorf("sandboxd: attach: exit frame: %w", err)
			}
			if ex.Error != "" {
				return ex.ExitCode, fmt.Errorf("sandboxd: attach: %s", ex.Error)
			}
			return ex.ExitCode, nil
		default:
			return -1, fmt.Errorf("sandboxd: attach: unknown frame type %d", typ)
		}
	}
}

func ctxErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}
