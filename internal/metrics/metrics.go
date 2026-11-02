// Package metrics is orbit's Prometheus surface. Every job here is silent on
// success, as the fleet's other jobs are, so the last-success timestamp per
// agent is what an alert watches.
package metrics

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	runs = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_runs_total", Help: "Finished runs by agent and status.",
	}, []string{"agent", "status"})
	runDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name: "orbit_run_duration_seconds", Help: "Wall time of a run, claim to final state.",
		// A run spans a quick API call to a long multi-turn CLI session.
		Buckets: []float64{5, 15, 30, 60, 120, 300, 600, 1200, 1800, 3600},
	}, []string{"agent"})
	runCost = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_run_cost_usd_total", Help: "Cost the backend reported, by agent and runner kind. Subscription runs report an equivalent API cost.",
	}, []string{"agent", "runner"})
	lastSuccess = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orbit_agent_last_success_timestamp_seconds", Help: "Unix time of the agent's last run that succeeded or was skipped by a gate (a fire skipped as missed is not counted).",
	}, []string{"agent"})
	skipped = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_runs_skipped_total", Help: "Runs a gate ended without a backend call, by agent and gate.",
	}, []string{"agent", "gate"})
	deliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_deliveries_total", Help: "Notification attempts by channel and result.",
	}, []string{"channel", "result"})
	webhookDeliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_webhook_deliveries_total", Help: "Inbound webhook deliveries to a configured trigger, by agent, trigger and outcome (accepted, duplicate, ping, bad_signature, stale, malformed, too_large, disabled, unconfigured).",
	}, []string{"agent", "trigger", "outcome"})
	actionsExecuted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_actions_executed_total", Help: "Executed actions by agent, type and result.",
	}, []string{"agent", "type", "result"})
	// ConfigValid is 1 while the configuration on disk loads; 0 means orbit
	// is running the last good one and a change has not taken effect.
	ConfigValid = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_config_valid", Help: "1 if the configuration on disk loaded, 0 if orbit kept the previous one.",
	})
	// Draining is 1 once this instance drains (a deploy is replacing it).
	// It and the run counters are per instance; Pending and Queued are
	// read from the shared database, so every instance reports the same.
	Draining = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_draining", Help: "1 while this instance drains: it claims nothing new and reports not ready.",
	})
	// Pending and Queued are refreshed from the database by the server.
	Pending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_actions_pending", Help: "Actions waiting for a person's decision.",
	})
	Queued = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_runs_queued", Help: "Runs due and waiting for an executor (a continuation counts once it is due).",
	})
	// QueuedOldest is how long the oldest due queued run has waited: a queue nothing drains (no executor can claim it) grows this while orbit_runs_queued may stay small.
	QueuedOldest = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_runs_queued_oldest_age_seconds", Help: "Seconds the oldest due queued run has waited since it became due; 0 when none is waiting.",
	})
	credentialExpiry = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orbit_credential_expiry_timestamp_seconds", Help: "Unix time a credential stops working, as orbit.yaml's expires: records it.",
	}, []string{"credential"})
	// sandboxdReady has no labels; a vector so that it is absent when no sandboxd is configured.
	sandboxdReady = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orbit_sandboxd_ready", Help: "1 if sandboxd answered its readiness probe ready, 0 if not or unreachable.",
	}, nil)
	egressReady = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "orbit_egress_ready", Help: "1 if sandboxd reports the egress proxy container running (and healthy, with a healthcheck), by proxy host.",
	}, []string{"proxy"})
)

// SetExpiries replaces the credential expiry series with exp, so a removed entry stops being exported.
func SetExpiries(exp map[string]time.Time) {
	credentialExpiry.Reset()
	for name, t := range exp {
		credentialExpiry.WithLabelValues(name).Set(float64(t.Unix()))
	}
}

// SetSandbox records one probe of sandboxd: whether it is ready, and each egress proxy's readiness as sandboxd reported it, keyed by proxy host (nil when sandboxd did not answer, which exports none).
func SetSandbox(ready bool, egress map[string]bool) {
	sandboxdReady.WithLabelValues().Set(gauge(ready))
	egressReady.Reset()
	for proxy, ok := range egress {
		egressReady.WithLabelValues(proxy).Set(gauge(ok))
	}
}

// ClearSandbox removes the sandbox series: no sandboxd is configured.
func ClearSandbox() {
	sandboxdReady.Reset()
	egressReady.Reset()
}

func gauge(b bool) float64 {
	if b {
		return 1
	}
	return 0
}

// RunFinished records one run's end.
func RunFinished(agent, status, runner string, d time.Duration, cost *float64) {
	runs.WithLabelValues(agent, status).Inc()
	runDuration.WithLabelValues(agent).Observe(d.Seconds())
	if cost != nil && runner != "" {
		runCost.WithLabelValues(agent, runner).Add(*cost)
	}
	// A skipped run finished well: a gate found nothing to judge, so the
	// agent is not stale.
	if status == "succeeded" || status == "skipped" {
		lastSuccess.WithLabelValues(agent).SetToCurrentTime()
	}
}

// RunMissed records a schedule fire skipped as missed: a skipped run that, unlike a gate's, is no success.
func RunMissed(agent string, d time.Duration) {
	runs.WithLabelValues(agent, "skipped").Inc()
	runDuration.WithLabelValues(agent).Observe(d.Seconds())
}

// RunSkipped records which gate ended a run; RunFinished (or RunMissed) is
// still called with status skipped.
func RunSkipped(agent, gate string) {
	skipped.WithLabelValues(agent, gate).Inc()
}

// SeedLastSuccess sets an agent's gauge from the database at startup, so a
// restart does not read as "never succeeded".
func SeedLastSuccess(agent string, t time.Time) {
	lastSuccess.WithLabelValues(agent).Set(float64(t.Unix()))
}

// Delivery records one notification attempt.
func Delivery(channel string, err error) {
	deliveries.WithLabelValues(channel, result(err)).Inc()
}

// WebhookDelivery records one inbound delivery to a configured webhook trigger.
func WebhookDelivery(agent, trigger, outcome string) {
	webhookDeliveries.WithLabelValues(agent, trigger, outcome).Inc()
}

// ActionExecuted records one action execution.
func ActionExecuted(agent, typ string, err error) {
	actionsExecuted.WithLabelValues(agent, typ, result(err)).Inc()
}

func result(err error) string {
	if err != nil {
		return "failed"
	}
	return "ok"
}
