package guard

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/snowfarm/internal/discord"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

type breakerEnv struct {
	roster *atomic.Pointer[roster.Roster]
	ledger *dispatch.Ledger
	units  *fakeUnits
	client *fakeClient
	posts  *recorder
	clock  *clock
	reg    *metrics.Registry
}

func newBreaker(t *testing.T) (*Breaker, *breakerEnv) {
	t.Helper()
	r := testRoster(t, t.TempDir())
	env := &breakerEnv{
		roster: held(r),
		ledger: openLedger(t),
		units:  newFakeUnits(),
		client: newFakeClient(),
		posts:  &recorder{},
		clock:  newClock(time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)),
		reg:    metrics.New(),
	}
	breaker := &Breaker{
		Roster:  env.roster,
		Units:   env.units,
		Journal: newFakeJournal(),
		Ledger:  env.ledger,
		Client:  env.client,
		Secrets: func(agent string) (map[string]string, bool) {
			return map[string]string{"DISCORD_BOT_TOKEN": "discord-token-for-" + agent}, true
		},
		Turns:   &Turns{},
		Post:    env.posts.post,
		Metrics: env.reg,
		Now:     env.clock.now,
	}
	return breaker, env
}

func TestTurnCounterPauses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		turns  int
		paused bool
	}{
		{name: "past the hourly bound", turns: 31, paused: true},
		{name: "under the hourly bound", turns: 29, paused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			breaker, env := newBreaker(t)
			appendLog(t, env.roster.Load(), "atlas", repeat(turnLine(testOperatorID), tc.turns)...)

			if err := breaker.Tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}

			stops := env.units.count("gateway atlas stop")
			pause, paused, err := env.ledger.Paused("atlas")
			if err != nil {
				t.Fatalf("read pause: %v", err)
			}
			if paused != tc.paused || (stops > 0) != tc.paused {
				t.Fatalf("%d turns: paused=%v, stops=%d, want paused=%v", tc.turns, paused, stops, tc.paused)
			}
			if !tc.paused {
				if posts := env.posts.all(); len(posts) > 0 {
					t.Fatalf("%d turns posted %v", tc.turns, posts)
				}
				return
			}
			if pause.Reason != pauseTurns {
				t.Fatalf("pause reason %q, want %q", pause.Reason, pauseTurns)
			}
			if want := env.clock.now().Add(pauseFor); !pause.Until.Equal(want) {
				t.Fatalf("pause expires %s, want %s", pause.Until, want)
			}
			if posts := env.posts.matching("atlas"); len(posts) != 1 {
				t.Fatalf("posts about atlas: %v", posts)
			}
		})
	}
}

// A manager pauses for thirty minutes and comes back on the tick after the
// row expires, with the counters that paused it cleared: a manager resumed
// with its hour of turns still counted would pause again at once.
func TestPauseExpiryResumes(t *testing.T) {
	breaker, env := newBreaker(t)
	appendLog(t, env.roster.Load(), "atlas", repeat(turnLine(testOperatorID), 31)...)
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	env.clock.advance(pauseFor - time.Minute)
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("tick before expiry: %v", err)
	}
	if starts := env.units.count("gateway atlas start"); starts != 0 {
		t.Fatalf("resumed %d times before the pause expired", starts)
	}

	env.clock.advance(2 * time.Minute)
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("tick after expiry: %v", err)
	}
	if starts := env.units.count("gateway atlas start"); starts != 1 {
		t.Fatalf("resume started the unit %d times, want 1", starts)
	}
	if _, paused, err := env.ledger.Paused("atlas"); err != nil || paused {
		t.Fatalf("pause row survived the resume: paused=%v err=%v", paused, err)
	}
}

func TestOperatorMentionsPause(t *testing.T) {
	for _, tc := range []struct {
		name     string
		mentions int
		paused   bool
	}{
		{name: "past the hourly bound", mentions: 7, paused: true},
		{name: "under the hourly bound", mentions: 5, paused: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			breaker, env := newBreaker(t)
			appendLog(t, env.roster.Load(), "atlas", repeat(mentionLine(testOperatorID), tc.mentions)...)

			if err := breaker.Tick(context.Background()); err != nil {
				t.Fatalf("tick: %v", err)
			}

			pause, paused, err := env.ledger.Paused("atlas")
			if err != nil {
				t.Fatalf("read pause: %v", err)
			}
			if paused != tc.paused {
				t.Fatalf("%d mentions: paused=%v, want %v", tc.mentions, paused, tc.paused)
			}
			if tc.paused && pause.Reason != pauseMentions {
				t.Fatalf("pause reason %q, want %q", pause.Reason, pauseMentions)
			}
		})
	}
}

// A mention of someone who is not the operator is ordinary traffic.
func TestOtherMentionsDoNotPause(t *testing.T) {
	breaker, env := newBreaker(t)
	appendLog(t, env.roster.Load(), "atlas", repeat(mentionLine(testStranger), 12)...)

	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if _, paused, err := env.ledger.Paused("atlas"); err != nil || paused {
		t.Fatalf("mentions of another user paused atlas: %v %v", paused, err)
	}
}

func TestBurstStop(t *testing.T) {
	breaker, env := newBreaker(t)
	appendLog(t, env.roster.Load(), "atlas", repeat(burstLine(), 21)...)

	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if stops := env.units.count("gateway atlas stop"); stops != 1 {
		t.Fatalf("burst stopped the unit %d times, want 1", stops)
	}
	pause, paused, err := env.ledger.Paused("atlas")
	if err != nil || !paused {
		t.Fatalf("burst left no pause row: %v %v", paused, err)
	}
	if !pause.Until.IsZero() {
		t.Fatalf("a burst stop expires at %s; it must not auto-resume", pause.Until)
	}
	env.clock.advance(2 * time.Hour)
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("later tick: %v", err)
	}
	if starts := env.units.count("gateway atlas start"); starts != 0 {
		t.Fatalf("a burst stop resumed itself after %d starts", starts)
	}
}

func TestAdapterTripRestart(t *testing.T) {
	t.Run("restarts on the manager's own budget", func(t *testing.T) {
		breaker, env := newBreaker(t)
		// The supervisor's own budget is exhausted: a breaker that asks with
		// the wrong token measures the wrong bot and refuses this restart.
		env.client.limits["discord-token-for-the-supervisor"] = discord.SessionStartLimit{Total: 1000, Remaining: 5}
		env.client.limits["discord-token-for-atlas"] = discord.SessionStartLimit{Total: 1000, Remaining: 120}
		appendLog(t, env.roster.Load(), "atlas", tripLine())

		if err := breaker.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if restarts := env.units.count("gateway atlas restart"); restarts != 1 {
			t.Fatalf("restarted %d times, want 1", restarts)
		}
	})

	t.Run("waits when the session budget is low", func(t *testing.T) {
		breaker, env := newBreaker(t)
		env.client.limits["discord-token-for-atlas"] = discord.SessionStartLimit{Total: 1000, Remaining: 40}
		appendLog(t, env.roster.Load(), "atlas", tripLine())

		if err := breaker.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		if restarts := env.units.count("gateway atlas restart"); restarts != 0 {
			t.Fatalf("restarted %d times on a low budget", restarts)
		}
		if posts := env.posts.matching("session"); len(posts) != 1 {
			t.Fatalf("posts about the session budget: %v", posts)
		}
	})

	t.Run("refuses the seventh trip in six hours", func(t *testing.T) {
		breaker, env := newBreaker(t)
		env.client.limits["discord-token-for-atlas"] = discord.SessionStartLimit{Total: 1000, Remaining: 900}
		for trip := range 7 {
			appendLog(t, env.roster.Load(), "atlas", tripLine())
			if err := breaker.Tick(context.Background()); err != nil {
				t.Fatalf("trip %d: %v", trip+1, err)
			}
			env.clock.advance(time.Minute)
		}
		if restarts := env.units.count("gateway atlas restart"); restarts != 6 {
			t.Fatalf("restarted %d times, want the budget of 6", restarts)
		}
		if posts := env.posts.matching("budget"); len(posts) != 1 {
			t.Fatalf("posts about the restart budget: %v", posts)
		}
	})
}

// The counters are exact at their bound, in both directions and for both
// signals: an off-by-one either pauses a manager that stayed inside its
// allowance or lets one past it.
func TestPauseThresholdsAreExact(t *testing.T) {
	r := testRoster(t, t.TempDir())
	for count := 27; count <= 33; count++ {
		breaker, env := newBreaker(t)
		appendLog(t, env.roster.Load(), "atlas", repeat(turnLine(testOperatorID), count)...)
		if err := breaker.Tick(context.Background()); err != nil {
			t.Fatalf("%d turns: %v", count, err)
		}
		_, paused, err := env.ledger.Paused("atlas")
		if err != nil {
			t.Fatalf("%d turns: %v", count, err)
		}
		if want := count >= r.Guard.ManagerTurnsPerHour; paused != want {
			t.Fatalf("%d turns paused=%v, want %v", count, paused, want)
		}
	}
	for count := 3; count <= 9; count++ {
		breaker, env := newBreaker(t)
		appendLog(t, env.roster.Load(), "atlas", repeat(mentionLine(testOperatorID), count)...)
		if err := breaker.Tick(context.Background()); err != nil {
			t.Fatalf("%d mentions: %v", count, err)
		}
		_, paused, err := env.ledger.Paused("atlas")
		if err != nil {
			t.Fatalf("%d mentions: %v", count, err)
		}
		if want := count >= r.Guard.OperatorMentionsPerHour; paused != want {
			t.Fatalf("%d mentions paused=%v, want %v", count, paused, want)
		}
	}
}

// A manager with no gateway log has never run; the breaker reads nothing and
// says nothing rather than reporting a missing file every thirty seconds.
func TestMissingGatewayLogIsQuiet(t *testing.T) {
	breaker, env := newBreaker(t)
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if posts := env.posts.all(); len(posts) > 0 {
		t.Fatalf("posted about a missing log: %v", posts)
	}
}

// The turn tracker pairs completions with starts, which is what the drained
// restarter reads. The default patterns overlap — a completion line also
// matches the start pattern — so the order the breaker tests them in is
// load-bearing.
func TestTurnsPairCompletionsWithStarts(t *testing.T) {
	breaker, env := newBreaker(t)
	appendLog(t, env.roster.Load(), "atlas", turnLine(testOperatorID))
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	if _, open := breaker.Turns.open("atlas"); !open {
		t.Fatal("a turn that started is not open")
	}
	appendLog(t, env.roster.Load(), "atlas", completeLine(testOperatorID))
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if _, open := breaker.Turns.open("atlas"); open {
		t.Fatal("a turn whose completion was logged is still open")
	}
}
