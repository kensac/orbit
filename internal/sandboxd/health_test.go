package sandboxd

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/kensac/orbit/internal/sandbox"
)

func get(t *testing.T, sock, path string) (int, string) {
	t.Helper()
	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req, _ := http.NewRequest(http.MethodGet, "http://x"+path, nil)
	_ = req.Write(conn)
	resp, err := http.ReadResponse(bufioReader(conn), req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// The probes answer without the token; readiness names each check, reports each egress and credential proxy container by its name, and is cached so probes do not each reach docker.
func TestReadiness(t *testing.T) {
	h := newHarness(t)
	h.srv.cfg.WorkstationProxy = "orbit-egress-open:3128"
	h.srv.cfg.WorkstationNetwork = "orbit-workstation"
	// One credential proxy, by name on one network and by IP on the other.
	h.srv.cfg.WorkstationCredProxy = "10.250.252.3:3129"
	h.fd.containers["orbit-credproxy"] = &fakeContainer{running: true}
	now := time.Unix(1_800_000_000, 0)
	h.srv.now = func() time.Time { return now }
	h.fd.containers["orbit-egress"] = &fakeContainer{running: true, health: "healthy"}

	if code, body := get(t, h.sock, sandbox.PathHealthz); code != http.StatusOK || body != "ok\n" {
		t.Fatalf("healthz = %d %q", code, body)
	}
	code, body := get(t, h.sock, sandbox.PathReadyz)
	if code != http.StatusServiceUnavailable || !strings.Contains(body, `"proxy:orbit-egress-open":"no container named orbit-egress-open"`) || !strings.Contains(body, `"proxy:orbit-egress":"ok"`) {
		t.Fatalf("readyz = %d %s", code, body)
	}
	rd, err := h.client.Ready(context.Background())
	if err != nil || rd.Ready || rd.Checks["docker"] != "ok" || rd.Checks["network"] != "ok" || rd.Checks["workstation_network"] != "ok" ||
		rd.Checks["credproxy:orbit-credproxy"] != "ok" || rd.Checks["credproxy:10.250.252.3"] != "unchecked: addressed by IP" {
		t.Fatalf("ready = %+v %v", rd, err)
	}
	if p := rd.Proxies(); len(p) != 2 || !p["orbit-egress"] || p["orbit-egress-open"] {
		t.Fatalf("proxies = %v", p)
	}

	// Cached: a fixed proxy is not seen until readyTTL passes.
	h.fd.containers["orbit-egress-open"] = &fakeContainer{running: true}
	if rd, _ := h.client.Ready(context.Background()); rd.Ready {
		t.Fatal("answer was not cached")
	}
	now = now.Add(readyTTL)
	if rd, _ := h.client.Ready(context.Background()); !rd.Ready {
		t.Fatalf("after the TTL: %+v", rd)
	}

	// An unhealthy proxy, a stopped credential proxy, or a network that stopped being internal, is not ready.
	h.fd.containers["orbit-egress"].health = "unhealthy"
	h.fd.containers["orbit-credproxy"].running = false
	h.srv.cfg.Network = "bridge-ish"
	h.srv.cfg.WorkstationNetwork = "open-ish"
	now = now.Add(readyTTL)
	rd, _ = h.client.Ready(context.Background())
	if rd.Ready || rd.Checks["proxy:orbit-egress"] != "orbit-egress is unhealthy" || rd.Checks["network"] != "bridge-ish is not internal" ||
		rd.Checks["workstation_network"] != "open-ish is not internal" || rd.Checks["credproxy:orbit-credproxy"] != "orbit-credproxy is not running" {
		t.Fatalf("ready = %+v", rd)
	}

	// Every other route still needs the token.
	if code, _ := get(t, h.sock, sandbox.PathSandboxes+"/"+strings.Repeat("a", 64)+"/archive"); code != http.StatusUnauthorized {
		t.Fatalf("archive without a token = %d", code)
	}
}
