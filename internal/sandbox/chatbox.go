package sandbox

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// ChatBoxCredentialTTL bounds each chat process's proxy grant. A process
// must stop by this time even though its container and volumes persist.
const ChatBoxCredentialTTL = 55 * time.Minute

// ChatBoxAttachRequest describes one Orbit-managed Claude process in a
// conversation's isolated persistent container.
type ChatBoxAttachRequest struct {
	RunID string        `json:"run_id"`
	Exec  AttachRequest `json:"exec"`
}

func (r ChatBoxAttachRequest) Validate() error {
	if !ValidRunID(r.RunID) {
		return fmt.Errorf("invalid run id %q", r.RunID)
	}
	return r.Exec.Validate()
}

// EnsureChatBox creates or starts a persistent per-conversation container.
// It has only its own home and work volumes, with no account mounts.
func (c *Client) EnsureChatBox(ctx context.Context, name string) (BoxStatus, error) {
	var st BoxStatus
	if !ValidChatBoxName(name) {
		return st, fmt.Errorf("invalid chat box name %q", name)
	}
	resp, err := c.do(ctx, http.MethodPost, PathChatBoxes+"/"+name, nil, "")
	if err != nil {
		return st, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return st, readError(resp)
	}
	err = json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&st)
	return st, err
}

// ArchiveChatBox returns the chat box's /work tree.
func (c *Client) ArchiveChatBox(ctx context.Context, name string) (io.ReadCloser, error) {
	if !ValidChatBoxName(name) {
		return nil, fmt.Errorf("invalid chat box name %q", name)
	}
	resp, err := c.do(ctx, http.MethodGet, PathChatBoxes+"/"+name+"/archive", nil, "")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		_ = resp.Body.Close()
		return nil, ErrNoBox
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readError(resp)
	}
	return resp.Body, nil
}

// AttachChatBox runs one Orbit-managed process in the persistent chat box.
// sandboxd replaces any backend credential with a short-lived proxy token.
func (c *Client) AttachChatBox(ctx context.Context, name string, ar ChatBoxAttachRequest, stdin io.Reader, stdout, stderr io.Writer) (int, error) {
	if !ValidChatBoxName(name) {
		return -1, fmt.Errorf("invalid chat box name %q", name)
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
	req, err := http.NewRequest(http.MethodPost, "http://"+c.host+PathChatBoxes+"/"+name+"/attach", bytes.NewReader(body))
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
			return -1, ctxErr(ctx, fmt.Errorf("sandboxd: chat box attach: %w", err))
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
				return -1, fmt.Errorf("sandboxd: chat box attach: exit frame: %w", err)
			}
			if ex.Error != "" {
				return ex.ExitCode, fmt.Errorf("sandboxd: chat box attach: %s", ex.Error)
			}
			return ex.ExitCode, nil
		default:
			return -1, fmt.Errorf("sandboxd: chat box attach: unknown frame type %d", typ)
		}
	}
}

// CreateChatBox ensures a persistent per-conversation container and restores
// its current workspace snapshot before a run.
func (c *Client) CreateChatBox(ctx context.Context, name, runID string, archive io.Reader) (CreateResponse, error) {
	if !ValidRunID(runID) {
		return CreateResponse{}, fmt.Errorf("invalid run id %q", runID)
	}
	if _, err := c.EnsureChatBox(ctx, name); err != nil {
		return CreateResponse{}, err
	}
	if archive != nil {
		var stderr bytes.Buffer
		code, err := c.AttachChatBox(ctx, name, ChatBoxAttachRequest{RunID: runID,
			Exec: AttachRequest{Argv: []string{"tar", "-xf", "-", "-C", WorkDir}}},
			archive, io.Discard, &stderr)
		if err == nil && code != 0 {
			err = fmt.Errorf("restore chat box workspace exited %d: %s", code, stderr.String())
		}
		if err != nil {
			return CreateResponse{}, err
		}
	}
	return CreateResponse{ID: name, ExpiresAt: time.Now().Add(ChatBoxCredentialTTL)}, nil
}
