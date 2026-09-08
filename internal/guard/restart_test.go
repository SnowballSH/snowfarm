package guard

import (
	"context"
	"testing"
	"time"

	"github.com/SnowballSH/snowfarm/internal/metrics"
)

func newRestarter(t *testing.T, at time.Time) (*Restarter, *breakerEnv) {
	t.Helper()
	env := &breakerEnv{
		roster: held(testRoster(t, t.TempDir())),
		ledger: openLedger(t),
		units:  newFakeUnits(),
		posts:  &recorder{},
		clock:  newClock(at),
		reg:    metrics.New(),
	}
	restarter := &Restarter{
		Roster:  env.roster,
		Units:   env.units,
		Ledger:  env.ledger,
		Turns:   &Turns{},
		Post:    env.posts.post,
		Metrics: env.reg,
		Now:     env.clock.now,
	}
	return restarter, env
}

func inWindow(hour, minute int) time.Time {
	return time.Date(2026, 9, 7, hour, minute, 0, 0, time.UTC)
}

// Quiescence is "no active turn", not "no recent turn-start": a manager turn
// runs for up to HERMES_AGENT_TIMEOUT, so a start line hours old proves
// nothing about whether the turn is still running, and restarting on its age
// aborts the turn.
func TestDrainedRestartWaitsForQuiet(t *testing.T) {
	t.Run("a completed turn is quiet", func(t *testing.T) {
		restarter, env := newRestarter(t, inWindow(4, 10))
		restarter.Turns.started("atlas", inWindow(4, 5))
		restarter.Turns.completed("atlas")
		restarter.Turns.started("iris", inWindow(4, 6))

		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if got := env.units.count("gateway atlas restart"); got != 1 {
			t.Fatalf("atlas restarted %d times, want 1", got)
		}
		if got := env.units.count("gateway iris restart"); got != 0 {
			t.Fatalf("iris restarted %d times while its turn was open", got)
		}
	})

	t.Run("an old uncompleted turn is not quiet", func(t *testing.T) {
		restarter, env := newRestarter(t, inWindow(4, 55))
		restarter.Turns.started("atlas", inWindow(2, 0))
		restarter.Turns.started("iris", inWindow(2, 0))

		for range 3 {
			if err := restarter.Tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}
			env.clock.advance(time.Minute)
		}
		if got := env.units.made(); len(got) != 0 {
			t.Fatalf("a turn-start line two hours old was read as quiet: %v", got)
		}
	})

	t.Run("the window closing restarts anyway", func(t *testing.T) {
		restarter, env := newRestarter(t, inWindow(4, 30))
		restarter.Turns.started("atlas", inWindow(4, 0))
		restarter.Turns.started("iris", inWindow(4, 0))
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("tick inside the window: %v", err)
		}
		if got := env.units.made(); len(got) != 0 {
			t.Fatalf("restarted inside the window with turns open: %v", got)
		}

		env.clock.advance(30 * time.Minute)
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("tick at the window's end: %v", err)
		}
		env.clock.advance(restartSpacing)
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("second tick at the window's end: %v", err)
		}
		for _, manager := range []string{"atlas", "iris"} {
			if got := env.units.count("gateway " + manager + " restart"); got != 1 {
				t.Fatalf("%s restarted %d times once the window closed, want 1", manager, got)
			}
		}
		if posts := env.posts.matching("window"); len(posts) != 2 {
			t.Fatalf("posts naming the closed window: %v", env.posts.all())
		}
	})

	t.Run("two managers restart two minutes apart", func(t *testing.T) {
		restarter, env := newRestarter(t, inWindow(4, 0))
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("first tick: %v", err)
		}
		if got := len(env.units.made()); got != 1 {
			t.Fatalf("first tick restarted %d managers, want 1", got)
		}
		env.clock.advance(restartSpacing - time.Second)
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("second tick: %v", err)
		}
		if got := len(env.units.made()); got != 1 {
			t.Fatalf("a second manager restarted inside the spacing: %v", env.units.made())
		}
		env.clock.advance(2 * time.Second)
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("third tick: %v", err)
		}
		if got := len(env.units.made()); got != 2 {
			t.Fatalf("after the spacing %d managers had restarted, want 2: %v", got, env.units.made())
		}
	})
}

// One restart a night: a tick every thirty seconds must not restart a manager
// it has already restarted in this window.
func TestDrainedRestartHappensOncePerWindow(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(4, 0))
	for range 6 {
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		env.clock.advance(restartSpacing)
	}
	for _, manager := range []string{"atlas", "iris"} {
		if got := env.units.count("gateway " + manager + " restart"); got != 1 {
			t.Fatalf("%s restarted %d times in one window, want 1", manager, got)
		}
	}
}

// Outside the window the restarter does nothing at all.
func TestDrainedRestartStaysInsideItsWindow(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(12, 0))
	if err := restarter.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := env.units.made(); len(got) != 0 {
		t.Fatalf("restarted outside the window: %v", got)
	}
}

// A manager the breaker or the operator stopped stays stopped: restarting it
// would undo the pause the guard imposed.
func TestDrainedRestartLeavesPausedManagers(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(4, 0))
	if err := env.ledger.RecordPause("atlas", pauseBurst, env.clock.now(), time.Time{}); err != nil {
		t.Fatalf("record pause: %v", err)
	}
	for range 3 {
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		env.clock.advance(restartSpacing)
	}
	if got := env.units.count("gateway atlas restart"); got != 0 {
		t.Fatalf("a paused manager was restarted %d times", got)
	}
	if got := env.units.count("gateway iris restart"); got != 1 {
		t.Fatalf("iris restarted %d times, want 1", got)
	}
}

// The completion pattern is what makes "quiet" mean anything. Until F2 pins
// it from a real gateway.log the restarter refuses to arm rather than
// restarting managers on a signal it does not have.
func TestDrainedRestartRefusesWithoutACompletionPattern(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(4, 0))
	r := testRoster(t, t.TempDir())
	r.Guard.TurnCompletePattern = ""
	env.roster.Store(r)

	if err := restarter.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := env.units.made(); len(got) != 0 {
		t.Fatalf("restarted with no completion pattern to prove quiet: %v", got)
	}
}
