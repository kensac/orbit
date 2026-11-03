package sandboxd

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

const testImage = "ghcr.io/kensac/orbit-build@sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func testConfig() Config {
	return Config{
		Token:   strings.Repeat("t", 40),
		Listen:  "unix:/run/sandboxd.sock",
		Image:   testImage,
		Network: "orbit-sandbox",
		Proxy:   "orbit-egress:3128",
		Docker:  "/var/run/docker.sock",

		CredProxy:    "orbit-credproxy:3129",
		CredProxyKey: strings.Repeat("k", 40),
	}
}

// TestContainerSpecDocument pins the whole create request. Any change to an
// option docker receives changes this document, so it is reviewed as the
// security change it is.
func TestContainerSpecDocument(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	b, err := json.Marshal(containerSpec(testConfig(), "run-1", now))
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	want := map[string]any{
		"Image":      testImage,
		"User":       "10001:10001",
		"Entrypoint": []any{"sleep", "3600"},
		"Cmd":        nil,
		"Env": []any{
			"HOME=/tmp", "TMPDIR=/tmp", "LANG=C.UTF-8",
			"HTTPS_PROXY=http://orbit-egress:3128", "HTTP_PROXY=http://orbit-egress:3128",
			"https_proxy=http://orbit-egress:3128", "http_proxy=http://orbit-egress:3128",
			"ANTHROPIC_BASE_URL=http://orbit-credproxy:3129", "NO_PROXY=orbit-credproxy", "no_proxy=orbit-credproxy",
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "GIT_TERMINAL_PROMPT=0",
		},
		"WorkingDir": "/work",
		"Hostname":   "sandbox",
		"Labels": map[string]any{
			"orbit.sandbox": "1", "orbit.sandbox.run_id": "run-1", "orbit.sandbox.expires": "1800003600",
		},
		"AttachStdin": false, "AttachStdout": false, "AttachStderr": false,
		"OpenStdin": false, "Tty": false, "NetworkDisabled": false,
		"HostConfig": map[string]any{
			"Privileged":     false,
			"ReadonlyRootfs": true,
			"CapAdd":         nil,
			"CapDrop":        []any{"ALL"},
			"SecurityOpt":    []any{"no-new-privileges:true"},
			"NetworkMode":    "orbit-sandbox",
			"IpcMode":        "private",
			"PidMode":        "",
			"UTSMode":        "",
			"UsernsMode":     "",
			"CgroupnsMode":   "private",
			"Binds":          nil,
			"Mounts": []any{map[string]any{
				"Type": "volume", "Target": "/work", "ReadOnly": false,
				"VolumeOptions": map[string]any{"NoCopy": false, "Labels": map[string]any{"orbit.sandbox": "1", "orbit.sandbox.run_id": "run-1"}},
			}},
			"Tmpfs":           map[string]any{"/tmp": "rw,nosuid,nodev,size=2147483648,mode=1777"},
			"Devices":         nil,
			"PublishAllPorts": false,
			"PortBindings":    nil,
			"ExtraHosts":      nil,
			"Memory":          float64(6 << 30),
			"MemorySwap":      float64(6 << 30),
			"NanoCpus":        float64(2_000_000_000),
			"PidsLimit":       float64(1024),
			"Ulimits":         []any{map[string]any{"Name": "nofile", "Soft": float64(8192), "Hard": float64(8192)}},
			"OomScoreAdj":     float64(500),
			"Init":            true,
			"AutoRemove":      false,
			"RestartPolicy":   map[string]any{"Name": "no"},
		},
		"NetworkingConfig": map[string]any{"EndpointsConfig": map[string]any{"orbit-sandbox": map[string]any{}}},
	}
	if !reflect.DeepEqual(got, want) {
		gb, _ := json.MarshalIndent(got, "", "  ")
		t.Fatalf("create request changed:\n%s", gb)
	}
}

func TestContainerSpecRuntime(t *testing.T) {
	cfg := testConfig()
	cfg.Runtime = "runsc"
	if got := containerSpec(cfg, "r", time.Now()).HostConfig.Runtime; got != "runsc" {
		t.Fatalf("runtime %q", got)
	}
}

func TestExtensionProfileHasNoNetworkOrCredentialEnvironment(t *testing.T) {
	cfg := testConfig()
	cfg.ExtensionProfile = true
	cfg.ExtensionMemoryBytes = ExtensionMemoryBytes
	c := containerSpec(cfg, "extension-1", time.Unix(1_800_000_000, 0))
	if !c.NetworkDisabled || c.HostConfig.NetworkMode != "none" || len(c.NetworkingConfig.EndpointsConfig) != 0 {
		t.Fatalf("extension network is enabled: %+v", c)
	}
	if !reflect.DeepEqual(c.Env, []string{"HOME=/tmp", "TMPDIR=/tmp", "LANG=C.UTF-8"}) {
		t.Fatalf("extension environment = %v", c.Env)
	}
	if c.HostConfig.Memory != ExtensionMemoryBytes || c.HostConfig.MemorySwap != ExtensionMemoryBytes || c.HostConfig.NanoCpus != ExtensionNanoCPUs || c.HostConfig.PidsLimit != ExtensionPids {
		t.Fatalf("extension limits = %+v", c.HostConfig)
	}
	if c.HostConfig.Tmpfs["/work/out"] == "" || c.HostConfig.Tmpfs["/tmp"] == "" {
		t.Fatalf("extension scratch limits missing: %v", c.HostConfig.Tmpfs)
	}
	if c.Labels[LabelProfile] != sandbox.ProfileExtension {
		t.Fatalf("extension profile label = %v", c.Labels)
	}
	got, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	var state containerState
	if err := json.Unmarshal(got, &state); err != nil {
		t.Fatal(err)
	}
	state.ID = strings.Repeat("a", 64)
	state.Config.User = user
	state.Config.Labels = c.Labels
	state.HostConfig.ReadonlyRootfs = true
	state.HostConfig.CapDrop = []string{"ALL"}
	state.HostConfig.SecurityOpt = []string{"no-new-privileges:true"}
	state.HostConfig.NetworkMode = "none"
	state.HostConfig.Tmpfs = c.HostConfig.Tmpfs
	for destination := range c.HostConfig.Tmpfs {
		state.Mounts = append(state.Mounts, mountState{Type: "tmpfs", Destination: destination})
	}
	state.HostConfig.IpcMode = "private"
	state.HostConfig.Memory = ExtensionMemoryBytes
	state.HostConfig.MemorySwap = ExtensionMemoryBytes
	state.HostConfig.NanoCpus = ExtensionNanoCPUs
	pids := int64(ExtensionPids)
	state.HostConfig.PidsLimit = &pids
	if err := verifyCreated(cfg, state); err != nil {
		t.Fatalf("verify extension profile: %v", err)
	}
}

func TestExtensionProfileAttachIsFixedAndCredentialFree(t *testing.T) {
	c := goodState()
	c.Config.Labels[LabelProfile] = sandbox.ProfileExtension
	for _, tc := range []struct {
		name string
		argv []string
		env  map[string]string
		ok   bool
	}{
		{name: "package entrypoint", argv: []string{"/work/extension/bin/reducer"}, ok: true},
		{name: "outside package", argv: []string{"/usr/bin/cat"}},
		{name: "traversal", argv: []string{"/work/extension/../bin/reducer"}},
		{name: "extra argument", argv: []string{"/work/extension/bin/reducer", "--unsafe"}},
		{name: "environment", argv: []string{"/work/extension/bin/reducer"}, env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "token"}},
		{name: "empty env map", argv: []string{"/work/extension/bin/reducer"}, env: map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateProfileAttach(c, sandbox.AttachRequest{Argv: tc.argv, Env: tc.env})
			if (err == nil) != tc.ok {
				t.Fatalf("validateProfileAttach() = %v, want ok=%v", err, tc.ok)
			}
		})
	}
}

// TestSpecTakesNoCallerOptions holds the shape of the inputs: the create
// spec is a function of sandboxd's config, a run id and the clock, and the
// exec spec of an AttachRequest, whose only fields are argv and env.
func TestSpecTakesNoCallerOptions(t *testing.T) {
	ar := reflect.TypeOf(sandbox.AttachRequest{})
	var fields []string
	for i := range ar.NumField() {
		fields = append(fields, ar.Field(i).Name)
	}
	if !reflect.DeepEqual(fields, []string{"Argv", "Env"}) {
		t.Fatalf("AttachRequest fields %v: a new caller-controlled field is a security change", fields)
	}
	ct := reflect.TypeOf(containerSpec)
	if ct.NumIn() != 3 || ct.In(0) != reflect.TypeOf(Config{}) || ct.In(1).Kind() != reflect.String || ct.In(2) != reflect.TypeOf(time.Time{}) {
		t.Fatalf("containerSpec signature %v", ct)
	}
}

func TestExecSpec(t *testing.T) {
	got := execSpec(sandbox.AttachRequest{
		Argv: []string{"claude", "-p"},
		// Validate refuses the second; execSpec drops it regardless.
		Env: map[string]string{"CLAUDE_CODE_OAUTH_TOKEN": "tok", "LD_PRELOAD": "/x.so"},
	})
	want := execCreate{
		AttachStdin: true, AttachStdout: true, AttachStderr: true,
		Tty: false, Privileged: false, User: "10001:10001", WorkingDir: "/work",
		Cmd: []string{"claude", "-p"}, Env: []string{"CLAUDE_CODE_OAUTH_TOKEN=tok"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("exec spec %+v", got)
	}
}

func goodState() containerState {
	var c containerState
	c.ID = strings.Repeat("a", 64)
	c.Config.User = "10001:10001"
	c.Config.Labels = map[string]string{LabelManaged: "1"}
	c.HostConfig.ReadonlyRootfs = true
	c.HostConfig.CapDrop = []string{"ALL"}
	c.HostConfig.SecurityOpt = []string{"no-new-privileges:true"}
	c.HostConfig.NetworkMode = "orbit-sandbox"
	c.HostConfig.IpcMode = "private"
	c.HostConfig.Memory = MemoryBytes
	c.HostConfig.MemorySwap = MemoryBytes
	c.HostConfig.NanoCpus = NanoCPUs
	p := int64(Pids)
	c.HostConfig.PidsLimit = &p
	c.Mounts = append(c.Mounts, mountState{Type: "volume", Destination: "/work"})
	return c
}

func TestVerifyCreated(t *testing.T) {
	cfg := testConfig()
	if err := verifyCreated(cfg, goodState()); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*containerState){
		"no label":       func(c *containerState) { c.Config.Labels = nil },
		"root user":      func(c *containerState) { c.Config.User = "" },
		"privileged":     func(c *containerState) { c.HostConfig.Privileged = true },
		"writable root":  func(c *containerState) { c.HostConfig.ReadonlyRootfs = false },
		"cap added":      func(c *containerState) { c.HostConfig.CapAdd = []string{"SYS_ADMIN"} },
		"caps kept":      func(c *containerState) { c.HostConfig.CapDrop = nil },
		"new privileges": func(c *containerState) { c.HostConfig.SecurityOpt = nil },
		"host network":   func(c *containerState) { c.HostConfig.NetworkMode = "host" },
		"bind mount":     func(c *containerState) { c.HostConfig.Binds = []string{"/:/host"} },
		"host pid":       func(c *containerState) { c.HostConfig.PidMode = "host" },
		"host userns":    func(c *containerState) { c.HostConfig.UsernsMode = "host" },
		"host ipc":       func(c *containerState) { c.HostConfig.IpcMode = "host" },
		"no memory cap":  func(c *containerState) { c.HostConfig.Memory = 0 },
		"no pids cap":    func(c *containerState) { c.HostConfig.PidsLimit = nil },
		"bind in mounts": func(c *containerState) { c.Mounts[0].Type = "bind" },
		"volume elsewhere": func(c *containerState) {
			c.Mounts[0].Destination = "/etc"
		},
	} {
		c := goodState()
		mutate(&c)
		if err := verifyCreated(cfg, c); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestConfigValidate(t *testing.T) {
	if err := testConfig().Validate(); err != nil {
		t.Fatal(err)
	}
	ok := []func(*Config){
		func(c *Config) { c.Listen = "100.123.116.85:8091" },
		func(c *Config) { c.Listen = "[fd7a:115c:a1e0::1]:8091" },
		func(c *Config) { c.Image = "sha256:" + strings.Repeat("0", 64) },
		func(c *Config) { c.Runtime = "runsc" },
		func(c *Config) {
			c.WorkstationProxy, c.WorkstationNetwork = "orbit-egress-open:3128", "orbit-workstation"
		},
	}
	for i, m := range ok {
		c := testConfig()
		m(&c)
		if err := c.Validate(); err != nil {
			t.Errorf("ok %d: %v", i, err)
		}
	}
	for name, m := range map[string]func(*Config){
		"short token":                   func(c *Config) { c.Token = "short" },
		"every interface":               func(c *Config) { c.Listen = ":8091" },
		"unspecified ipv4":              func(c *Config) { c.Listen = "0.0.0.0:8091" },
		"unspecified ipv6":              func(c *Config) { c.Listen = "[::]:8091" },
		"hostname listen":               func(c *Config) { c.Listen = "oci:8091" },
		"relative socket":               func(c *Config) { c.Listen = "unix:sandboxd.sock" },
		"tag image":                     func(c *Config) { c.Image = "ghcr.io/kensac/orbit-build:latest" },
		"no image":                      func(c *Config) { c.Image = "" },
		"host network":                  func(c *Config) { c.Network = "host" },
		"bridge network":                func(c *Config) { c.Network = "bridge" },
		"container net":                 func(c *Config) { c.Network = "container:abc" },
		"proxy no port":                 func(c *Config) { c.Proxy = "orbit-egress" },
		"other runtime":                 func(c *Config) { c.Runtime = "kata" },
		"relative docker":               func(c *Config) { c.Docker = "docker.sock" },
		"no credproxy":                  func(c *Config) { c.CredProxy = "" },
		"credproxy a URL":               func(c *Config) { c.CredProxy = "http://orbit-credproxy:3129" },
		"no credproxy key":              func(c *Config) { c.CredProxyKey = "" },
		"short key":                     func(c *Config) { c.CredProxyKey = "short" },
		"workstation credproxy no port": func(c *Config) { c.WorkstationCredProxy = "10.251.1.3" },
		"workstation beside builds": func(c *Config) {
			c.WorkstationProxy, c.WorkstationNetwork = "orbit-egress-open:3128", c.Network
		},
		"workstation without its network": func(c *Config) { c.WorkstationProxy = "orbit-egress-open:3128" },
		"workstation network alone":       func(c *Config) { c.WorkstationNetwork = "orbit-workstation" },
		"workstation on host":             func(c *Config) { c.WorkstationProxy, c.WorkstationNetwork = "orbit-egress-open:3128", "host" },
	} {
		c := testConfig()
		m(&c)
		if err := c.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
