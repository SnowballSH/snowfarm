// Package dispatch runs the farm's dispatch tick: it gives each worker a pass
// of Hermes' own dispatcher under that worker's Unix account, keeps its own
// record of what those passes did, and stops a run only where Hermes cannot.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/journal"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	// stopGrace, with two dispatch intervals, is how long past a card's own
	// deadline the guard waits for Hermes to enforce it before doing so
	// itself.
	stopGrace = 60 * time.Second

	// passResolveAge keeps a pass the tick has only just issued out of the
	// sweep that declares passes unresolved.
	passResolveAge = 5 * time.Second

	defaultPassWait = 10 * time.Second
	defaultPassPoll = 250 * time.Millisecond

	// rateLimitExit is the status a worker exits with when the upstream
	// model gateway refused it. It is a requeue, not a failure.
	rateLimitExit = 75

	tickSpawn        = "spawn"
	tickHousekeeping = "housekeeping"

	skipFarmCap       = "farm_cap"
	skipNoJSON        = "no_json"
	skipOtherAssignee = "other_assignee"

	reasonMaxRuntime   = "max_runtime"
	reasonNoMaxRuntime = "no_max_runtime"
	ReasonOperator     = "operator"

	outcomeRateLimited = "rate_limited"

	noticeNoMaxRuntime = "no_max_runtime"
	noticeStopFailed   = "stop_failed"
)

// Loop is one dispatch tick. The guard runs it serially — tick, sleep,
// tick — so a tick that serialises several passes delays the next rather
// than running beside it.
type Loop struct {
	Roster  *atomic.Pointer[roster.Roster]
	UIDs    *atomic.Pointer[map[string]int]
	Board   *board.Reader
	Units   unitctl.Controller
	Journal journal.Reader
	Ledger  *Ledger
	Post    func(ctx context.Context, text string)
	PostTo  func(ctx context.Context, channelID, text string)
	Metrics *metrics.Registry
	Now     func() time.Time
	Paused  atomic.Bool

	PassWait time.Duration
	PassPoll time.Duration

	mu       sync.Mutex
	running  []board.RunningCard
	previous map[string]board.RunningCard
}

func (l *Loop) Tick(ctx context.Context) error {
	r := l.Roster.Load()
	if r == nil {
		return errors.New("dispatch tick: the guard holds no roster")
	}
	now := l.now()
	running, err := l.Board.Running(ctx)
	if err != nil {
		return fmt.Errorf("dispatch tick: %w", err)
	}
	l.setRunning(running)
	l.Metrics.RunsInFlight.Set(float64(len(running)))

	current := index(running)
	spawns, reserved, sweepErr := l.sweepPasses(ctx, r, current, now)
	occupied := len(running) + spawns + reserved

	return errors.Join(
		sweepErr,
		l.issuePasses(ctx, r, running, now, &occupied),
		l.backstop(ctx, r, running, now),
		l.classifyFinished(ctx, r, current),
	)
}

// Stop ends the run behind one card at the operator's request, through the
// same path the backstop uses: the unit is stopped, the card reclaimed and
// blocked, and the outcome posted to the card's own thread. It reads the
// board rather than the last tick's snapshot, so an operator acting on a card
// that started since the last tick still reaches it. A card the board does
// not report running is reported as such rather than treated as stopped.
func (l *Loop) Stop(ctx context.Context, taskID string) (bool, error) {
	running, err := l.Board.Running(ctx)
	if err != nil {
		return false, err
	}
	for _, card := range running {
		if card.ID == taskID {
			return true, l.stopRun(ctx, card, ReasonOperator, 0, l.now())
		}
	}
	return false, nil
}

// Running is the in-flight set the last tick read, for the operator's `runs`
// control.
func (l *Loop) Running() []board.RunningCard {
	l.mu.Lock()
	defer l.mu.Unlock()
	return slices.Clone(l.running)
}

func (l *Loop) setRunning(running []board.RunningCard) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.running = running
}

func (l *Loop) now() time.Time {
	if l.Now != nil {
		return l.Now()
	}
	return time.Now().UTC()
}

// sweepPasses reads what the passes of earlier ticks did. It returns the
// spawns it learned about that the tick's own reading of the board is too old
// to hold, and the number of spawn passes whose outcome is still unknown, each
// of which keeps reserving a slot.
func (l *Loop) sweepPasses(ctx context.Context, r *roster.Roster, running map[string]board.RunningCard, now time.Time) (spawns, reserved int, err error) {
	var errs []error
	for _, agent := range r.EnabledWorkers() {
		passes, err := l.Ledger.PendingPasses(agent.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, pass := range passes {
			if now.Sub(pass.At) < passResolveAge {
				reserved += reserves(pass)
				continue
			}
			started, resolved, err := l.resolvePass(ctx, r, agent, pass, running, now)
			if err != nil {
				errs = append(errs, err)
			}
			spawns += started
			if !resolved {
				reserved += reserves(pass)
			}
		}
	}
	return spawns, reserved, errors.Join(errs...)
}

func reserves(pass Pass) int {
	if pass.Kind == PassSpawn {
		return 1
	}
	return 0
}

// resolvePass reads one pass's journal. A pass that printed its result is
// done however empty that result is — a pass that lost the board's dispatch
// lock prints a valid empty result and is indistinguishable from an idle one,
// and both mean the worker can be reconsidered. A pass with no result is
// resolved only once its unit has exited: while the unit lives the pass may
// still claim a card, so its reservation is retained.
//
// The nonspawnable bucket is counted and not posted about: five workers sweep
// one board, so four of them see every card as nonspawnable by construction,
// and the field that would separate a foreign assignee from that expected
// traffic is unverified at the pin (F2 pins it; the hygiene sweep carries the
// signal until then).
func (l *Loop) resolvePass(ctx context.Context, r *roster.Roster, agent roster.Agent, pass Pass, running map[string]board.RunningCard, now time.Time) (spawns int, resolved bool, err error) {
	uid, err := l.uid(r, agent)
	if err != nil {
		return 0, false, err
	}
	entries, err := l.Journal.UserUnit(ctx, uid, pass.Unit, pass.At)
	if err != nil {
		return 0, false, fmt.Errorf("read pass %s: %w", pass.Unit, err)
	}
	result, ok, err := ParseResult(entries)
	if err != nil {
		return 0, false, err
	}
	if !ok {
		if l.unitLives(ctx, agent.Name, pass.Unit) {
			return 0, false, nil
		}
		l.Metrics.DispatchSkippedTotal.WithLabelValues(skipNoJSON).Inc()
		return 0, true, l.Ledger.ResolvePass(pass.Unit, now)
	}
	var errs []error
	for _, spawned := range result.Spawned {
		if err := l.Ledger.RecordSpawn(agent.Name, pass.Unit, spawned.TaskID, now); err != nil {
			errs = append(errs, err)
			continue
		}
		l.Metrics.RunsStartedTotal.WithLabelValues(agent.Name).Inc()
		if _, counted := running[spawned.TaskID]; !counted {
			spawns++
		}
	}
	for range result.SkippedNonspawnable {
		l.Metrics.DispatchSkippedTotal.WithLabelValues(skipOtherAssignee).Inc()
	}
	errs = append(errs, l.Ledger.ResolvePass(pass.Unit, now))
	return spawns, true, errors.Join(errs...)
}

// unitLives reports whether the transient unit is still on the agent's user
// manager. A run-list that cannot be read is read as "still there", so an
// unreadable manager never releases a slot the farm may still be using.
func (l *Loop) unitLives(ctx context.Context, agent, unit string) bool {
	units, err := l.Units.RunList(ctx, agent)
	if err != nil {
		return true
	}
	return slices.Contains(units, unit)
}

type candidate struct {
	agent roster.Agent
	last  time.Time
	seen  bool
}

// issuePasses gives out this tick's passes, one at a time. A worker holding a
// running card gets a maintenance pass, which spawns nothing and reserves
// nothing; an idle worker gets a spawn pass, and only if the farm has a free
// slot at that moment — the count is re-tested before every spawn pass, never
// taken once per tick.
func (l *Loop) issuePasses(ctx context.Context, r *roster.Roster, running []board.RunningCard, now time.Time, occupied *int) error {
	busy := busyWorkers(running)
	interval := time.Duration(r.Guard.DispatchInterval)
	candidates, err := l.candidates(r)
	errs := []error{err}
	for _, c := range candidates {
		if c.seen && now.Sub(c.last) < interval {
			continue
		}
		kind := PassSpawn
		switch {
		case busy[c.agent.Name]:
			if !housekeeps(r) {
				continue
			}
			kind = PassMaintenance
		case l.Paused.Load():
			continue
		case *occupied >= r.Guard.MaxConcurrentRuns:
			l.Metrics.DispatchSkippedTotal.WithLabelValues(skipFarmCap).Inc()
			continue
		}
		if err := l.issuePass(ctx, r, c.agent, kind, now, occupied); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

func (l *Loop) issuePass(ctx context.Context, r *roster.Roster, agent roster.Agent, kind PassKind, now time.Time, occupied *int) error {
	uid, err := l.uid(r, agent)
	if err != nil {
		return err
	}
	unit, err := l.runPass(ctx, agent.Name, kind)
	if err != nil {
		return err
	}
	if kind == PassSpawn {
		*occupied++
	}
	l.Metrics.DispatchTicksTotal.WithLabelValues(agent.Name, metricKind(kind)).Inc()
	return errors.Join(
		l.Ledger.RecordPass(agent.Name, unit, kind, now),
		l.awaitPass(ctx, uid, unit, now),
	)
}

func (l *Loop) runPass(ctx context.Context, agent string, kind PassKind) (string, error) {
	if kind == PassMaintenance {
		return l.Units.RunMaintenance(ctx, agent)
	}
	return l.Units.RunDispatch(ctx, agent)
}

// awaitPass holds the tick until the pass has printed its result or the bound
// expires. Passes must not overlap: they contend for one board-wide dispatch
// lock, and systemd-run --wait cannot serialise them because ExitType=cgroup
// keeps a pass unit alive for as long as the worker it spawned.
func (l *Loop) awaitPass(ctx context.Context, uid int, unit string, since time.Time) error {
	deadline := time.Now().Add(l.passWait())
	for {
		entries, err := l.Journal.UserUnit(ctx, uid, unit, since)
		if err != nil {
			return fmt.Errorf("await pass %s: %w", unit, err)
		}
		if _, ok, err := ParseResult(entries); err != nil || ok {
			return err
		}
		if time.Now().After(deadline) {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(l.passPoll()):
		}
	}
}

// candidates orders the enabled workers by how long each has waited for a
// pass, longest first. Taking them in roster order instead would give the
// first worker every freed slot and starve the rest whenever the farm's cap
// is smaller than its roster.
func (l *Loop) candidates(r *roster.Roster) ([]candidate, error) {
	workers := r.EnabledWorkers()
	candidates := make([]candidate, 0, len(workers))
	var errs []error
	for _, agent := range workers {
		last, seen, err := l.Ledger.LastPass(agent.Name)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		candidates = append(candidates, candidate{agent: agent, last: last.At, seen: seen})
	}
	slices.SortStableFunc(candidates, func(a, b candidate) int {
		switch {
		case a.seen != b.seen:
			if a.seen {
				return 1
			}
			return -1
		case !a.seen:
			return 0
		default:
			return a.last.Compare(b.last)
		}
	})
	return candidates, errors.Join(errs...)
}

// backstop is the guard's own termination path, which fires only where Hermes'
// enforce_max_runtime cannot: past a card's own bound plus the guard's
// detection grace, and past a default bound for a card that was created
// without one, which Hermes never bounds at all.
func (l *Loop) backstop(ctx context.Context, r *roster.Roster, running []board.RunningCard, now time.Time) error {
	interval := time.Duration(r.Guard.DispatchInterval)
	fallback := time.Duration(r.Guard.DefaultMaxRuntime)
	unbounded := map[string]int{}
	var errs []error
	for _, card := range running {
		if !isEnabledWorker(r, card.Assignee) || card.StartedAt.IsZero() {
			continue
		}
		budget, reason := card.MaxRuntime, reasonMaxRuntime
		if budget <= 0 {
			budget, reason = fallback, reasonNoMaxRuntime
			unbounded[card.Assignee]++
			errs = append(errs, l.adviseUnbounded(ctx, card, fallback, now))
		}
		if now.Before(card.StartedAt.Add(budget + 2*interval + stopGrace)) {
			continue
		}
		errs = append(errs, l.stopRun(ctx, card, reason, budget, now))
	}
	for _, agent := range r.EnabledWorkers() {
		l.Metrics.CardsWithoutMaxRuntime.WithLabelValues(agent.Name).Set(float64(unbounded[agent.Name]))
	}
	return errors.Join(errs...)
}

// stopRun ends the run and lands the card where nothing will pick it up
// again: reclaim alone returns it to ready with a cleared failure counter, so
// the next spawn pass would re-claim it within one tick and the stop would
// undo itself.
//
// The stop is recorded only once the board has actually moved. A card the
// guard failed to reclaim stays running, holding its worker and a farm slot,
// so recording that failure as a stop would retire the only path that frees
// them; leaving the ledger untouched has the next tick try again.
func (l *Loop) stopRun(ctx context.Context, card board.RunningCard, reason string, budget time.Duration, now time.Time) error {
	unit := l.unitFor(card)
	stopped, err := l.Ledger.Stopped(card.ID, unit)
	if err != nil || stopped {
		return err
	}
	detail := stopDetail(reason, budget)
	var errs []error
	var failed []string
	if unit != "" && l.unitLives(ctx, card.Assignee, unit) {
		if err := l.Units.RunStop(ctx, card.Assignee, unit); err != nil {
			errs = append(errs, err)
			failed = append(failed, "stop the run unit")
		}
	}
	blocked := "the farm guard stopped this run: " + detail
	reclaimed := true
	if _, err := l.Units.Kanban(ctx, card.Assignee, "reclaim", card.ID); err != nil {
		errs = append(errs, err)
		failed = append(failed, "reclaim the card")
		reclaimed = false
	}
	if _, err := l.Units.Kanban(ctx, card.Assignee, "block", card.ID, blocked, "--kind", "transient"); err != nil {
		errs = append(errs, err)
		failed = append(failed, "block the card")
	}
	if len(errs) > 0 {
		errs = append(errs, l.adviseStopFailed(ctx, card, unit, detail, failed, reclaimed, now))
		return errors.Join(errs...)
	}
	if err := l.Ledger.RecordStop(card.ID, unit, reason, now); err != nil {
		return err
	}
	l.Metrics.RunStopsTotal.WithLabelValues(card.Assignee, reason).Inc()
	l.postToCard(ctx, card.ID, fmt.Sprintf(
		"card %s (%s): %s; the run was stopped, the card reclaimed and blocked (transient)",
		card.ID, card.Assignee, detail))
	return nil
}

// adviseStopFailed tells the card's thread once — not once per tick — which
// steps of the stop did not land, and whether a later tick will try again. A
// reclaim that succeeded has taken the card out of the board's running set,
// which is the only set the guard revisits, so promising a retry there would
// leave the operator waiting for one that never comes.
func (l *Loop) adviseStopFailed(ctx context.Context, card board.RunningCard, unit, detail string, failed []string, reclaimed bool, now time.Time) error {
	first, err := l.Ledger.NoticeOnce(noticeStopFailed, card.ID+"@"+unit, now)
	if err != nil || !first {
		return err
	}
	next := "it is still running, so the guard retries the whole stop on every tick"
	if reclaimed {
		next = "the card was reclaimed and has left the running set, so no tick revisits it: " +
			"block it by hand or it will be claimed again"
	}
	l.postToCard(ctx, card.ID, fmt.Sprintf(
		"card %s (%s): %s, but the guard could not %s; %s",
		card.ID, card.Assignee, detail, strings.Join(failed, " or "), next))
	return nil
}

func stopDetail(reason string, budget time.Duration) string {
	switch reason {
	case ReasonOperator:
		return "the operator stopped it"
	case reasonNoMaxRuntime:
		return fmt.Sprintf("it was created without a max_runtime and outran the guard's %s default", budget)
	default:
		return fmt.Sprintf("it outran its %s max_runtime and Hermes did not end it", budget)
	}
}

// unitFor names the unit to stop: the ledger knows it for every run this
// guard spawned, and the worker's own cgroup answers for a run it inherited.
func (l *Loop) unitFor(card board.RunningCard) string {
	if unit, ok := l.Ledger.UnitForTask(card.ID); ok {
		return unit
	}
	if card.WorkerPID > 0 && pidAlive(card.WorkerPID) {
		if unit, err := UnitForPID(card.WorkerPID); err == nil {
			return unit
		}
	}
	return ""
}

func (l *Loop) adviseUnbounded(ctx context.Context, card board.RunningCard, fallback time.Duration, now time.Time) error {
	first, err := l.Ledger.NoticeOnce(noticeNoMaxRuntime, card.ID, now)
	if err != nil || !first {
		return err
	}
	l.post(ctx, fmt.Sprintf(
		"card %s (%s) was created without a max_runtime; the guard is enforcing the %s default, "+
			"set --max-runtime for a task-specific bound", card.ID, card.Assignee, fallback))
	return nil
}

// classifyFinished names the outcome of every run that left the board since
// the last tick. The guard does this itself because Hermes cannot: it
// classifies a worker exit only when one process both spawned and reaped it,
// and a farm worker outlives the pass that spawned it and is reaped by
// systemd. Left to Hermes, every finished run would score as crashed and a
// rate-limited one would count towards the card's failure limit.
func (l *Loop) classifyFinished(ctx context.Context, r *roster.Roster, current map[string]board.RunningCard) error {
	previous := l.previous
	l.previous = current
	if previous == nil {
		return nil
	}
	var errs []error
	for _, id := range slices.Sorted(maps.Keys(previous)) {
		if _, still := current[id]; still {
			continue
		}
		outcome, ok, err := l.outcomeOf(ctx, r, previous[id])
		if err != nil {
			errs = append(errs, err)
		}
		if ok {
			l.Metrics.RunsFinishedTotal.WithLabelValues(previous[id].Assignee, outcome).Inc()
		}
	}
	return errors.Join(errs...)
}

func (l *Loop) outcomeOf(ctx context.Context, r *roster.Roster, card board.RunningCard) (string, bool, error) {
	limited, err := l.rateLimited(ctx, r, card)
	if err != nil {
		return "", false, err
	}
	if limited {
		_, err := l.Units.Kanban(ctx, card.Assignee, "reclaim", card.ID)
		return outcomeRateLimited, true, err
	}
	outcome, ok, err := l.Board.LastOutcome(ctx, card.ID)
	if err != nil {
		return "", false, err
	}
	return outcome, ok, nil
}

func (l *Loop) rateLimited(ctx context.Context, r *roster.Roster, card board.RunningCard) (bool, error) {
	unit, ok := l.Ledger.UnitForTask(card.ID)
	if !ok {
		return false, nil
	}
	agent, ok := r.Agent(card.Assignee)
	if !ok {
		return false, nil
	}
	uid, err := l.uid(r, agent)
	if err != nil {
		return false, err
	}
	entries, err := l.Journal.UserUnit(ctx, uid, unit, card.StartedAt)
	if err != nil {
		return false, fmt.Errorf("read run %s: %w", unit, err)
	}
	status, ok := ExitStatus(entries)
	return ok && status == rateLimitExit, nil
}

// postToCard sends an outcome to the thread the card's manager subscribed,
// because a reclaimed card raises no terminal event and Hermes' notifier will
// say nothing about it. A card nobody subscribed falls back to the status
// channel rather than losing the message.
func (l *Loop) postToCard(ctx context.Context, taskID, text string) {
	sub, ok, err := l.Board.NotifierSubForTask(ctx, taskID)
	if err == nil && ok && sub.Target() != "" && l.PostTo != nil {
		l.PostTo(ctx, sub.Target(), text)
		return
	}
	l.post(ctx, text)
}

func (l *Loop) post(ctx context.Context, text string) {
	if l.Post != nil {
		l.Post(ctx, text)
	}
}

func (l *Loop) uid(r *roster.Roster, agent roster.Agent) (int, error) {
	if uids := l.UIDs.Load(); uids != nil {
		if uid, ok := (*uids)[agent.Name]; ok {
			return uid, nil
		}
	}
	return 0, fmt.Errorf("no uid for agent %s", agent.Name)
}

func (l *Loop) passWait() time.Duration {
	if l.PassWait > 0 {
		return l.PassWait
	}
	return defaultPassWait
}

func (l *Loop) passPoll() time.Duration {
	if l.PassPoll > 0 {
		return l.PassPoll
	}
	return defaultPassPoll
}

func index(running []board.RunningCard) map[string]board.RunningCard {
	cards := make(map[string]board.RunningCard, len(running))
	for _, card := range running {
		cards[card.ID] = card
	}
	return cards
}

func busyWorkers(running []board.RunningCard) map[string]bool {
	busy := make(map[string]bool, len(running))
	for _, card := range running {
		busy[card.Assignee] = true
	}
	return busy
}

// housekeeps reads the roster's opt-out. A nil flag is default-on; only an
// explicit false suspends the maintenance pass, and never the guard's own
// backstop.
func housekeeps(r *roster.Roster) bool {
	return r.Guard.HousekeepingPasses == nil || *r.Guard.HousekeepingPasses
}

func isEnabledWorker(r *roster.Roster, name string) bool {
	return slices.ContainsFunc(r.EnabledWorkers(), func(a roster.Agent) bool { return a.Name == name })
}

func metricKind(kind PassKind) string {
	if kind == PassMaintenance {
		return tickHousekeeping
	}
	return tickSpawn
}
