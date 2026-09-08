package guard

import (
	"context"
	"slices"
	"syscall"
	"testing"
	"time"
)

func waitFor(t *testing.T, what string, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// The enabled flag is the phase gate, and a phase is `apply` then `reload`:
// apply provisions the host and signals nothing, so a reload that re-read only
// the age files would leave a newly provisioned agent with no dispatch pass
// and no schedule until the next restart.
func TestReloadPicksUpNewlyEnabledAgent(t *testing.T) {
	f := newGuardFixture(t, false)
	f.run(t)

	waitFor(t, "the first dispatch pass", func() bool { return f.units.count("run-dispatch hestia") > 0 })
	if got := f.units.count("run-dispatch argus"); got != 0 {
		t.Fatalf("a disabled agent was dispatched %d times", got)
	}
	if scheduled := f.guard.runner.Scheduled(); slices.Contains(scheduled, "snowsys-morning") {
		t.Fatalf("a schedule assigned to a disabled agent was registered: %v", scheduled)
	}

	f.writeConfig(t, true)
	f.hup <- syscall.SIGHUP

	waitFor(t, "argus to be dispatched", func() bool { return f.units.count("run-dispatch argus") > 0 })
	waitFor(t, "the morning check to be scheduled", func() bool {
		return slices.Contains(f.guard.runner.Scheduled(), "snowsys-morning")
	})
}

// A farm.yaml that no longer validates is a defect in the file, not a reason
// to leave the guard with no roster.
func TestReloadKeepsTheOldRosterWhenTheNewOneIsRefused(t *testing.T) {
	f := newGuardFixture(t, true)
	f.run(t)
	before := f.guard.roster.Load()

	write(t, f.config, "farm:\n  metrics_addr: 127.0.0.1:9110\nagents: []\n")
	if err := f.guard.Reload(context.Background()); err == nil {
		t.Fatal("a roster with no operator and no agents was accepted")
	}
	if after := f.guard.roster.Load(); after != before {
		t.Fatal("the refused roster replaced the one the guard was running")
	}
	waitFor(t, "dispatch to carry on", func() bool { return f.units.count("run-dispatch hestia") > 0 })
}
