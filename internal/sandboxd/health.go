package sandboxd

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
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

// Readiness is whether sandboxd can make a sandbox now: the daemon answers, the sandbox network is still internal, and each egress proxy's container is running (and healthy, when it has a healthcheck). A proxy is checked by the container named as its host, which is how compose names it on the network. Answers are cached for readyTTL.
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
	if n, err := s.d.network(ctx, s.cfg.Network); err != nil {
		fail("network", err)
	} else if !n.Internal {
		fail("network", errors.New(s.cfg.Network+" is not internal"))
	} else {
		rd.Checks["network"] = "ok"
	}
	for _, p := range []string{s.cfg.Proxy, s.cfg.WorkstationProxy} {
		host, _, err := net.SplitHostPort(p)
		if p == "" || err != nil {
			continue
		}
		if err := s.proxyUp(ctx, host); err != nil {
			fail(sandbox.ProxyCheck(host), err)
		} else {
			rd.Checks[sandbox.ProxyCheck(host)] = "ok"
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
