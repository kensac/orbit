package sandboxd

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/extensions"
	"github.com/kensac/orbit/internal/sandbox"
)

// baseImage is the integration test's stand-in for the build image: a shell,
// sleep and cat, plus the probe and orbit binaries the test copies in.
const baseImage = "busybox:1.37.0@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// allowedHost is the egress allowlist in the test: a host a real build
// needs, reached through the proxy.
const allowedHost = "proxy.golang.org"

// dockerForTest returns a client for a working docker daemon, or skips.
// Under CI a missing daemon fails: GitHub's runners have one, and a skipped
// isolation test would make a green run mean nothing.
func dockerForTest(t *testing.T) (*Docker, dockerInfo) {
	t.Helper()
	sock := os.Getenv("ORBIT_TEST_DOCKER_SOCKET")
	if sock == "" {
		sock = "/var/run/docker.sock"
	}
	d := NewDocker(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	info, err := d.info(ctx)
	if err != nil {
		if os.Getenv("CI") != "" {
			t.Fatalf("docker is not available under CI: %v", err)
		}
		t.Skipf("docker is not available: %v", err)
	}
	return d, info
}

func goarch(dockerArch string) string {
	switch dockerArch {
	case "x86_64", "amd64":
		return "amd64"
	case "aarch64", "arm64":
		return "arm64"
	}
	return dockerArch
}

// buildBinaries cross-compiles orbit and the probe for the daemon's
// architecture.
func buildBinaries(t *testing.T, arch string) map[string][]byte {
	t.Helper()
	dir := t.TempDir()
	_, here, _, _ := runtime.Caller(0)
	pkgDir := filepath.Dir(here)
	out := map[string][]byte{}
	for name, pkg := range map[string]string{
		"orbit":           filepath.Join(pkgDir, "..", "..", "cmd", "orbit"),
		"probe":           filepath.Join(pkgDir, "testdata", "probe"),
		"extension-probe": filepath.Join(pkgDir, "testdata", "extension-probe"),
	} {
		bin := filepath.Join(dir, name)
		cmd := exec.Command("go", "build", "-trimpath", "-o", bin, ".")
		cmd.Dir = pkg
		cmd.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS=linux", "GOARCH="+arch)
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("build %s: %v\n%s", name, err, b)
		}
		b, err := os.ReadFile(bin)
		if err != nil {
			t.Fatal(err)
		}
		out[name] = b
	}
	return out
}

// buildTestImage makes the sandbox image: busybox with the binaries in
// /usr/local/bin and /work owned by UID, committed from a container (no
// build API needed). It returns the local image id, which pins it as a
// digest does.
func buildTestImage(t *testing.T, d *Docker, bins map[string][]byte) string {
	t.Helper()
	ctx := context.Background()
	if err := d.pull(ctx, baseImage); err != nil {
		t.Fatal(err)
	}
	var spec struct {
		Image  string            `json:"Image"`
		Cmd    []string          `json:"Cmd"`
		Labels map[string]string `json:"Labels"`
	}
	spec.Image, spec.Cmd, spec.Labels = baseImage, []string{"true"}, map[string]string{"orbit.sandbox.test": "1"}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create", nil, spec, &created); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = d.remove(context.Background(), created.ID) }()
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	now := time.Now()
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "work/", Mode: 0o755, Uid: UID, Gid: UID, ModTime: now})
	_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "usr/local/bin/", Mode: 0o755, ModTime: now})
	for name, data := range bins {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "usr/local/bin/" + name, Mode: 0o755, Size: int64(len(data)), ModTime: now})
		_, _ = tw.Write(data)
	}
	// The host's CA bundle, where it has one (a Linux runner), so `orbit credproxy` in the image can verify the backend.
	if ca, err := os.ReadFile("/etc/ssl/certs/ca-certificates.crt"); err == nil {
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeDir, Name: "etc/ssl/certs/", Mode: 0o755, ModTime: now})
		_ = tw.WriteHeader(&tar.Header{Typeflag: tar.TypeReg, Name: "etc/ssl/certs/ca-certificates.crt", Mode: 0o644, Size: int64(len(ca)), ModTime: now})
		_, _ = tw.Write(ca)
	}
	_ = tw.Close()
	if err := d.putArchive(ctx, created.ID, "/", &b); err != nil {
		t.Fatal(err)
	}
	var img struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/commit", url.Values{"container": {created.ID}}, map[string]any{"Labels": map[string]string{"orbit.sandbox.test": "1"}}, &img); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = d.call(context.Background(), http.MethodDelete, "/images/"+img.ID, url.Values{"force": {"true"}}, nil, nil)
	})
	return img.ID
}

func createNetwork(t *testing.T, d *Docker, name string, internal bool) {
	t.Helper()
	body := map[string]any{"Name": name, "Driver": "bridge", "Internal": internal, "Labels": map[string]string{"orbit.sandbox.test": "1"}}
	if err := d.call(context.Background(), http.MethodPost, "/networks/create", nil, body, nil); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.call(context.Background(), http.MethodDelete, "/networks/"+name, nil, nil, nil) })
}

// startEgress runs `orbit egress` as a container on both networks, as the
// deployment does.
func startEgress(t *testing.T, d *Docker, image, name, internal, outside string) string {
	t.Helper()
	ctx := context.Background()
	spec := map[string]any{
		"Image":      image,
		"User":       "10001:10001",
		"Entrypoint": []string{"orbit", "egress"},
		"Env":        []string{"ORBIT_EGRESS_ALLOW=" + allowedHost, "ORBIT_EGRESS_LISTEN=:3128"},
		"Labels":     map[string]string{"orbit.sandbox.test": "1"},
		"HostConfig": map[string]any{"NetworkMode": outside, "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{
			outside:  map[string]any{},
			internal: map[string]any{"Aliases": []string{name}},
		}},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create?name="+name, nil, spec, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.remove(context.Background(), created.ID) })
	if err := d.start(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
	return created.ID
}

// credproxyKey seals the integration test's tokens.
var credproxyKey = strings.Repeat("c", 40)

// startCredproxy runs `orbit credproxy` as a container on both networks, as the deployment does.
func startCredproxy(t *testing.T, d *Docker, image, name, internal, outside string) {
	t.Helper()
	ctx := context.Background()
	spec := map[string]any{
		"Image":      image,
		"User":       "10001:10001",
		"Entrypoint": []string{"orbit", "credproxy"},
		"Env":        []string{"ORBIT_CREDPROXY_KEY=" + credproxyKey, "ORBIT_CREDPROXY_LISTEN=:3129"},
		"Labels":     map[string]string{"orbit.sandbox.test": "1"},
		"HostConfig": map[string]any{"NetworkMode": outside, "ReadonlyRootfs": true, "CapDrop": []string{"ALL"}, "SecurityOpt": []string{"no-new-privileges:true"}},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{
			outside:  map[string]any{},
			internal: map[string]any{"Aliases": []string{name}},
		}},
	}
	var created struct {
		ID string `json:"Id"`
	}
	if err := d.call(ctx, http.MethodPost, "/containers/create?name="+name, nil, spec, &created); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.remove(context.Background(), created.ID) })
	if err := d.start(ctx, created.ID); err != nil {
		t.Fatal(err)
	}
}

type probeReport struct {
	UID        int               `json:"uid"`
	GID        int               `json:"gid"`
	Status     map[string]string `json:"status"`
	Writes     map[string]string `json:"writes"`
	Dials      map[string]string `json:"dials"`
	DNS        string            `json:"dns"`
	Proxy      map[string]any    `json:"proxy"`
	DockerSock string            `json:"docker_sock"`
	Credproxy  map[string]any    `json:"credproxy"`
	Env        []string          `json:"env"`
}

// TestIntegrationIsolation runs a real sandbox: create, attach, readback,
// destroy, and checks from inside what the container can reach.
func TestIntegrationIsolation(t *testing.T) {
	d, info := dockerForTest(t)
	if testing.Short() {
		t.Skip("-short")
	}
	bins := buildBinaries(t, goarch(info.Architecture))
	image := buildTestImage(t, d, bins)
	suffix := newID()[:8]
	internalNet, outsideNet, proxyName := "orbit-sbx-test-"+suffix, "orbit-sbx-out-"+suffix, "orbit-egress-"+suffix
	credName := "orbit-credproxy-" + suffix
	createNetwork(t, d, internalNet, true)
	createNetwork(t, d, outsideNet, false)
	egressID := startEgress(t, d, image, proxyName, internalNet, outsideNet)
	startCredproxy(t, d, image, credName, internalNet, outsideNet)

	dir := socketDir(t)
	cfg := Config{
		Token:   strings.Repeat("s", 40),
		Listen:  "unix:" + filepath.Join(dir, "sandboxd.sock"),
		Image:   image,
		Network: internalNet,
		Proxy:   proxyName + ":3128",
		Docker:  d.sock,

		CredProxy:    credName + ":3129",
		CredProxyKey: credproxyKey,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	var logs lockedLog
	s := New(cfg, slog.New(slog.NewTextHandler(&logs, nil)))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := s.Check(ctx); err != nil {
		t.Fatal(err)
	}
	serveUnix(t, filepath.Join(dir, "sandboxd.sock"), s.Handler())
	c, err := sandbox.NewClient(cfg.Listen, cfg.Token)
	if err != nil {
		t.Fatal(err)
	}

	in := mkTar(t, entry{h: tar.Header{Typeflag: tar.TypeReg, Name: "hello.txt", Mode: 0o644}, body: "hello\n"})
	cr, err := c.Create(ctx, "integration-"+suffix, in)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Destroy(context.Background(), cr.ID) })

	run := func(argv []string, stdin string) (int, string, string) {
		t.Helper()
		var stdout, stderr bytes.Buffer
		code, err := c.Attach(ctx, cr.ID, sandbox.AttachRequest{Argv: argv, Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "placeholder"}},
			strings.NewReader(stdin), &stdout, &stderr)
		if err != nil {
			t.Fatalf("%v: %v (stderr %q)", argv, err, stderr.String())
		}
		return code, stdout.String(), stderr.String()
	}

	// A trivial command, in /work, as the sandbox user.
	code, out, _ := run([]string{"sh", "-c", "id -u; id -g; pwd; cat hello.txt; echo out > result.txt"}, "")
	if code != 0 || out != "10001\n10001\n/work\nhello\n" {
		t.Fatalf("trivial command: exit %d, %q", code, out)
	}
	// stdin reaches the process and its EOF ends it.
	if code, out, _ := run([]string{"cat"}, "ping\n"); code != 0 || out != "ping\n" {
		t.Fatalf("stdin: exit %d, %q", code, out)
	}
	if code, _, errOut := run([]string{"sh", "-c", "echo oops >&2; exit 3"}, ""); code != 3 || errOut != "oops\n" {
		t.Fatalf("exit status: %d, %q", code, errOut)
	}

	code, out, _ = run([]string{"probe"}, "")
	if code != 0 {
		t.Fatalf("probe exit %d: %s", code, out)
	}
	var p probeReport
	if err := json.Unmarshal([]byte(out), &p); err != nil {
		t.Fatalf("probe output %q: %v", out, err)
	}
	t.Logf("probe: %s", out)
	if p.UID != UID || p.GID != UID {
		t.Errorf("runs as %d:%d", p.UID, p.GID)
	}
	if p.Status["CapEff"] != "0000000000000000" || p.Status["CapPrm"] != "0000000000000000" || p.Status["CapBnd"] != "0000000000000000" {
		t.Errorf("capabilities: %v", p.Status)
	}
	if p.Status["NoNewPrivs"] != "1" {
		t.Errorf("no_new_privs is %q", p.Status["NoNewPrivs"])
	}
	if p.Status["Seccomp"] != "2" {
		t.Errorf("seccomp mode is %q, want 2 (filter)", p.Status["Seccomp"])
	}
	for path, err := range p.Writes {
		writable := path == "/tmp/probe" || path == "/work/probe.txt"
		if writable && err != "" {
			t.Errorf("write %s failed: %s", path, err)
		}
		if !writable && err == "" {
			t.Errorf("wrote outside the workdir: %s", path)
		}
	}
	for addr, err := range p.Dials {
		if err == "" {
			t.Errorf("direct connection to %s succeeded", addr)
		}
	}
	if p.DNS == "" {
		t.Error("an external name resolved inside the sandbox (a DNS channel out)")
	}
	for target, want := range map[string]float64{
		"github.com:443":       403,
		"api.github.com:443":   403,
		"example.com:443":      403,
		"proxy.golang.org:80":  403,
		"proxy.golang.org:443": 200,
	} {
		if got, _ := p.Proxy[target].(float64); got != want {
			t.Errorf("CONNECT %s: %v, want %v", target, p.Proxy[target], want)
		}
	}
	// The process holds a token, never the credential, and reaches the backend only through the credential proxy: its token is forwarded to the Messages API (upstream answers the placeholder credential), another path or a forged token is refused by the proxy.
	if code, out, _ := run([]string{"sh", "-c", `echo "$CLAUDE_CODE_OAUTH_TOKEN"`}, ""); code != 0 || strings.Contains(out, "placeholder") || !strings.HasPrefix(out, "orbit-cp1.") {
		t.Errorf("the credential in the sandbox: %q", out)
	}
	// The Messages API request passed every check: the backend answered it (refusing the placeholder credential), or, without a CA bundle in the image, the proxy could not verify the backend (502).
	if got, _ := p.Credproxy["messages"].(map[string]any); got["by_proxy"] != false && got["status"] != float64(502) {
		t.Errorf("credproxy messages: %v, want it forwarded", p.Credproxy["messages"])
	}
	for name, want := range map[string]map[string]any{
		"models": {"status": float64(403), "by_proxy": true},
		"forged": {"status": float64(401), "by_proxy": true},
	} {
		got, _ := p.Credproxy[name].(map[string]any)
		for k, v := range want {
			if got[k] != v {
				t.Errorf("credproxy %s: %v, want %s %v", name, p.Credproxy[name], k, v)
			}
		}
	}
	if p.DockerSock == "" {
		t.Error("the docker socket is visible in the sandbox")
	}
	allowedEnv := map[string]bool{
		"HOME": true, "TMPDIR": true, "LANG": true, "PATH": true, "HOSTNAME": true,
		"HTTPS_PROXY": true, "HTTP_PROXY": true, "https_proxy": true, "http_proxy": true, "NO_PROXY": true, "no_proxy": true,
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": true, "DISABLE_AUTOUPDATER": true, "GIT_TERMINAL_PROMPT": true,
		"CLAUDE_CODE_OAUTH_TOKEN": true, "ANTHROPIC_API_KEY": true, "ANTHROPIC_BASE_URL": true,
	}
	for _, n := range p.Env {
		if !allowedEnv[n] {
			t.Errorf("unexpected variable in the sandbox: %s", n)
		}
	}

	// The workdir comes back with what the processes wrote.
	rc, err := c.Archive(ctx, cr.ID)
	if err != nil {
		t.Fatal(err)
	}
	files := map[string]string{}
	for _, e := range readTar(t, rc) {
		files[e.h.Name] = e.body
	}
	rc.Close()
	if files["hello.txt"] != "hello\n" || files["result.txt"] != "out\n" || files["probe.txt"] != "x" {
		t.Fatalf("readback: %v", files)
	}

	// Another container on the host, addressed by its full id, is not a
	// sandbox.
	var se *sandbox.Error
	if _, err := c.Archive(ctx, egressID); !errors.As(err, &se) || se.Status != http.StatusNotFound {
		t.Fatalf("archive of the egress container: %v", err)
	}

	if err := c.Destroy(ctx, cr.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := d.inspect(ctx, cr.ID); !errors.Is(err, errNotFound) {
		t.Fatalf("container after destroy: %v", err)
	}
	var vols struct {
		Volumes []struct{ Name string } `json:"Volumes"`
	}
	f, _ := json.Marshal(map[string][]string{"label": {LabelRunID + "=integration-" + suffix}})
	if err := d.call(ctx, http.MethodGet, "/volumes", url.Values{"filters": {string(f)}}, nil, &vols); err != nil {
		t.Fatal(err)
	}
	if len(vols.Volumes) != 0 {
		t.Fatalf("volumes left after destroy: %v", vols.Volumes)
	}
	if !strings.Contains(logs.String(), "sandboxd: destroyed") {
		t.Fatalf("logs: %s", logs.String())
	}

	assertExtensionIsolation(t, ctx, c, d, bins["extension-probe"])
}

func assertExtensionIsolation(t *testing.T, ctx context.Context, c *sandbox.Client, d *Docker, binary []byte) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "extension-probe"), binary, 0o755); err != nil {
		t.Fatal(err)
	}
	var outputSchema map[string]any
	if err := json.Unmarshal([]byte(extensionProbeOutputSchema), &outputSchema); err != nil {
		t.Fatal(err)
	}
	inputSchema := map[string]any{"type": "object", "properties": map[string]any{
		"marker": map[string]any{"type": "string", "maxLength": 32},
		"other":  map[string]any{"type": "string", "maxLength": 32},
		"hang":   map[string]any{"type": "boolean"},
		"flood":  map[string]any{"type": "boolean"},
		"memory": map[string]any{"type": "boolean"},
	}, "additionalProperties": false}
	operation := func(name, description string, timeout int) map[string]any {
		return map[string]any{"name": name, "kind": "source", "description": description, "inputs": []string{"data"},
			"input_schema": inputSchema, "output_schema": outputSchema,
			"limits": map[string]any{"timeout_ms": timeout, "memory_bytes": 134217728, "input_bytes": 1024, "output_bytes": 8192}}
	}
	manifest, err := json.Marshal(map[string]any{"protocol": "orbit.extension.v1", "id": "isolation-probe", "version": "1.0.0",
		"runtime": "native", "entrypoint": "extension-probe", "operations": []any{
			operation("inspect", "Inspect the sandbox boundary.", 15000),
			operation("hang", "Exercise timeout cleanup.", 1500),
			operation("flood", "Exercise the process limit.", 15000),
			operation("memory", "Exercise the memory limit.", 15000),
		}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), manifest, 0o600); err != nil {
		t.Fatal(err)
	}
	pkg, err := extensions.ValidateDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	invocation := func(operation string, params map[string]any) (json.RawMessage, error) {
		return extensions.Invoke(ctx, c, extensions.Invocation{PackageID: pkg.Manifest.ID, Directory: dir,
			Digest: pkg.Digest, Operation: operation, Parameters: extensionMustJSON(t, params),
			Inputs: map[string]extensions.Input{"data": {MediaType: "text/plain", Content: []byte("probe-input")}}})
	}
	check := func(result json.RawMessage, wantCrossTmp bool) {
		t.Helper()
		var got map[string]any
		if err := json.Unmarshal(result, &got); err != nil {
			t.Fatalf("extension result %q: %v", result, err)
		}
		for _, key := range []string{"package_read_only", "input_present", "input_read_only", "work_root_read_only", "tmp_writable", "out_writable", "no_network", "proxy_absent", "secrets_absent", "environment_clean", "docker_socket_absent", "capabilities_zero", "no_new_privileges", "seccomp_enabled", "host_paths_unreadable", "link_escape_blocked", "hardlink_escape_blocked"} {
			if got[key] != true {
				t.Errorf("extension isolation %s = %v", key, got[key])
			}
		}
		if !got["input_present"].(bool) {
			t.Errorf("extension input missing; write error: %v", got["input_write_error"])
		}
		if !got["work_root_read_only"].(bool) {
			t.Errorf("extension work root is writable or chmod-able; write error: %v, chmod error: %v", got["work_root_write_error"], got["work_root_chmod_error"])
		}
		if got["no_cross_tmp"] != wantCrossTmp {
			t.Errorf("extension cross-invocation tmpfs isolation = %v, want %v", got["no_cross_tmp"], wantCrossTmp)
		}
	}
	result, err := invocation("inspect", map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	check(result, true)

	result, err = invocation("flood", map[string]any{"flood": true})
	if err != nil {
		t.Fatal(err)
	}
	var flood map[string]any
	if err := json.Unmarshal(result, &flood); err != nil {
		t.Fatal(err)
	}
	forks, _ := flood["forks_before_limit"].(float64)
	if flood["fork_limit_hit"] != true || forks < 1 || forks >= ExtensionPids {
		t.Fatalf("process limit probe = %v, want bounded fork failure below %d", flood, ExtensionPids)
	}
	assertNoExtensionContainers(t, ctx, d)

	if _, err := invocation("memory", map[string]any{"memory": true}); err == nil || strings.Contains(err.Error(), "timed out") {
		t.Fatalf("memory exhaustion invocation = %v, want a bounded process failure", err)
	}
	assertNoExtensionContainers(t, ctx, d)

	// Two overlapping invocations must have different writable tmpfs state.
	type outcome struct {
		result json.RawMessage
		err    error
	}
	start := make(chan struct{})
	outcomes := make(chan outcome, 2)
	for _, pair := range [][2]string{{"first", "second"}, {"second", "first"}} {
		pair := pair
		go func() {
			<-start
			r, err := invocation("inspect", map[string]any{"marker": pair[0], "other": pair[1]})
			outcomes <- outcome{r, err}
		}()
	}
	close(start)
	for range 2 {
		got := <-outcomes
		if got.err != nil {
			t.Fatal(got.err)
		}
		check(got.result, true)
	}

	if _, err := invocation("hang", map[string]any{"hang": true}); err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("timed-out extension error = %v", err)
	}
	assertNoExtensionContainers(t, ctx, d)
}

func assertNoExtensionContainers(t *testing.T, ctx context.Context, d *Docker) {
	t.Helper()
	containers, err := d.managed(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, container := range containers {
		if container.Labels[LabelProfile] == sandbox.ProfileExtension {
			t.Errorf("extension container survived invocation cleanup: %s", container.ID)
		}
	}
}

func extensionMustJSON(t *testing.T, value any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

const extensionProbeOutputSchema = `{"type":"object","properties":{
"package_read_only":{"type":"boolean"},"input_present":{"type":"boolean"},"input_read_only":{"type":"boolean"},"input_write_error":{"type":"string","maxLength":512},"work_root_read_only":{"type":"boolean"},"work_root_write_error":{"type":"string","maxLength":512},"work_root_chmod_error":{"type":"string","maxLength":512},"tmp_writable":{"type":"boolean"},"out_writable":{"type":"boolean"},"no_network":{"type":"boolean"},"proxy_absent":{"type":"boolean"},"secrets_absent":{"type":"boolean"},"environment_clean":{"type":"boolean"},"docker_socket_absent":{"type":"boolean"},"capabilities_zero":{"type":"boolean"},"no_new_privileges":{"type":"boolean"},"seccomp_enabled":{"type":"boolean"},"host_paths_unreadable":{"type":"boolean"},"link_escape_blocked":{"type":"boolean"},"hardlink_escape_blocked":{"type":"boolean"},"fork_limit_hit":{"type":"boolean"},"forks_before_limit":{"type":"integer","minimum":0,"maximum":31},"no_cross_tmp":{"type":"boolean"}},"required":["package_read_only","input_present","input_read_only","input_write_error","work_root_read_only","work_root_write_error","work_root_chmod_error","tmp_writable","out_writable","no_network","proxy_absent","secrets_absent","environment_clean","docker_socket_absent","capabilities_zero","no_new_privileges","seccomp_enabled","host_paths_unreadable","link_escape_blocked","hardlink_escape_blocked","fork_limit_hit","forks_before_limit","no_cross_tmp"],"additionalProperties":false}`

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
