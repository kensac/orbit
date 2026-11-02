package sandboxd

import (
	"fmt"
	"net"
	"sort"
	"strconv"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

// Every limit and security option of a build container. None of them is
// taken from a request: a caller chooses the workdir's content, a run id
// label and an argv, nothing else.
const (
	// UID is the build image's user (deploy/build.Dockerfile), the same uid
	// orbit's own image runs as. It owns /work and nothing else.
	UID = 10001
	// Lifetime is how long a sandbox's processes may run: the container's
	// PID 1 is `sleep Lifetime`, so at expiry the kernel ends every process
	// in it, whatever sandboxd is doing.
	Lifetime = time.Hour
	// Grace is how long an expired sandbox is kept so orbit can still read
	// its workdir back, before the reaper removes it.
	Grace = 30 * time.Minute
	// MemoryBytes is the container's memory limit, swap included. tmpfs
	// pages count against it, so it covers TmpBytes plus a `go test -race`
	// of orbit with room to spare.
	MemoryBytes = 6 << 30
	// NanoCPUs is two CPUs: oci has four and also runs CI runners.
	NanoCPUs = 2_000_000_000
	// Pids bounds processes and threads: go test and node use many threads.
	Pids = 1024
	// TmpBytes sizes /tmp, which is also HOME (the CLI's state, Go's module
	// and build caches).
	TmpBytes = 2 << 30
	// NoFile is the open-file limit.
	NoFile = 8192
	// MaxSandboxes is how many may exist at once: two of MemoryBytes is what
	// oci can give without starving its CI runners.
	MaxSandboxes = 2
)

// Labels sandboxd puts on what it creates. Every operation refuses a
// container without LabelManaged, so the API can never reach another
// container on the host.
const (
	LabelManaged = "orbit.sandbox"
	LabelRunID   = "orbit.sandbox.run_id"
	LabelExpires = "orbit.sandbox.expires"
)

// The Docker Engine API shapes sandboxd sends. Only fields sandboxd sets are
// declared, so a field that is not here is the daemon's default; options.go's
// tests pin the full encoded document.

type containerCreate struct {
	Image            string            `json:"Image"`
	User             string            `json:"User"`
	Entrypoint       []string          `json:"Entrypoint"`
	Cmd              []string          `json:"Cmd"`
	Env              []string          `json:"Env"`
	WorkingDir       string            `json:"WorkingDir"`
	Hostname         string            `json:"Hostname"`
	Labels           map[string]string `json:"Labels"`
	AttachStdin      bool              `json:"AttachStdin"`
	AttachStdout     bool              `json:"AttachStdout"`
	AttachStderr     bool              `json:"AttachStderr"`
	OpenStdin        bool              `json:"OpenStdin"`
	Tty              bool              `json:"Tty"`
	NetworkDisabled  bool              `json:"NetworkDisabled"`
	HostConfig       hostConfig        `json:"HostConfig"`
	NetworkingConfig networkingConfig  `json:"NetworkingConfig"`
}

type hostConfig struct {
	Privileged      bool              `json:"Privileged"`
	ReadonlyRootfs  bool              `json:"ReadonlyRootfs"`
	CapAdd          []string          `json:"CapAdd"`
	CapDrop         []string          `json:"CapDrop"`
	SecurityOpt     []string          `json:"SecurityOpt"`
	NetworkMode     string            `json:"NetworkMode"`
	IpcMode         string            `json:"IpcMode"`
	PidMode         string            `json:"PidMode"`
	UTSMode         string            `json:"UTSMode"`
	UsernsMode      string            `json:"UsernsMode"`
	CgroupnsMode    string            `json:"CgroupnsMode"`
	Binds           []string          `json:"Binds"`
	Mounts          []mount           `json:"Mounts"`
	Tmpfs           map[string]string `json:"Tmpfs"`
	Devices         []any             `json:"Devices"`
	PublishAllPorts bool              `json:"PublishAllPorts"`
	PortBindings    map[string]any    `json:"PortBindings"`
	ExtraHosts      []string          `json:"ExtraHosts"`
	Memory          int64             `json:"Memory"`
	MemorySwap      int64             `json:"MemorySwap"`
	NanoCpus        int64             `json:"NanoCpus"`
	PidsLimit       int64             `json:"PidsLimit"`
	Ulimits         []ulimit          `json:"Ulimits"`
	OomScoreAdj     int               `json:"OomScoreAdj"`
	Init            bool              `json:"Init"`
	AutoRemove      bool              `json:"AutoRemove"`
	RestartPolicy   restartPolicy     `json:"RestartPolicy"`
	Runtime         string            `json:"Runtime,omitempty"`
}

type mount struct {
	Type string `json:"Type"`
	// Source names a box's volume; a build's is anonymous.
	Source        string        `json:"Source,omitempty"`
	Target        string        `json:"Target"`
	ReadOnly      bool          `json:"ReadOnly"`
	VolumeOptions volumeOptions `json:"VolumeOptions"`
}

type volumeOptions struct {
	NoCopy bool              `json:"NoCopy"`
	Labels map[string]string `json:"Labels"`
}

type ulimit struct {
	Name string `json:"Name"`
	Soft int64  `json:"Soft"`
	Hard int64  `json:"Hard"`
}

type restartPolicy struct {
	Name string `json:"Name"`
}

type networkingConfig struct {
	EndpointsConfig map[string]struct{} `json:"EndpointsConfig"`
}

type execCreate struct {
	AttachStdin  bool     `json:"AttachStdin"`
	AttachStdout bool     `json:"AttachStdout"`
	AttachStderr bool     `json:"AttachStderr"`
	Tty          bool     `json:"Tty"`
	Privileged   bool     `json:"Privileged"`
	User         string   `json:"User"`
	WorkingDir   string   `json:"WorkingDir"`
	Cmd          []string `json:"Cmd"`
	Env          []string `json:"Env"`
	// ConsoleSize is a TTY's [rows, cols] at start.
	ConsoleSize []int `json:"ConsoleSize,omitempty"`
}

// user is the uid:gid every process runs as.
var user = strconv.Itoa(UID) + ":" + strconv.Itoa(UID)

// containerSpec is the one container sandboxd ever creates. Its inputs are
// sandboxd's own configuration, the caller's run id (a label, validated) and
// the clock.
func containerSpec(cfg Config, runID string, now time.Time) containerCreate {
	labels := map[string]string{
		LabelManaged: "1",
		LabelRunID:   runID,
		LabelExpires: strconv.FormatInt(now.Add(Lifetime).Unix(), 10),
	}
	proxy := "http://" + cfg.Proxy
	credHost, _, _ := net.SplitHostPort(cfg.CredProxy)
	return containerCreate{
		Image: cfg.Image,
		User:  user,
		// PID 1 (under docker's init, which reaps orphans) is a timer: the
		// processes orbit attaches are its siblings and end with it.
		Entrypoint: []string{"sleep", strconv.Itoa(int(Lifetime / time.Second))},
		Cmd:        nil,
		Env: []string{
			"HOME=/tmp",
			"TMPDIR=/tmp",
			"LANG=C.UTF-8",
			// The only route out is the egress proxy; tools that ignore
			// these variables have no route at all.
			"HTTPS_PROXY=" + proxy,
			"HTTP_PROXY=" + proxy,
			"https_proxy=" + proxy,
			"http_proxy=" + proxy,
			// The backend is reached through the credential proxy, directly on this network, never through egress.
			"ANTHROPIC_BASE_URL=http://" + cfg.CredProxy,
			"NO_PROXY=" + credHost,
			"no_proxy=" + credHost,
			"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
			"DISABLE_AUTOUPDATER=1",
			"GIT_TERMINAL_PROMPT=0",
		},
		WorkingDir: sandbox.WorkDir,
		Hostname:   "sandbox",
		Labels:     labels,
		HostConfig: hostConfig{
			Privileged:     false,
			ReadonlyRootfs: true,
			CapAdd:         nil,
			CapDrop:        []string{"ALL"},
			// Docker's default seccomp and AppArmor profiles stay on
			// (sandboxd refuses a daemon without seccomp, Check).
			SecurityOpt:  []string{"no-new-privileges:true"},
			NetworkMode:  cfg.Network,
			IpcMode:      "private",
			PidMode:      "",
			UTSMode:      "",
			UsernsMode:   "",
			CgroupnsMode: "private",
			Binds:        nil,
			Mounts: []mount{{
				// An anonymous volume, removed with the container.
				Type:          "volume",
				Target:        sandbox.WorkDir,
				ReadOnly:      false,
				VolumeOptions: volumeOptions{NoCopy: false, Labels: map[string]string{LabelManaged: "1", LabelRunID: runID}},
			}},
			Tmpfs: map[string]string{
				"/tmp": "rw,nosuid,nodev,size=" + strconv.Itoa(TmpBytes) + ",mode=1777",
			},
			Devices:         nil,
			PublishAllPorts: false,
			PortBindings:    nil,
			ExtraHosts:      nil,
			Memory:          MemoryBytes,
			MemorySwap:      MemoryBytes,
			NanoCpus:        NanoCPUs,
			PidsLimit:       Pids,
			Ulimits:         []ulimit{{Name: "nofile", Soft: NoFile, Hard: NoFile}},
			// Under memory pressure on the host, a build is what goes first.
			OomScoreAdj:   500,
			Init:          true,
			AutoRemove:    false,
			RestartPolicy: restartPolicy{Name: "no"},
			Runtime:       cfg.Runtime,
		},
		NetworkingConfig: networkingConfig{EndpointsConfig: map[string]struct{}{cfg.Network: {}}},
	}
}

// execSpec is the one process shape sandboxd starts: the caller's argv and
// allowed environment, everything else fixed. The environment is sorted so
// the request is deterministic.
func execSpec(ar sandbox.AttachRequest) execCreate {
	env := make([]string, 0, len(ar.Env))
	for k, v := range ar.Env {
		if sandbox.AllowedEnv[k] {
			env = append(env, k+"="+v)
		}
	}
	sort.Strings(env)
	return execCreate{
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Tty:          false,
		Privileged:   false,
		User:         user,
		WorkingDir:   sandbox.WorkDir,
		Cmd:          append([]string(nil), ar.Argv...),
		Env:          env,
	}
}

// containerState is the part of a container inspect sandboxd checks.
type containerState struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	Config  struct {
		Image  string            `json:"Image"`
		User   string            `json:"User"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	HostConfig struct {
		Privileged     bool     `json:"Privileged"`
		ReadonlyRootfs bool     `json:"ReadonlyRootfs"`
		CapAdd         []string `json:"CapAdd"`
		CapDrop        []string `json:"CapDrop"`
		SecurityOpt    []string `json:"SecurityOpt"`
		NetworkMode    string   `json:"NetworkMode"`
		Binds          []string `json:"Binds"`
		PidMode        string   `json:"PidMode"`
		IpcMode        string   `json:"IpcMode"`
		UsernsMode     string   `json:"UsernsMode"`
		Memory         int64    `json:"Memory"`
		PidsLimit      *int64   `json:"PidsLimit"`
	} `json:"HostConfig"`
	Mounts []mountState `json:"Mounts"`
	State  struct {
		Running bool `json:"Running"`
		// Health is absent for a container without a healthcheck.
		Health *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// mountState is one mount of an inspected container; Name is a volume's.
type mountState struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Destination string `json:"Destination"`
}

// verifyCreated checks that the container the daemon made is the one
// sandboxd asked for, so a daemon default, plugin or API change that
// altered an option is caught before anything runs in it.
func verifyCreated(cfg Config, c containerState) error {
	h := c.HostConfig
	switch {
	case c.Config.Labels[LabelManaged] != "1":
		return fmt.Errorf("label %s missing", LabelManaged)
	case c.Config.User != user:
		return fmt.Errorf("user is %q, want %q", c.Config.User, user)
	case h.Privileged:
		return fmt.Errorf("container is privileged")
	case !h.ReadonlyRootfs:
		return fmt.Errorf("root filesystem is writable")
	case len(h.CapAdd) != 0:
		return fmt.Errorf("capabilities added: %v", h.CapAdd)
	case len(h.CapDrop) != 1 || h.CapDrop[0] != "ALL":
		return fmt.Errorf("capabilities dropped: %v, want [ALL]", h.CapDrop)
	case !contains(h.SecurityOpt, "no-new-privileges:true"):
		return fmt.Errorf("no-new-privileges missing from %v", h.SecurityOpt)
	case h.NetworkMode != cfg.Network:
		return fmt.Errorf("network is %q, want %q", h.NetworkMode, cfg.Network)
	case len(h.Binds) != 0:
		return fmt.Errorf("bind mounts present: %v", h.Binds)
	case h.PidMode != "" || h.UsernsMode != "":
		return fmt.Errorf("host namespace shared (pid %q, userns %q)", h.PidMode, h.UsernsMode)
	case h.IpcMode != "private":
		return fmt.Errorf("ipc mode is %q", h.IpcMode)
	case h.Memory != MemoryBytes:
		return fmt.Errorf("memory limit is %d", h.Memory)
	case h.PidsLimit == nil || *h.PidsLimit != Pids:
		return fmt.Errorf("pids limit is not %d", Pids)
	}
	for _, m := range c.Mounts {
		if m.Type != "volume" && m.Type != "tmpfs" {
			return fmt.Errorf("mount of type %s at %s", m.Type, m.Destination)
		}
		if m.Type == "volume" && m.Destination != sandbox.WorkDir {
			return fmt.Errorf("volume at %s", m.Destination)
		}
	}
	return nil
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}
