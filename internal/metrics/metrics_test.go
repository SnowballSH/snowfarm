package metrics

import (
	"slices"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
)

// The scrape config's metric_relabel keep-list and the farm's alert rules name
// these families literally, so a rename here is invisible until an alert stops
// evaluating. The list is the subset the dispatch loop and the hygiene sweep
// publish.
func TestRegistryExportsTheDeclaredFamilies(t *testing.T) {
	reg := New()
	reg.RunsStartedTotal.WithLabelValues("hestia").Inc()
	reg.RunsFinishedTotal.WithLabelValues("hestia", "done").Inc()
	reg.RunStopsTotal.WithLabelValues("hestia", "max_runtime").Inc()
	reg.CardsWithoutMaxRuntime.WithLabelValues("hestia").Set(1)
	reg.DispatchTicksTotal.WithLabelValues("hestia", "spawn").Inc()
	reg.DispatchSkippedTotal.WithLabelValues("farm_cap").Inc()
	reg.Cards.WithLabelValues("ready").Set(2)
	reg.CardsStranded.Set(1)
	reg.ProfileDrift.WithLabelValues("argus").Set(1)
	reg.NotifySubBlocksTotal.WithLabelValues("hestia", "atlas").Inc()
	reg.ManagerUp.WithLabelValues("atlas").Set(1)
	reg.ManagerManaged.WithLabelValues("atlas").Set(1)
	reg.ManagerRestartsTotal.WithLabelValues("atlas").Inc()
	reg.ManagerTurnsTotal.WithLabelValues("atlas").Inc()
	reg.ManagerPausesTotal.WithLabelValues("atlas", "turns").Inc()
	reg.AdapterTripRestartsTotal.WithLabelValues("atlas").Inc()
	reg.BurstStopsTotal.WithLabelValues("atlas").Inc()
	reg.BoardBytes.WithLabelValues("db").Set(4096)
	reg.PinDrift.Set(0)
	reg.ModelgateKeyAgeSeconds.WithLabelValues("atlas").Set(86400)
	reg.ModelgateReachable.Set(1)
	reg.GoogleTokenHealthy.Set(1)
	reg.ClaudeRunsTotal.WithLabelValues("hestia", "claude-opus-5").Inc()
	reg.ClaudeLimitHitsTotal.WithLabelValues("session").Inc()
	reg.ClaudeRunSecondsTotal.WithLabelValues("hestia").Add(12)

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	want := []string{
		"snowfarm_adapter_trip_restarts_total",
		"snowfarm_board_bytes",
		"snowfarm_burst_stops_total",
		"snowfarm_cards",
		"snowfarm_cards_stranded",
		"snowfarm_cards_without_max_runtime",
		"snowfarm_claude_limit_hits_total",
		"snowfarm_claude_run_seconds_total",
		"snowfarm_claude_runs_total",
		"snowfarm_dispatch_skipped_total",
		"snowfarm_dispatch_ticks_total",
		"snowfarm_google_token_healthy",
		"snowfarm_manager_managed",
		"snowfarm_manager_pauses_total",
		"snowfarm_manager_restarts_total",
		"snowfarm_manager_turns_total",
		"snowfarm_manager_up",
		"snowfarm_modelgate_key_age_seconds",
		"snowfarm_modelgate_reachable",
		"snowfarm_notify_sub_blocks_total",
		"snowfarm_pin_drift",
		"snowfarm_profile_drift",
		"snowfarm_run_stops_total",
		"snowfarm_runs_finished_total",
		"snowfarm_runs_in_flight",
		"snowfarm_runs_started_total",
	}
	slices.Sort(names)
	if !slices.Equal(names, want) {
		t.Fatalf("registry exports %v, want %v", names, want)
	}
}

// client_golang panics when a producer passes the wrong number of label
// values, so every tuple a guard component passes is exercised here rather
// than on the first dispatch tick of a live farm.
func TestRegistryLabelsMatchProducers(t *testing.T) {
	reg := New()
	counters := []struct {
		name   string
		vec    *prometheus.CounterVec
		labels []string
	}{
		{"runs_started_total", reg.RunsStartedTotal, []string{"hestia"}},
		{"runs_finished_total", reg.RunsFinishedTotal, []string{"hestia", "rate_limited"}},
		{"run_stops_total", reg.RunStopsTotal, []string{"hestia", "operator"}},
		{"dispatch_ticks_total", reg.DispatchTicksTotal, []string{"hestia", "housekeeping"}},
		{"dispatch_skipped_total", reg.DispatchSkippedTotal, []string{"no_json"}},
		{"notify_sub_blocks_total", reg.NotifySubBlocksTotal, []string{"hestia", "atlas"}},
		{"manager_restarts_total", reg.ManagerRestartsTotal, []string{"atlas"}},
		{"manager_turns_total", reg.ManagerTurnsTotal, []string{"atlas"}},
		{"manager_pauses_total", reg.ManagerPausesTotal, []string{"atlas", "operator_mentions"}},
		{"adapter_trip_restarts_total", reg.AdapterTripRestartsTotal, []string{"atlas"}},
		{"burst_stops_total", reg.BurstStopsTotal, []string{"atlas"}},
		{"claude_runs_total", reg.ClaudeRunsTotal, []string{"hestia", "claude-opus-5"}},
		{"claude_limit_hits_total", reg.ClaudeLimitHitsTotal, []string{"weekly"}},
		{"claude_run_seconds_total", reg.ClaudeRunSecondsTotal, []string{"hestia"}},
	}
	for _, c := range counters {
		if _, err := c.vec.GetMetricWithLabelValues(c.labels...); err != nil {
			t.Errorf("%s%v: %v", c.name, c.labels, err)
		}
	}
	gauges := []struct {
		name   string
		vec    *prometheus.GaugeVec
		labels []string
	}{
		{"cards", reg.Cards, []string{"running"}},
		{"cards_without_max_runtime", reg.CardsWithoutMaxRuntime, []string{"hestia"}},
		{"profile_drift", reg.ProfileDrift, []string{"argus"}},
		{"manager_up", reg.ManagerUp, []string{"atlas"}},
		{"manager_managed", reg.ManagerManaged, []string{"atlas"}},
		{"board_bytes", reg.BoardBytes, []string{"wal"}},
		{"modelgate_key_age_seconds", reg.ModelgateKeyAgeSeconds, []string{"atlas"}},
	}
	for _, g := range gauges {
		if _, err := g.vec.GetMetricWithLabelValues(g.labels...); err != nil {
			t.Errorf("%s%v: %v", g.name, g.labels, err)
		}
	}
}
