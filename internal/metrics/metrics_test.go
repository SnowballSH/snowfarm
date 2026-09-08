package metrics

import (
	"slices"
	"testing"
)

// The scrape config's metric_relabel keep-list and the farm's alert rules name
// these families literally, so a rename here is invisible until an alert stops
// evaluating. The list is the subset the dispatch loop publishes.
func TestRegistryExportsTheDeclaredFamilies(t *testing.T) {
	reg := New()
	reg.RunsStartedTotal.WithLabelValues("hestia").Inc()
	reg.RunsFinishedTotal.WithLabelValues("hestia", "done").Inc()
	reg.RunStopsTotal.WithLabelValues("hestia", "max_runtime").Inc()
	reg.CardsWithoutMaxRuntime.WithLabelValues("hestia").Set(1)
	reg.DispatchTicksTotal.WithLabelValues("hestia", "spawn").Inc()
	reg.DispatchSkippedTotal.WithLabelValues("farm_cap").Inc()

	families, err := reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var names []string
	for _, family := range families {
		names = append(names, family.GetName())
	}
	want := []string{
		"snowfarm_cards_without_max_runtime",
		"snowfarm_dispatch_skipped_total",
		"snowfarm_dispatch_ticks_total",
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
