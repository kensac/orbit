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
		Name: "orbit_agent_last_success_timestamp_seconds", Help: "Unix time of the agent's last successful run.",
	}, []string{"agent"})
	deliveries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_deliveries_total", Help: "Notification attempts by channel and result.",
	}, []string{"channel", "result"})
	actionsExecuted = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "orbit_actions_executed_total", Help: "Executed actions by agent, type and result.",
	}, []string{"agent", "type", "result"})
	// ConfigValid is 1 while the configuration on disk loads; 0 means orbit
	// is running the last good one and a change has not taken effect.
	ConfigValid = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_config_valid", Help: "1 if the configuration on disk loaded, 0 if orbit kept the previous one.",
	})
	// Pending and Queued are refreshed from the database by the server.
	Pending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_actions_pending", Help: "Actions waiting for a person's decision.",
	})
	Queued = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "orbit_runs_queued", Help: "Runs due and waiting for an executor (a continuation counts once it is due).",
	})
)

// RunFinished records one run's end.
func RunFinished(agent, status, runner string, d time.Duration, cost *float64) {
	runs.WithLabelValues(agent, status).Inc()
	runDuration.WithLabelValues(agent).Observe(d.Seconds())
	if cost != nil && runner != "" {
		runCost.WithLabelValues(agent, runner).Add(*cost)
	}
	if status == "succeeded" {
		lastSuccess.WithLabelValues(agent).SetToCurrentTime()
	}
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
