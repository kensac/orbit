// Package sandboxd is `orbit sandboxd`: the one process that holds a docker
// socket, and turns four narrow operations (internal/sandbox) into build
// containers whose every security option is fixed here, in code.
package sandboxd

import (
	"errors"
	"fmt"
	"net"
	"net/netip"
	"regexp"
	"strings"

	"github.com/kensac/orbit/internal/credproxy"
)

// Config is sandboxd's own configuration, from its environment. Nothing in
// it comes from a request.
type Config struct {
	// Token authenticates orbit (ORBIT_SANDBOX_TOKEN).
	Token string
	// Listen is "unix:/path" or "ip:port" with an explicit, specified IP
	// (ORBIT_SANDBOX_LISTEN): the tailnet address, never every interface.
	Listen string
	// Image is the build image, pinned by digest (ORBIT_SANDBOX_IMAGE).
	Image string
	// Network is the internal docker network build containers join
	// (ORBIT_SANDBOX_NETWORK); Check refuses one that is not internal.
	Network string
	// Proxy is the egress proxy's host:port on that network
	// (ORBIT_SANDBOX_PROXY).
	Proxy string
	// WorkstationProxy is the egress proxy a workstation profile's
	// sandboxes use instead of Proxy, on the same network
	// (ORBIT_SANDBOX_WORKSTATION_PROXY): one that allows any public host.
	// Unset refuses that profile.
	WorkstationProxy string
	// Runtime is "" (runc) or "runsc" (gVisor, when the host has it)
	// (ORBIT_SANDBOX_RUNTIME).
	Runtime string
	// Docker is the docker socket's path (ORBIT_SANDBOX_DOCKER_SOCKET).
	Docker string
	// CredProxy is `orbit credproxy`'s host:port on Network (ORBIT_SANDBOX_CREDPROXY): every sandbox's ANTHROPIC_BASE_URL, so the CLI reaches its backend only through it.
	CredProxy string
	// WorkstationCredProxy is the credential proxy's host:port on the workstation profile's network, when that differs (ORBIT_SANDBOX_WORKSTATION_CREDPROXY, default CredProxy): a proxy addressed by IP has one address per network.
	WorkstationCredProxy string
	// CredProxyKey seals the token a sandbox gets in place of the backend credential (ORBIT_CREDPROXY_KEY, shared with `orbit credproxy`, never given to a sandbox).
	CredProxyKey string
}

// MinTokenLen matches ORBIT_API_TOKEN's floor.
const MinTokenLen = 32

var (
	// A digest-pinned reference, or a local image id: both name exactly one
	// image, so what runs cannot change under a tag.
	imageRE   = regexp.MustCompile(`^([a-z0-9][a-z0-9._/:-]*@)?sha256:[0-9a-f]{64}$`)
	networkRE = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,63}$`)
)

// ConfigFromEnv reads the configuration and validates it.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	def := func(name, d string) string {
		if v := getenv(name); v != "" {
			return v
		}
		return d
	}
	c := Config{
		Token:   getenv("ORBIT_SANDBOX_TOKEN"),
		Listen:  getenv("ORBIT_SANDBOX_LISTEN"),
		Image:   getenv("ORBIT_SANDBOX_IMAGE"),
		Network: def("ORBIT_SANDBOX_NETWORK", "orbit-sandbox"),
		Proxy:   def("ORBIT_SANDBOX_PROXY", "orbit-egress:3128"),
		Runtime: getenv("ORBIT_SANDBOX_RUNTIME"),

		WorkstationProxy: getenv("ORBIT_SANDBOX_WORKSTATION_PROXY"),
		Docker:           def("ORBIT_SANDBOX_DOCKER_SOCKET", "/var/run/docker.sock"),
		CredProxy:        def("ORBIT_SANDBOX_CREDPROXY", "orbit-credproxy:3129"),
		CredProxyKey:     getenv("ORBIT_CREDPROXY_KEY"),
	}
	c.WorkstationCredProxy = def("ORBIT_SANDBOX_WORKSTATION_CREDPROXY", c.CredProxy)
	return c, c.Validate()
}

// Validate checks every field.
func (c Config) Validate() error {
	var errs []error
	if len(c.Token) < MinTokenLen {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_TOKEN must be at least %d characters", MinTokenLen))
	}
	if err := validListen(c.Listen); err != nil {
		errs = append(errs, err)
	}
	if !imageRE.MatchString(c.Image) {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_IMAGE %q must be pinned by digest (name@sha256:...)", c.Image))
	}
	if !networkRE.MatchString(c.Network) || c.Network == "host" || c.Network == "bridge" || c.Network == "none" {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_NETWORK %q must name a user-defined network", c.Network))
	}
	if host, port, err := net.SplitHostPort(c.Proxy); err != nil || host == "" || port == "" || strings.ContainsAny(host, "/@ ") {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_PROXY %q must be host:port", c.Proxy))
	}
	if p := c.WorkstationProxy; p != "" {
		if host, port, err := net.SplitHostPort(p); err != nil || host == "" || port == "" || strings.ContainsAny(host, "/@ ") {
			errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_WORKSTATION_PROXY %q must be host:port", p))
		}
	}
	if host, port, err := net.SplitHostPort(c.CredProxy); err != nil || host == "" || port == "" || strings.ContainsAny(host, "/@ ,") {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_CREDPROXY %q must be host:port", c.CredProxy))
	}
	if p := c.WorkstationCredProxy; p != "" {
		if host, port, err := net.SplitHostPort(p); err != nil || host == "" || port == "" || strings.ContainsAny(host, "/@ ,") {
			errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_WORKSTATION_CREDPROXY %q must be host:port", p))
		}
	}
	if len(c.CredProxyKey) < credproxy.MinKeyLen {
		errs = append(errs, fmt.Errorf("ORBIT_CREDPROXY_KEY must be at least %d characters: a sandbox never gets the backend credential, only a token sealed with it", credproxy.MinKeyLen))
	}
	if c.Runtime != "" && c.Runtime != "runsc" {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_RUNTIME %q: only runsc may replace the default runtime", c.Runtime))
	}
	if !strings.HasPrefix(c.Docker, "/") {
		errs = append(errs, fmt.Errorf("ORBIT_SANDBOX_DOCKER_SOCKET %q must be an absolute path", c.Docker))
	}
	return errors.Join(errs...)
}

// validListen refuses an address that would listen on every interface: on a
// host without a firewall that is public.
func validListen(addr string) error {
	if p, ok := strings.CutPrefix(addr, "unix:"); ok {
		if !strings.HasPrefix(p, "/") {
			return fmt.Errorf("ORBIT_SANDBOX_LISTEN %q: a unix socket path must be absolute", addr)
		}
		return nil
	}
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return fmt.Errorf("ORBIT_SANDBOX_LISTEN %q must be unix:/path or ip:port", addr)
	}
	if ap.Addr().IsUnspecified() {
		return fmt.Errorf("ORBIT_SANDBOX_LISTEN %q listens on every interface; bind the tailnet address", addr)
	}
	return nil
}
