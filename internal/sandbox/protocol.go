// Package sandbox is the contract between orbit and `orbit sandboxd`, the
// service that alone holds a docker socket and runs build containers. It
// holds the wire protocol both sides share and the client orbit uses.
//
// sandboxd exposes four operations on build sandboxes, and none takes a
// docker option (agents' boxes are box.go's):
//
//	POST   /v1/sandboxes?run_id=<id>   body: the workdir as a tar; creates and starts a container
//	POST   /v1/sandboxes/{id}/attach   body: AttachRequest; upgrades to a framed stdio stream
//	GET    /v1/sandboxes/{id}/archive  the workdir as a tar
//	DELETE /v1/sandboxes/{id}          kills and removes the container and its volume
//
// Every request carries `Authorization: Bearer <ORBIT_SANDBOX_TOKEN>`.
package sandbox

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/kensac/orbit/internal/claudeproto"
)

// Paths and the upgrade token of the attach stream.
const (
	PathSandboxes = "/v1/sandboxes"
	// PathHealthz and PathReadyz are sandboxd's probes, the only routes without the token: they reveal no sandbox and change nothing.
	PathHealthz   = "/healthz"
	PathReadyz    = "/readyz"
	AttachUpgrade = "orbit-sandbox-attach"
)

// WorkDir is where the archive is unpacked inside the container, and the
// only writable path that outlives the process.
const WorkDir = "/work"

// ExtensionWorkDir is a separate volume target that is absent from the runner
// image. Docker creates it root-owned, so the fixed unprivileged extension
// identity cannot add files beside its read-only package and inputs.
const ExtensionWorkDir = "/orbit-extension"

// Limits both sides hold. MaxArchiveBytes bounds the workdir in either
// direction: orbit's own checkout is a few MB, so 256 MiB leaves room for a
// repository with assets and still stops a sandbox from filling sandboxd's
// disk with its readback.
const (
	MaxArchiveBytes = 256 << 20
	// MaxClaudeConfigBytes bounds the laptop-to-box Claude config bundle; it
	// is enough for a large skill library without copying an entire home.
	MaxClaudeConfigBytes = 32 << 20
	// MaxArgs and MaxArgvBytes bound an attach command line: the CLI's
	// largest argument is the output schema, tens of KB.
	MaxArgs      = 256
	MaxArgvBytes = 1 << 20
	// MaxEnvValue bounds one credential value.
	MaxEnvValue = 16 << 10
	// MaxFrame is the largest frame either side accepts. sandboxd writes at
	// most frameChunk bytes per frame.
	MaxFrame   = 1 << 20
	frameChunk = 32 << 10
)

// AllowedEnv is every variable a caller may send for an attached process: the backend credentials the CLI can reach its own backend with (AGENTS.md rule 2), and nothing else. A caller sends at most one of them (build.SandboxEnv); sandboxd sets the rest of the environment itself. A sandbox's process never receives the value: sandboxd puts a credential proxy token in its place (internal/credproxy).
var AllowedEnv = map[string]bool{claudeproto.OAuthToken: true, claudeproto.APIKey: true}

// CreateResponse is the answer to a create.
type CreateResponse struct {
	ID        string    `json:"id"`
	ExpiresAt time.Time `json:"expires_at"`
}

// AttachRequest starts one process in a sandbox: an argv and the allowed
// environment. There is deliberately no field for a user, a working
// directory, a TTY or privileges; sandboxd fixes them.
type AttachRequest struct {
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env,omitempty"`
}

// Validate is the check sandboxd applies; the client applies it too so a
// bad request fails before it is sent.
func (r AttachRequest) Validate() error {
	if len(r.Argv) == 0 || r.Argv[0] == "" {
		return errors.New("argv is empty")
	}
	if len(r.Argv) > MaxArgs {
		return fmt.Errorf("argv has %d arguments, more than %d", len(r.Argv), MaxArgs)
	}
	n := 0
	for _, a := range r.Argv {
		if strings.IndexByte(a, 0) >= 0 {
			return errors.New("argv contains a NUL byte")
		}
		n += len(a)
	}
	if n > MaxArgvBytes {
		return fmt.Errorf("argv is %d bytes, more than %d", n, MaxArgvBytes)
	}
	for k, v := range r.Env {
		if !AllowedEnv[k] {
			return fmt.Errorf("env %q is not allowed", k)
		}
		if strings.IndexByte(v, 0) >= 0 || len(v) > MaxEnvValue {
			return fmt.Errorf("env %q has an invalid value", k)
		}
	}
	if len(r.Env) > 1 {
		return errors.New("env carries more than one backend credential")
	}
	return nil
}

// Exit is the last frame of an attach stream.
type Exit struct {
	ExitCode int    `json:"exit_code"`
	Error    string `json:"error,omitempty"`
}

var (
	idRE    = regexp.MustCompile(`^[0-9a-f]{64}$`)
	runIDRE = regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`)
)

// ValidID reports whether s has the shape of a sandbox id (a container id).
func ValidID(s string) bool { return idRE.MatchString(s) }

// Profiles a create may name (POST /v1/sandboxes?profile=): "build" (the
// default, the build egress allowlist) and "workstation" (a chat session's
// own box: egress to any public host, through sandboxd's workstation
// proxy). sandboxd decides what each means; a caller only names one.
const (
	ProfileBuild       = "build"
	ProfileWorkstation = "workstation"
	ProfileExtension   = "extension"
)

// ValidProfile reports whether p names a profile ("" is build).
func ValidProfile(p string) bool {
	return p == "" || p == ProfileBuild || p == ProfileWorkstation || p == ProfileExtension
}

// ValidRunID reports whether s is an acceptable run id label.
func ValidRunID(s string) bool { return runIDRE.MatchString(s) }

// Frame types of the attach stream (sandboxd to orbit). orbit to sandboxd is
// the process's stdin, unframed, ended by a half-close.
const (
	FrameStdout byte = 1
	FrameStderr byte = 2
	FrameExit   byte = 3
)

// WriteFrame writes one frame: a type byte, three zero bytes, a big-endian
// uint32 length, the payload. Docker's own stream framing has the same shape.
func WriteFrame(w io.Writer, typ byte, p []byte) error {
	if len(p) > MaxFrame {
		return fmt.Errorf("frame of %d bytes exceeds %d", len(p), MaxFrame)
	}
	var h [8]byte
	h[0] = typ
	binary.BigEndian.PutUint32(h[4:], uint32(len(p))) // #nosec G115 -- bounded by MaxFrame above
	if _, err := w.Write(h[:]); err != nil {
		return err
	}
	_, err := w.Write(p)
	return err
}

// WriteChunked writes p as frames of at most frameChunk bytes.
func WriteChunked(w io.Writer, typ byte, p []byte) error {
	for len(p) > 0 {
		n := min(len(p), frameChunk)
		if err := WriteFrame(w, typ, p[:n]); err != nil {
			return err
		}
		p = p[n:]
	}
	return nil
}

// ReadFrame reads one frame. io.EOF means the stream ended between frames.
func ReadFrame(r io.Reader) (byte, []byte, error) {
	var h [8]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		return 0, nil, err
	}
	if h[1] != 0 || h[2] != 0 || h[3] != 0 {
		return 0, nil, errors.New("malformed frame header")
	}
	n := binary.BigEndian.Uint32(h[4:])
	if n > MaxFrame {
		return 0, nil, fmt.Errorf("frame of %d bytes exceeds %d", n, MaxFrame)
	}
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, fmt.Errorf("truncated frame: %w", err)
	}
	return h[0], p, nil
}

// Readiness is sandboxd's /readyz body (200 when Ready, 503 when not). Checks maps "docker", "network" and "proxy:<host>" to "ok" or the failure.
type Readiness struct {
	Ready  bool              `json:"ready"`
	Checks map[string]string `json:"checks"`
}

// ProxyCheck is the Checks key of the egress proxy at host.
func ProxyCheck(host string) string { return "proxy:" + host }

// Proxies returns each proxy check's host and whether it passed.
func (r Readiness) Proxies() map[string]bool {
	out := map[string]bool{}
	for k, v := range r.Checks {
		if host, ok := strings.CutPrefix(k, "proxy:"); ok {
			out[host] = v == "ok"
		}
	}
	return out
}
