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
	// Runtime is "" (runc) or "runsc" (gVisor, when the host has it)
	// (ORBIT_SANDBOX_RUNTIME).
	Runtime string
	// Docker is the docker socket's path (ORBIT_SANDBOX_DOCKER_SOCKET).
	Docker string
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
		Docker:  def("ORBIT_SANDBOX_DOCKER_SOCKET", "/var/run/docker.sock"),
	}
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
