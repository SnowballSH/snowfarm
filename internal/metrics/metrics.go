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
	}
	r.MustRegister(
		r.RunsStartedTotal,
		r.RunsFinishedTotal,
		r.RunsInFlight,
		r.RunStopsTotal,
		r.CardsWithoutMaxRuntime,
		r.DispatchTicksTotal,
		r.DispatchSkippedTotal,
	)
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
