package metrics

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// The expiry and sandbox series exist only for what is configured: a removed entry, or a sandboxd that went away from the configuration, stops being exported.
func TestConfiguredSeries(t *testing.T) {
	SetExpiries(map[string]time.Time{"A": time.Unix(100, 0), "B": time.Unix(200, 0)})
	if testutil.CollectAndCount(credentialExpiry) != 2 || testutil.ToFloat64(credentialExpiry.WithLabelValues("B")) != 200 {
		t.Fatal("expiries not exported")
	}
	SetExpiries(map[string]time.Time{"A": time.Unix(100, 0)})
	if testutil.CollectAndCount(credentialExpiry) != 1 {
		t.Fatal("a removed expiry is still exported")
	}

	SetSandbox(true, map[string]bool{"orbit-egress": true, "orbit-egress-open": false})
	if testutil.ToFloat64(sandboxdReady.WithLabelValues()) != 1 || testutil.CollectAndCount(egressReady) != 2 ||
		testutil.ToFloat64(egressReady.WithLabelValues("orbit-egress-open")) != 0 {
		t.Fatal("sandbox not exported")
	}
	SetSandbox(false, nil)
	if testutil.ToFloat64(sandboxdReady.WithLabelValues()) != 0 || testutil.CollectAndCount(egressReady) != 0 {
		t.Fatal("an unanswered probe kept egress series")
	}
	ClearSandbox()
	if testutil.CollectAndCount(sandboxdReady) != 0 {
		t.Fatal("no sandboxd configured, yet orbit_sandboxd_ready is exported")
	}
}
