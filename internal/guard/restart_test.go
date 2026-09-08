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
		roster: held(armedRoster(t, t.TempDir())),
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
		Turns:   &Turns{Store: env.ledger},
		Post:    env.posts.post,
		Metrics: env.reg,
		Now:     env.clock.now,
	}
	return restarter, env
}

func startTurn(t *testing.T, turns *Turns, agent string, at time.Time) {
	t.Helper()
	if err := turns.started(agent, at); err != nil {
		t.Fatalf("start a turn of %s: %v", agent, err)
	}
}

func completeTurn(t *testing.T, turns *Turns, agent string) {
	t.Helper()
	if err := turns.completed(agent); err != nil {
		t.Fatalf("complete a turn of %s: %v", agent, err)
	}
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
		startTurn(t, restarter.Turns, "atlas", inWindow(4, 5))
		completeTurn(t, restarter.Turns, "atlas")
		startTurn(t, restarter.Turns, "iris", inWindow(4, 6))

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
		startTurn(t, restarter.Turns, "atlas", inWindow(2, 0))
		startTurn(t, restarter.Turns, "iris", inWindow(2, 0))

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
		startTurn(t, restarter.Turns, "atlas", inWindow(4, 0))
		startTurn(t, restarter.Turns, "iris", inWindow(4, 0))
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

// The shipped roster does not arm the nightly restart, and until F2 pins the
// turn patterns from a real gateway.log it must not: nothing would match, every
// manager would read as quiet, and both gateways would be restarted mid-turn
// at 04:00 every night. This is the state a host loads, not one a test builds.
func TestDrainedRestartIsDisarmedInTheShippedRoster(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(4, 0))
	env.roster.Store(testRoster(t, t.TempDir()))

	for range 3 {
		if err := restarter.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
	}
	if got := env.units.made(); len(got) != 0 {
		t.Fatalf("a disarmed restarter restarted something: %v", got)
	}
}

// Arming without a completion pattern is a load error, so the restarter meets
// that roster only from a caller that built one by hand; it still refuses.
func TestDrainedRestartRefusesWithoutACompletionPattern(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(4, 0))
	r := armedRoster(t, t.TempDir())
	r.Guard.TurnCompletePattern = ""
	env.roster.Store(r)

	if err := restarter.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := env.units.made(); len(got) != 0 {
		t.Fatalf("restarted with no completion pattern to prove quiet: %v", got)
	}
}

// The open turns a guard read are the restarter's only proof that a manager
// is between them, and the offset they were read from is durable: a restarted
// guard never re-reads those lines. Kept in memory alone they would be empty
// after every restart, and the restarter would abort a turn that is still
// running — the failure the drained restart exists to prevent (S19).
func TestOpenTurnsSurviveAGuardRestart(t *testing.T) {
	restarter, env := newRestarter(t, inWindow(4, 10))
	startTurn(t, restarter.Turns, "atlas", inWindow(3, 55))
	startTurn(t, restarter.Turns, "iris", inWindow(3, 55))
	completeTurn(t, restarter.Turns, "iris")

	restarter.Turns = &Turns{Store: env.ledger}

	if err := restarter.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if got := env.units.count("gateway atlas restart"); got != 0 {
		t.Fatalf("a restarted guard aborted atlas's open turn: %d restarts", got)
	}
	if got := env.units.count("gateway iris restart"); got != 1 {
		t.Fatalf("iris restarted %d times, want 1", got)
	}

	if err := restarter.Turns.reset("atlas"); err != nil {
		t.Fatalf("reset atlas's turns: %v", err)
	}
	if open, err := env.ledger.OpenTurns("atlas"); err != nil || len(open) != 0 {
		t.Fatalf("a restart left %v open turns behind: %v", open, err)
	}
}
