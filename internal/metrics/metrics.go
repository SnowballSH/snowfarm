// Package metrics holds the one Prometheus registry the guard exports on the
// farm's tailnet address. Every family the farm publishes is registered here
// and nowhere else, so a label set a producer passes is checked against the
// declaration a scrape config and an alert rule were written against.
package metrics

import "github.com/prometheus/client_golang/prometheus"

const namespace = "snowfarm"

type Registry struct {
	*prometheus.Registry

	RunsStartedTotal       *prometheus.CounterVec
	RunsFinishedTotal      *prometheus.CounterVec
	RunsInFlight           prometheus.Gauge
	RunStopsTotal          *prometheus.CounterVec
	CardsWithoutMaxRuntime *prometheus.GaugeVec
	DispatchTicksTotal     *prometheus.CounterVec
	DispatchSkippedTotal   *prometheus.CounterVec
	Cards                  *prometheus.GaugeVec
	CardsStranded          prometheus.Gauge
	ProfileDrift           *prometheus.GaugeVec
	NotifySubBlocksTotal   *prometheus.CounterVec

	ManagerUp                *prometheus.GaugeVec
	ManagerManaged           *prometheus.GaugeVec
	ManagerRestartsTotal     *prometheus.CounterVec
	ManagerTurnsTotal        *prometheus.CounterVec
	ManagerPausesTotal       *prometheus.CounterVec
	AdapterTripRestartsTotal *prometheus.CounterVec
	BurstStopsTotal          *prometheus.CounterVec

	BoardBytes             *prometheus.GaugeVec
	PinDrift               prometheus.Gauge
	ModelgateKeyAgeSeconds *prometheus.GaugeVec
	ModelgateReachable     prometheus.Gauge
	GoogleTokenHealthy     prometheus.Gauge

	ClaudeRunsTotal       *prometheus.CounterVec
	ClaudeLimitHitsTotal  *prometheus.CounterVec
	ClaudeRunSecondsTotal *prometheus.CounterVec
}

func New() *Registry {
	r := &Registry{
		Registry: prometheus.NewRegistry(),
		RunsStartedTotal: counter("runs_started_total",
			"Board cards a dispatch pass spawned a worker for.", "agent"),
		RunsFinishedTotal: counter("runs_finished_total",
			"Runs that left the board's running set, by the outcome the guard resolved.", "agent", "outcome"),
		RunsInFlight: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "runs_in_flight",
			Help:      "Cards the board reports running at the last dispatch tick.",
		}),
		RunStopsTotal: counter("run_stops_total",
			"Runs the guard terminated itself, by the trigger that fired.", "agent", "reason"),
		CardsWithoutMaxRuntime: gauge("cards_without_max_runtime",
			"Running cards created with no max_runtime, on which the guard enforces its own default.", "agent"),
		DispatchTicksTotal: counter("dispatch_ticks_total",
			"Dispatch passes the guard issued, by kind.", "agent", "kind"),
		DispatchSkippedTotal: counter("dispatch_skipped_total",
			"Dispatch opportunities the guard passed over, by reason.", "reason"),
		Cards: gauge("cards",
			"Cards on the board at the last hygiene sweep, by status.", "status"),
		CardsStranded: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "cards_stranded",
			Help:      "Ready cards the farm had free capacity to claim and did not.",
		}),
		ProfileDrift: gauge("profile_drift",
			"Agents whose rendered profile no longer matches the baseline, or could not be read.", "agent"),
		NotifySubBlocksTotal: counter("notify_sub_blocks_total",
			"Notifier subscriptions the hygiene sweep removed, by the worker that forged one and the manager it named.", "worker", "notifier"),
		ManagerUp: gauge("manager_up",
			"Manager gateways whose unit the agent's own user manager reports active.", "agent"),
		ManagerManaged: gauge("manager_managed",
			"Manager gateways the farm is running: installed with resolved channel ids and not paused.", "agent"),
		ManagerRestartsTotal: counter("manager_restarts_total",
			"Manager gateway restarts the guard performed, drained and adapter-trip alike.", "agent"),
		ManagerTurnsTotal: counter("manager_turns_total",
			"Mention-bearing turns a manager handled, counted from its gateway log.", "agent"),
		ManagerPausesTotal: counter("manager_pauses_total",
			"Manager pauses the guard imposed, by the signal that fired.", "agent", "reason"),
		AdapterTripRestartsTotal: counter("adapter_trip_restarts_total",
			"Restarts the guard performed on a Hermes adapter circuit-breaker trip.", "agent"),
		BurstStopsTotal: counter("burst_stops_total",
			"Manager gateways stopped for a burst of Discord 401, 403 or 429 responses.", "agent"),
		BoardBytes: gauge("board_bytes",
			"Size of the Kanban database and its write-ahead log.", "file"),
		PinDrift: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "pin_drift",
			Help:      "1 when the installed Hermes does not report the pinned commit.",
		}),
		ModelgateKeyAgeSeconds: gauge("modelgate_key_age_seconds",
			"Age of each agent's modelgate key, from the mint date the roster records.", "agent"),
		ModelgateReachable: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "modelgate_reachable",
			Help:      "1 when the model gateway answered the supervisor's last liveness probe.",
		}),
		GoogleTokenHealthy: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "google_token_healthy",
			Help:      "0 when Google refused the stored refresh token; 1 while it is honoured, and while there is no token to check.",
		}),
		ClaudeRunsTotal: counter("claude_runs_total",
			"Claude Code runs the wrapper recorded, by the agent that ran one and the model it asked for.", "agent", "model"),
		ClaudeLimitHitsTotal: counter("claude_limit_hits_total",
			"Claude Code subscription limits the wrapper hit, by lowercased kind: session, weekly, opus, sonnet, fable, credits or unknown.", "kind"),
		ClaudeRunSecondsTotal: counter("claude_run_seconds_total",
			"Seconds the farm spent inside Claude Code runs, by agent.", "agent"),
	}
	r.MustRegister(
		r.RunsStartedTotal,
		r.RunsFinishedTotal,
		r.RunsInFlight,
		r.RunStopsTotal,
		r.CardsWithoutMaxRuntime,
		r.DispatchTicksTotal,
		r.DispatchSkippedTotal,
		r.Cards,
		r.CardsStranded,
		r.ProfileDrift,
		r.NotifySubBlocksTotal,
		r.ManagerUp,
		r.ManagerManaged,
		r.ManagerRestartsTotal,
		r.ManagerTurnsTotal,
		r.ManagerPausesTotal,
		r.AdapterTripRestartsTotal,
		r.BurstStopsTotal,
		r.BoardBytes,
		r.PinDrift,
		r.ModelgateKeyAgeSeconds,
		r.ModelgateReachable,
		r.GoogleTokenHealthy,
		r.ClaudeRunsTotal,
		r.ClaudeLimitHitsTotal,
		r.ClaudeRunSecondsTotal,
	)
	// A gauge is born at zero, and zero is this family's alerting value: a
	// farm whose weekly probe has not run yet, or has nothing to check, must
	// not report a refused Google grant from the moment it starts.
	r.GoogleTokenHealthy.Set(1)
	return r
}

func counter(name, help string, labels ...string) *prometheus.CounterVec {
	return prometheus.NewCounterVec(prometheus.CounterOpts{
		Namespace: namespace,
		Name:      name,
		Help:      help,
	}, labels)
}

func gauge(name, help string, labels ...string) *prometheus.GaugeVec {
	return prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      name,
		Help:      help,
	}, labels)
}
