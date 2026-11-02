package sandboxd

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
	"strconv"
	"strings"
	"time"
)

// apiVersion is the Docker Engine API sandboxd speaks: 1.44 is Docker 25,
// the oldest release Docker 29 still accepts, and has every field
// options.go sets.
const apiVersion = "v1.44"

// Docker is a minimal Engine API client over the unix socket: only the
// calls sandboxd makes, so the surface it can exercise is this file.
type Docker struct {
	sock string
	hc   *http.Client
}

// NewDocker returns a client for the socket at path.
func NewDocker(path string) *Docker {
	d := &net.Dialer{Timeout: 10 * time.Second}
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			return d.DialContext(ctx, "unix", path)
		},
		MaxIdleConns:    8,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Docker{sock: path, hc: &http.Client{Transport: tr}}
}

// errNotFound is the daemon's 404.
var errNotFound = errors.New("not found")

type dockerError struct {
	status int
	msg    string
}

func (e *dockerError) Error() string { return fmt.Sprintf("docker: %d %s", e.status, e.msg) }

func (e *dockerError) Is(target error) bool {
	return target == errNotFound && e.status == http.StatusNotFound
}

func (d *Docker) request(ctx context.Context, method, path string, q url.Values, body io.Reader, ctype string) (*http.Response, error) {
	u := "http://docker/" + apiVersion + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	// The host is a placeholder: every request goes over the unix socket.
	req, err := http.NewRequestWithContext(ctx, method, u, body) //nolint:gosec // fixed host on the docker socket
	if err != nil {
		return nil, err
	}
	if ctype != "" {
		req.Header.Set("Content-Type", ctype)
	}
	resp, err := d.hc.Do(req) //nolint:gosec // fixed host on the docker socket
	if err != nil {
		return nil, fmt.Errorf("docker: %w", err)
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		return nil, readDockerError(resp)
	}
	return resp, nil
}

func readDockerError(resp *http.Response) error {
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	var m struct {
		Message string `json:"message"`
	}
	msg := strings.TrimSpace(string(b))
	if json.Unmarshal(b, &m) == nil && m.Message != "" {
		msg = m.Message
	}
	return &dockerError{status: resp.StatusCode, msg: msg}
}

// call sends in as JSON (when not nil) and decodes the answer into out
// (when not nil).
func (d *Docker) call(ctx context.Context, method, path string, q url.Values, in, out any) error {
	var body io.Reader
	ctype := ""
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body, ctype = bytes.NewReader(b), "application/json"
	}
	resp, err := d.request(ctx, method, path, q, body, ctype)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out == nil {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
		return nil
	}
	return json.NewDecoder(resp.Body).Decode(out)
}

type dockerInfo struct {
	Name            string              `json:"Name"`
	SecurityOptions []string            `json:"SecurityOptions"`
	Runtimes        map[string]struct{} `json:"Runtimes"`
	Architecture    string              `json:"Architecture"`
}

func (d *Docker) info(ctx context.Context) (dockerInfo, error) {
	var i dockerInfo
	return i, d.call(ctx, http.MethodGet, "/info", nil, nil, &i)
}

type networkState struct {
	Name     string `json:"Name"`
	Driver   string `json:"Driver"`
	Internal bool   `json:"Internal"`
}

func (d *Docker) network(ctx context.Context, name string) (networkState, error) {
	var n networkState
	return n, d.call(ctx, http.MethodGet, "/networks/"+url.PathEscape(name), nil, nil, &n)
}

func (d *Docker) imageExists(ctx context.Context, ref string) (bool, error) {
	err := d.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if errors.Is(err, errNotFound) {
		return false, nil
	}
	return err == nil, err
}

// pull fetches ref and reports the first error in the progress stream,
// which arrives with a 200 status.
func (d *Docker) pull(ctx context.Context, ref string) error {
	resp, err := d.request(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	dec := json.NewDecoder(resp.Body)
	for {
		var m struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&m); err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			return fmt.Errorf("docker: pull %s: %w", ref, err)
		}
		if m.Error != "" {
			return fmt.Errorf("docker: pull %s: %s", ref, m.Error)
		}
	}
}

func (d *Docker) createContainer(ctx context.Context, spec containerCreate) (string, error) {
	return d.createNamed(ctx, "", spec)
}

// createNamed creates a container with a name ("" lets docker pick one).
func (d *Docker) createNamed(ctx context.Context, name string, spec containerCreate) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	var q url.Values
	if name != "" {
		q = url.Values{"name": {name}}
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create", q, spec, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

func (d *Docker) inspect(ctx context.Context, id string) (containerState, error) {
	var c containerState
	return c, d.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &c)
}

func (d *Docker) start(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// stop stops a container, giving its processes 10 s after SIGTERM.
func (d *Docker) stop(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodPost, "/containers/"+id+"/stop", url.Values{"t": {"10"}}, nil, nil)
}

// resizeExec sets an exec's TTY size.
func (d *Docker) resizeExec(ctx context.Context, id string, cols, rows int) error {
	return d.call(ctx, http.MethodPost, "/exec/"+id+"/resize", url.Values{"h": {strconv.Itoa(rows)}, "w": {strconv.Itoa(cols)}}, nil, nil)
}

// remove kills and removes a container with its anonymous volumes.
func (d *Docker) remove(ctx context.Context, id string) error {
	return d.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {"true"}, "v": {"true"}}, nil, nil)
}

// putArchive extracts a tar into the container at dir.
func (d *Docker) putArchive(ctx context.Context, id, dir string, tarball io.Reader) error {
	resp, err := d.request(ctx, http.MethodPut, "/containers/"+id+"/archive",
		url.Values{"path": {dir}, "noOverwriteDirNonDir": {"true"}}, tarball, "application/x-tar")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	return nil
}

// getArchive returns dir as a tar whose entries are prefixed with its base
// name.
func (d *Docker) getArchive(ctx context.Context, id, dir string) (io.ReadCloser, error) {
	resp, err := d.request(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {dir}}, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

type listedContainer struct {
	ID     string            `json:"Id"`
	Labels map[string]string `json:"Labels"`
}

func labelFilter(label string) url.Values {
	f, _ := json.Marshal(map[string][]string{"label": {label}})
	return url.Values{"filters": {string(f)}}
}

// managed lists every container sandboxd created, running or not.
func (d *Docker) managed(ctx context.Context) ([]listedContainer, error) {
	q := labelFilter(LabelManaged + "=1")
	q.Set("all", "true")
	var out []listedContainer
	return out, d.call(ctx, http.MethodGet, "/containers/json", q, nil, &out)
}

// danglingVolumes lists sandboxd's volumes no container uses.
func (d *Docker) danglingVolumes(ctx context.Context) ([]string, error) {
	f, _ := json.Marshal(map[string][]string{"label": {LabelManaged + "=1"}, "dangling": {"true"}})
	var out struct {
		Volumes []struct {
			Name   string            `json:"Name"`
			Labels map[string]string `json:"Labels"`
		} `json:"Volumes"`
	}
	if err := d.call(ctx, http.MethodGet, "/volumes", url.Values{"filters": {string(f)}}, nil, &out); err != nil {
		return nil, err
	}
	var names []string
	for _, v := range out.Volumes {
		// The filter is the daemon's; the label is checked again here so
		// a filter the daemon ignored can never widen what is removed.
		if v.Labels[LabelManaged] == "1" {
			names = append(names, v.Name)
		}
	}
	return names, nil
}

func (d *Docker) removeVolume(ctx context.Context, name string) error {
	return d.call(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil)
}

func (d *Docker) createExec(ctx context.Context, id string, spec execCreate) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/"+id+"/exec", nil, spec, &out); err != nil {
		return "", err
	}
	return out.ID, nil
}

type execState struct {
	Running  bool `json:"Running"`
	ExitCode int  `json:"ExitCode"`
}

func (d *Docker) inspectExec(ctx context.Context, id string) (execState, error) {
	var e execState
	return e, d.call(ctx, http.MethodGet, "/exec/"+id+"/json", nil, nil, &e)
}

// startExec starts an exec attached and returns the hijacked connection:
// writes go to the process's stdin, reads are docker's multiplexed
// stdout/stderr frames.
func (d *Docker) startExec(ctx context.Context, id string) (net.Conn, *bufio.Reader, error) {
	return d.startExecWith(ctx, id, []byte(`{"Detach":false,"Tty":false}`))
}

// startExecTTY starts a TTY exec attached: the stream is the terminal's raw
// bytes both ways, not multiplexed.
func (d *Docker) startExecTTY(ctx context.Context, id string, cols, rows int) (net.Conn, *bufio.Reader, error) {
	body, _ := json.Marshal(map[string]any{"Detach": false, "Tty": true, "ConsoleSize": []int{rows, cols}})
	return d.startExecWith(ctx, id, body)
}

func (d *Docker) startExecWith(ctx context.Context, id string, body []byte) (net.Conn, *bufio.Reader, error) {
	var dl net.Dialer
	conn, err := dl.DialContext(ctx, "unix", d.sock)
	if err != nil {
		return nil, nil, fmt.Errorf("docker: %w", err)
	}
	req, err := http.NewRequest(http.MethodPost, "http://docker/"+apiVersion+"/exec/"+id+"/start", bytes.NewReader(body))
	if err != nil {
		_ = conn.Close()
		return nil, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "tcp")
	if err := req.Write(conn); err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("docker: exec start: %w", err)
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req) //nolint:bodyclose // an upgrade: the connection is the body, closed by the caller
	if err != nil {
		_ = conn.Close()
		return nil, nil, fmt.Errorf("docker: exec start: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols && resp.StatusCode != http.StatusOK {
		defer func() { _ = conn.Close() }()
		return nil, nil, readDockerError(resp)
	}
	return conn, br, nil
}
