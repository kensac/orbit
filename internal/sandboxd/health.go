package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

// readyTTL is how long a readiness answer is reused, so a probe every few seconds asks docker at most this often.
const readyTTL = 10 * time.Second

// readyTimeout bounds the docker calls of one readiness check.
const readyTimeout = 3 * time.Second

func (s *Server) readyz(w http.ResponseWriter, r *http.Request) {
	rd := s.Readiness(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if !rd.Ready {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	_ = json.NewEncoder(w).Encode(rd)
}

// Readiness is whether sandboxd can make a sandbox now: the daemon answers, the sandbox network (and the workstation network, when set) is still internal, and each egress proxy's and credential proxy's container is running (and healthy, when it has a healthcheck). A proxy is checked by the container named as its host, which is how compose names it on the network. Answers are cached for readyTTL.
func (s *Server) Readiness(ctx context.Context) sandbox.Readiness {
	s.readyMu.Lock()
	defer s.readyMu.Unlock()
	if !s.readyAt.IsZero() && s.now().Sub(s.readyAt) < readyTTL {
		return s.ready
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), readyTimeout)
	defer cancel()
	rd := sandbox.Readiness{Ready: true, Checks: map[string]string{}}
	fail := func(name string, err error) {
		rd.Ready = false
		rd.Checks[name] = err.Error()
	}
	if _, err := s.d.info(ctx); err != nil {
		fail("docker", err)
	} else {
		rd.Checks["docker"] = "ok"
	}
	nets := []struct{ check, name string }{{"network", s.cfg.Network}}
	if s.cfg.WorkstationNetwork != "" {
		nets = append(nets, struct{ check, name string }{"workstation_network", s.cfg.WorkstationNetwork})
	}
	for _, nw := range nets {
		if n, err := s.d.network(ctx, nw.name); err != nil {
			fail(nw.check, err)
		} else if !n.Internal {
			fail(nw.check, errors.New(nw.name+" is not internal"))
		} else {
			rd.Checks[nw.check] = "ok"
		}
	}
	// The egress proxies are proxy:<host> (orbit_egress_ready), the credential proxy credproxy:<host>.
	seen := map[string]bool{}
	for _, p := range []struct{ addr, check string }{
		{s.cfg.Proxy, "proxy:"}, {s.cfg.WorkstationProxy, "proxy:"},
		{s.cfg.CredProxy, "credproxy:"}, {s.cfg.WorkstationCredProxy, "credproxy:"},
	} {
		host, _, err := net.SplitHostPort(p.addr)
		check := p.check + host
		if p.addr == "" || err != nil || seen[check] {
			continue
		}
		seen[check] = true
		if _, err := netip.ParseAddr(host); err == nil {
			// Addressed by IP on a sandbox network sandboxd is not on: there is no container name to look up.
			rd.Checks[check] = "unchecked: addressed by IP"
			continue
		}
		if err := s.proxyUp(ctx, host); err != nil {
			fail(check, err)
		} else {
			rd.Checks[check] = "ok"
		}
	}
	s.ready, s.readyAt = rd, s.now()
	return rd
}

func (s *Server) proxyUp(ctx context.Context, name string) error {
	c, err := s.d.inspect(ctx, url.PathEscape(name))
	if errors.Is(err, errNotFound) {
		return errors.New("no container named " + name)
	}
	if err != nil {
		return err
	}
	switch {
	case !c.State.Running:
		return errors.New(name + " is not running")
	case c.State.Health != nil && c.State.Health.Status != "healthy":
		return errors.New(name + " is " + c.State.Health.Status)
	}
	return nil
}
