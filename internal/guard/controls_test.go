package guard

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/discord"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

const controlChannel = "400000000000000009"

type controlEnv struct {
	roster *atomic.Pointer[roster.Roster]
	ledger *dispatch.Ledger
	units  *fakeUnits
	posts  *recorder
	clock  *clock
	loop   *dispatch.Loop
}

func newControls(t *testing.T) (*Controls, *controlEnv) {
	t.Helper()
	r := testRoster(t, t.TempDir())
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	env := &controlEnv{
		roster: held(r),
		ledger: openLedger(t),
		units:  newFakeUnits(),
		posts:  &recorder{},
		clock:  newClock(now),
	}
	uids := map[string]int{}
	for _, agent := range r.Agents {
		uids[agent.Name] = r.UID(agent)
	}
	var held atomic.Pointer[map[string]int]
	held.Store(&uids)

	reg := metrics.New()
	env.loop = &dispatch.Loop{
		Roster:   env.roster,
		UIDs:     &held,
		Board:    openBoard(t, board.RunningCard{ID: "t_1", Assignee: "hestia", WorkerPID: 4242, StartedAt: now.Add(-10 * time.Minute), MaxRuntime: 2 * time.Hour}),
		Units:    env.units,
		Journal:  newFakeJournal(dispatchResult()),
		Ledger:   env.ledger,
		Post:     env.posts.post,
		Metrics:  reg,
		Now:      env.clock.now,
		PassWait: time.Millisecond,
	}
	breaker := &Breaker{
		Roster:  env.roster,
		Units:   env.units,
		Ledger:  env.ledger,
		Turns:   &Turns{},
		Post:    env.posts.post,
		Metrics: reg,
		Now:     env.clock.now,
	}
	controls := &Controls{
		Roster:   env.roster,
		Board:    env.loop.Board,
		Loop:     env.loop,
		Ledger:   env.ledger,
		Breaker:  breaker,
		Post:     env.posts.post,
		Now:      env.clock.now,
		Channel:  controlChannel,
		Operator: testOperatorID,
	}
	return controls, env
}

func control(author, text string) discord.Event {
	return discord.Event{
		Kind:      discord.EventMessageCreate,
		ChannelID: controlChannel,
		AuthorID:  author,
		Content:   text,
	}
}

func TestOnlyOperatorCommands(t *testing.T) {
	t.Run("pause obeys the operator and nobody else", func(t *testing.T) {
		controls, env := newControls(t)
		if err := controls.Handle(context.Background(), control(testStranger, "pause atlas")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if calls := env.units.made(); len(calls) != 0 {
			t.Fatalf("a command from another author acted: %v", calls)
		}
		if posts := env.posts.all(); len(posts) != 0 {
			t.Fatalf("a command from another author posted: %v", posts)
		}

		if err := controls.Handle(context.Background(), control(testOperatorID, "pause atlas")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if got := env.units.count("gateway atlas stop"); got != 1 {
			t.Fatalf("pause stopped the gateway %d times, want 1", got)
		}
		pause, paused, err := env.ledger.Paused("atlas")
		if err != nil || !paused {
			t.Fatalf("pause left no row: %v %v", paused, err)
		}
		if pause.Reason != pauseOperator {
			t.Fatalf("pause reason %q, want %q", pause.Reason, pauseOperator)
		}
		if posts := env.posts.matching("atlas"); len(posts) != 1 {
			t.Fatalf("posts about the pause: %v", env.posts.all())
		}
	})

	t.Run("a command outside the control channel is ignored", func(t *testing.T) {
		controls, env := newControls(t)
		event := control(testOperatorID, "pause atlas")
		event.ChannelID = "400000000000000001"
		if err := controls.Handle(context.Background(), event); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if calls := env.units.made(); len(calls) != 0 {
			t.Fatalf("a command in another channel acted: %v", calls)
		}
	})

	// resume carries state: it clears the row a thirty-minute breaker pause
	// wrote, so a manually resumed manager stays up until the breaker trips
	// again.
	t.Run("resume clears a breaker pause and starts the unit", func(t *testing.T) {
		controls, env := newControls(t)
		until := env.clock.now().Add(pauseFor)
		if err := env.ledger.RecordPause("atlas", pauseTurns, env.clock.now(), until); err != nil {
			t.Fatalf("record pause: %v", err)
		}
		if err := controls.Handle(context.Background(), control(testOperatorID, "resume atlas")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if _, paused, err := env.ledger.Paused("atlas"); err != nil || paused {
			t.Fatalf("resume left the pause row: %v %v", paused, err)
		}
		if got := env.units.count("gateway atlas start"); got != 1 {
			t.Fatalf("resume started the gateway %d times, want 1", got)
		}
		if posts := env.posts.matching("atlas"); len(posts) != 1 {
			t.Fatalf("posts about the resume: %v", env.posts.all())
		}
	})

	// A bare RunStop and reclaim returns the card to ready with a cleared
	// failure counter, and the next spawn pass re-runs it within a tick: the
	// block is what makes the operator's stop hold.
	t.Run("stop ends the run, reclaims and blocks the card", func(t *testing.T) {
		controls, env := newControls(t)
		unit := "snowfarm-run-hestia-1.service"
		if err := env.ledger.RecordSpawn("hestia", unit, "t_1", env.clock.now()); err != nil {
			t.Fatalf("record spawn: %v", err)
		}
		env.units.units["hestia"] = []string{unit}

		if err := controls.Handle(context.Background(), control(testOperatorID, "stop t_1")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		for _, call := range []string{
			"run-stop hestia " + unit,
			"kanban hestia reclaim t_1",
		} {
			if got := env.units.count(call); got != 1 {
				t.Fatalf("%q happened %d times, want 1: %v", call, got, env.units.made())
			}
		}
		blocks := 0
		for _, call := range env.units.made() {
			if strings.HasPrefix(call, "kanban hestia block t_1 ") && strings.HasSuffix(call, "--kind transient") {
				blocks++
			}
		}
		if blocks != 1 {
			t.Fatalf("the card was blocked %d times as transient: %v", blocks, env.units.made())
		}
	})

	t.Run("stop names a card the board is not running", func(t *testing.T) {
		controls, env := newControls(t)
		if err := controls.Handle(context.Background(), control(testOperatorID, "stop t_404")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if calls := env.units.made(); len(calls) != 0 {
			t.Fatalf("stopping an unknown card acted: %v", calls)
		}
		if posts := env.posts.matching("t_404"); len(posts) != 1 {
			t.Fatalf("posts about the unknown card: %v", env.posts.all())
		}
	})

	t.Run("status reports the board, the runs and the pauses", func(t *testing.T) {
		controls, env := newControls(t)
		if err := env.ledger.RecordPause("iris", pauseBurst, env.clock.now(), time.Time{}); err != nil {
			t.Fatalf("record pause: %v", err)
		}
		if err := controls.Handle(context.Background(), control(testOperatorID, "status")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		posts := env.posts.all()
		if len(posts) != 1 {
			t.Fatalf("status posted %v", posts)
		}
		for _, want := range []string{"running 1", "iris", pauseBurst} {
			if !strings.Contains(posts[0], want) {
				t.Fatalf("status post %q does not carry %q", posts[0], want)
			}
		}
	})

	t.Run("runs lists what is in flight", func(t *testing.T) {
		controls, env := newControls(t)
		if err := env.loop.Tick(context.Background()); err != nil {
			t.Fatalf("tick: %v", err)
		}
		env.posts.posts = nil
		if err := controls.Handle(context.Background(), control(testOperatorID, "runs")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		posts := env.posts.all()
		if len(posts) != 1 {
			t.Fatalf("runs posted %v", posts)
		}
		for _, want := range []string{"t_1", "hestia", "10m"} {
			if !strings.Contains(posts[0], want) {
				t.Fatalf("runs post %q does not carry %q", posts[0], want)
			}
		}
	})

	t.Run("an unknown command is answered, not obeyed", func(t *testing.T) {
		controls, env := newControls(t)
		if err := controls.Handle(context.Background(), control(testOperatorID, "deploy everything")); err != nil {
			t.Fatalf("handle: %v", err)
		}
		if calls := env.units.made(); len(calls) != 0 {
			t.Fatalf("an unknown command acted: %v", calls)
		}
		if posts := env.posts.all(); len(posts) != 1 {
			t.Fatalf("an unknown command posted %v", posts)
		}
	})

	// pause and resume name a manager: a worker has no gateway to stop, and a
	// name outside the roster is a typo the operator should see.
	t.Run("pause refuses anything but an enabled manager", func(t *testing.T) {
		controls, env := newControls(t)
		for _, name := range []string{"hestia", "nobody"} {
			if err := controls.Handle(context.Background(), control(testOperatorID, "pause "+name)); err != nil {
				t.Fatalf("handle %s: %v", name, err)
			}
		}
		if calls := env.units.made(); len(calls) != 0 {
			t.Fatalf("pause acted on a non-manager: %v", calls)
		}
		if posts := env.posts.all(); len(posts) != 2 {
			t.Fatalf("pause answered %v", posts)
		}
	})
}
