// Package guard is the supervisor's long-running half: the manager breaker,
// the drained nightly restarts, the liveness probes, the operator's
// #farm-control commands, and the wiring that starts every component around
// one shared roster.
package guard

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowfarm/internal/discord"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/journal"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	pauseFor    = 30 * time.Minute
	turnWindow  = time.Hour
	burstWindow = 5 * time.Minute
	tripWindow  = 6 * time.Hour

	// sessionFloor is the IDENTIFY budget a manager must still hold before
	// the guard spends one on an adapter-trip restart.
	sessionFloor = 50

	gatewayLogFile = "gateway.log"

	pauseTurns    = "turns"
	pauseMentions = "operator_mentions"
	pauseBurst    = "http_burst"
	pauseOperator = "operator"
)

// burstErrors is Discord refusing the bot: the rate a host is banned for.
var burstErrors = regexp.MustCompile(`HTTP (401|403|429)`)

// adapterTrip is Hermes' own circuit breaker opening, which never closes by
// itself. The literal is a placeholder until the F2 drill pins it from a real
// gateway.log, as the two turn patterns are.
var adapterTrip = regexp.MustCompile(`circuit breaker opened`)

// Turns is what each manager is doing right now: a turn is open from the line
// that starts it until the line that completes it, and only that pairing
// makes a manager safe to restart. A manager turn may run for
// HERMES_AGENT_TIMEOUT, so the age of a start line proves nothing.
type Turns struct {
	mu       sync.Mutex
	inflight map[string][]time.Time
}

func (t *Turns) started(agent string, at time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.inflight == nil {
		t.inflight = map[string][]time.Time{}
	}
	t.inflight[agent] = append(t.inflight[agent], at)
}

func (t *Turns) completed(agent string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.inflight[agent]) > 0 {
		t.inflight[agent] = t.inflight[agent][1:]
	}
}

// open reports the start of the oldest turn that has not been completed.
func (t *Turns) open(agent string) (time.Time, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.inflight[agent]) == 0 {
		return time.Time{}, false
	}
	return t.inflight[agent][0], true
}

// reset forgets a manager's open turns, which is what a restart does to them.
func (t *Turns) reset(agent string) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.inflight, agent)
}

// Breaker reads each manager's gateway log and acts on what it finds: a
// manager over its turn or operator-mention allowance is paused, a burst of
// Discord refusals stops it outright, and an adapter trip restarts it inside
// a session and restart budget.
type Breaker struct {
	Roster  *atomic.Pointer[roster.Roster]
	Units   unitctl.Controller
	Journal journal.Reader
	Ledger  *dispatch.Ledger
	Client  discord.Client

	// Secrets serves an agent's decrypted variables. The adapter-trip
	// restart re-IDENTIFYs on the manager's own bot token, so the budget it
	// checks must be that bot's and not the supervisor's.
	Secrets func(agent string) (map[string]string, bool)

	Turns   *Turns
	Post    func(ctx context.Context, text string)
	Metrics *metrics.Registry
	Now     func() time.Time

	mu       sync.Mutex
	signals  map[string]*signals
	compiled patterns
}

// signals is one manager's recent history, each list pruned to its own
// window. Restarts outlive a pause, so a pause clears the other three only.
type signals struct{ turns, mentions, bursts, restarts []time.Time }

type patterns struct {
	turnLog, turnComplete, mention *regexp.Regexp
	turnSource, completeSource     string
	mentionSource                  string
}

func (b *Breaker) Tick(ctx context.Context) error {
	r := b.Roster.Load()
	if r == nil {
		return errors.New("breaker: the guard holds no roster")
	}
	p, err := b.patternsFor(r)
	if err != nil {
		return err
	}
	now := b.now()
	var errs []error
	for _, manager := range r.EnabledManagers() {
		errs = append(errs, b.sweep(ctx, r, manager, p, now))
	}
	return errors.Join(errs...)
}

func (b *Breaker) sweep(ctx context.Context, r *roster.Roster, manager roster.Agent, p patterns, now time.Time) error {
	pause, paused, err := b.Ledger.Paused(manager.Name)
	if err != nil {
		return err
	}
	if paused && !pause.Until.IsZero() && !now.Before(pause.Until) {
		if err := b.Resume(ctx, manager.Name); err != nil {
			return err
		}
		b.post(ctx, fmt.Sprintf("%s: its %s pause expired and its gateway was started again", manager.Name, pause.Reason))
		paused = false
	}
	trips, err := b.read(ctx, r, manager, p, now)
	if err != nil || paused {
		return err
	}
	return b.act(ctx, r, manager.Name, trips, now)
}

// read tails the manager's gateway log from where the last pass stopped. A
// log that does not exist is a manager that has never run, which is a state
// and not a fault.
func (b *Breaker) read(ctx context.Context, r *roster.Roster, manager roster.Agent, p patterns, now time.Time) (int, error) {
	key := offsetKey(manager.Name)
	offset, err := b.Ledger.Offset(key)
	if err != nil {
		return 0, err
	}
	path := gatewayLogPath(r, manager)
	lines, next, err := b.Journal.GatewayLog(ctx, path, offset)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return 0, nil
	case err != nil:
		return 0, fmt.Errorf("read %s: %w", path, err)
	}
	trips := b.classify(manager.Name, lines, p, now)
	if next == offset {
		return trips, nil
	}
	return trips, b.Ledger.SetOffset(key, next)
}

// classify reads each line once, stamping it with the time the guard read it
// rather than with the timestamp in the line: the pass runs every thirty
// seconds, so the two differ by less than a rate window's resolution, and the
// log's own timestamp format is not pinned until F2. The completion pattern
// is tested first because the default patterns overlap: a completion line
// also matches the start pattern, and reading it as a start would leave a
// turn open forever.
func (b *Breaker) classify(agent string, lines []string, p patterns, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.signalsLocked(agent)
	trips, turns := 0, 0
	for _, line := range lines {
		switch {
		case p.turnComplete != nil && p.turnComplete.MatchString(line):
			b.Turns.completed(agent)
		case p.turnLog != nil && p.turnLog.MatchString(line):
			b.Turns.started(agent, now)
			s.turns = append(s.turns, now)
			turns++
		}
		if p.mention != nil && p.mention.MatchString(line) {
			s.mentions = append(s.mentions, now)
		}
		if burstErrors.MatchString(line) {
			s.bursts = append(s.bursts, now)
		}
		if adapterTrip.MatchString(line) {
			trips++
		}
	}
	if turns > 0 {
		b.Metrics.ManagerTurnsTotal.WithLabelValues(agent).Add(float64(turns))
	}
	return trips
}

// act fires at most one action per manager per pass, in the order of the
// design's breaker table. Each count is exact at its bound: a manager pauses
// at its allowance, never below it.
func (b *Breaker) act(ctx context.Context, r *roster.Roster, agent string, trips int, now time.Time) error {
	turns, mentions, bursts := b.counts(agent, now)
	switch {
	case turns >= r.Guard.ManagerTurnsPerHour:
		return b.pauseSignal(ctx, agent, pauseTurns, now,
			fmt.Sprintf("it handled %d turns in an hour", turns))
	case mentions >= r.Guard.OperatorMentionsPerHour:
		return b.pauseSignal(ctx, agent, pauseMentions, now,
			fmt.Sprintf("it mentioned the operator %d times in an hour", mentions))
	case bursts >= r.Guard.BurstPer5m:
		return b.burstStop(ctx, agent, bursts)
	case trips > 0:
		return b.tripRestart(ctx, r, agent, now)
	}
	return nil
}

func (b *Breaker) pauseSignal(ctx context.Context, agent, reason string, now time.Time, detail string) error {
	if err := b.Pause(ctx, agent, reason, now.Add(pauseFor)); err != nil {
		return err
	}
	b.post(ctx, fmt.Sprintf("%s: %s, so its gateway is paused until %s; board work is unaffected",
		agent, detail, now.Add(pauseFor).Format(time.TimeOnly)))
	return nil
}

// burstStop has no expiry: Discord bans a host that keeps sending refused
// requests, so the manager stays down until someone has looked at it.
func (b *Breaker) burstStop(ctx context.Context, agent string, bursts int) error {
	if err := b.Pause(ctx, agent, pauseBurst, time.Time{}); err != nil {
		return err
	}
	b.Metrics.BurstStopsTotal.WithLabelValues(agent).Inc()
	b.post(ctx, fmt.Sprintf(
		"%s: %d Discord 401, 403 or 429 responses in %s, so its gateway was stopped and will not resume on its own; `resume %s` in #farm-control after checking it",
		agent, bursts, burstWindow, agent))
	return nil
}

// tripRestart spends one of the manager's own Discord sessions, and only when
// that bot still has budget for it and the guard has not already restarted it
// six times in six hours.
func (b *Breaker) tripRestart(ctx context.Context, r *roster.Roster, agent string, now time.Time) error {
	b.Metrics.AdapterTripRestartsTotal.WithLabelValues(agent).Inc()
	if used := b.restarts(agent, now); used >= r.Guard.RestartBudgetPer6h {
		b.post(ctx, fmt.Sprintf(
			"%s: its Discord adapter tripped again, but the guard has spent its restart budget of %d in %s; the gateway was left as it is",
			agent, r.Guard.RestartBudgetPer6h, tripWindow))
		return nil
	}
	limit, err := b.sessionBudget(ctx, agent)
	if err != nil {
		b.post(ctx, fmt.Sprintf("%s: its Discord adapter tripped, but its session budget could not be read, so the gateway was not restarted: %v", agent, err))
		return err
	}
	if limit.Remaining < sessionFloor {
		b.post(ctx, fmt.Sprintf(
			"%s: its Discord adapter tripped, but only %d of %d session starts remain, so the gateway was not restarted",
			agent, limit.Remaining, limit.Total))
		return nil
	}
	if _, err := b.Units.Gateway(ctx, agent, "restart"); err != nil {
		return err
	}
	b.recordRestart(agent, now)
	b.Turns.reset(agent)
	b.Metrics.ManagerRestartsTotal.WithLabelValues(agent).Inc()
	b.post(ctx, fmt.Sprintf("%s: its Discord adapter tripped its circuit, which never closes on its own, so the gateway was restarted", agent))
	return nil
}

func (b *Breaker) sessionBudget(ctx context.Context, agent string) (discord.SessionStartLimit, error) {
	token := ""
	if b.Secrets != nil {
		if vars, ok := b.Secrets(agent); ok {
			token = vars["DISCORD_BOT_TOKEN"]
		}
	}
	if token == "" {
		return discord.SessionStartLimit{}, fmt.Errorf("no Discord bot token is loaded for %s", agent)
	}
	return b.Client.GatewayBot(ctx, token)
}

// Pause stops a manager's gateway and records why, so the guard's own metric
// and the operator's `status` both say a manager that is down is meant to be.
func (b *Breaker) Pause(ctx context.Context, agent, reason string, until time.Time) error {
	if _, err := b.Units.Gateway(ctx, agent, "stop"); err != nil {
		return err
	}
	if err := b.Ledger.RecordPause(agent, reason, b.now(), until); err != nil {
		return err
	}
	b.clearSignals(agent)
	b.Turns.reset(agent)
	b.Metrics.ManagerPausesTotal.WithLabelValues(agent, reason).Inc()
	return nil
}

// Resume clears the pause row before it starts the unit, so a guard that dies
// between the two leaves a manager the next pass will start rather than one
// nothing will.
func (b *Breaker) Resume(ctx context.Context, agent string) error {
	if err := b.Ledger.ClearPause(agent); err != nil {
		return err
	}
	b.clearSignals(agent)
	_, err := b.Units.Gateway(ctx, agent, "start")
	return err
}

func (b *Breaker) counts(agent string, now time.Time) (turns, mentions, bursts int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.signalsLocked(agent)
	s.turns = within(s.turns, now, turnWindow)
	s.mentions = within(s.mentions, now, turnWindow)
	s.bursts = within(s.bursts, now, burstWindow)
	return len(s.turns), len(s.mentions), len(s.bursts)
}

func (b *Breaker) restarts(agent string, now time.Time) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.signalsLocked(agent)
	s.restarts = within(s.restarts, now, tripWindow)
	return len(s.restarts)
}

func (b *Breaker) recordRestart(agent string, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.signalsLocked(agent)
	s.restarts = append(s.restarts, now)
}

// clearSignals forgets what a pause was imposed for. Without it a manager
// resumed after thirty minutes still carries the hour of turns that paused
// it and pauses again on its first line.
func (b *Breaker) clearSignals(agent string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s := b.signalsLocked(agent)
	s.turns, s.mentions, s.bursts = nil, nil, nil
}

func (b *Breaker) signalsLocked(agent string) *signals {
	if b.signals == nil {
		b.signals = map[string]*signals{}
	}
	s, ok := b.signals[agent]
	if !ok {
		s = &signals{}
		b.signals[agent] = s
	}
	return s
}

// patternsFor compiles the roster's turn patterns once and again whenever a
// reload changes them.
func (b *Breaker) patternsFor(r *roster.Roster) (patterns, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	mention := mentionPattern(r.Farm.Discord.OperatorUserID)
	if b.compiled.turnSource == r.Guard.TurnLogPattern &&
		b.compiled.completeSource == r.Guard.TurnCompletePattern &&
		b.compiled.mentionSource == mention {
		return b.compiled, nil
	}
	p := patterns{
		turnSource:     r.Guard.TurnLogPattern,
		completeSource: r.Guard.TurnCompletePattern,
		mentionSource:  mention,
	}
	var errs []error
	var err error
	if p.turnSource != "" {
		if p.turnLog, err = regexp.Compile(p.turnSource); err != nil {
			errs = append(errs, fmt.Errorf("guard.turn_log_pattern: %w", err))
		}
	}
	if p.completeSource != "" {
		if p.turnComplete, err = regexp.Compile(p.completeSource); err != nil {
			errs = append(errs, fmt.Errorf("guard.turn_complete_pattern: %w", err))
		}
	}
	if mention != "" {
		if p.mention, err = regexp.Compile(mention); err != nil {
			errs = append(errs, fmt.Errorf("operator mention pattern: %w", err))
		}
	}
	if err := errors.Join(errs...); err != nil {
		return patterns{}, err
	}
	b.compiled = p
	return p, nil
}

// mentionPattern matches the manager writing the operator's mention, which is
// the escalation the design bounds. An inbound turn from the operator is a
// turn, and is counted as one.
func mentionPattern(operatorID string) string {
	if operatorID == "" {
		return ""
	}
	return `<@!?` + regexp.QuoteMeta(operatorID) + `>`
}

func (b *Breaker) post(ctx context.Context, text string) {
	if b.Post != nil {
		b.Post(ctx, text)
	}
}

func (b *Breaker) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now().UTC()
}

func within(stamps []time.Time, now time.Time, window time.Duration) []time.Time {
	cut := now.Add(-window)
	kept := stamps[:0]
	for _, at := range stamps {
		if at.After(cut) {
			kept = append(kept, at)
		}
	}
	return kept
}

func offsetKey(agent string) string { return "gateway:" + agent }

func gatewayLogPath(r *roster.Roster, agent roster.Agent) string {
	return filepath.Join(agent.HermesHome(r.Farm), "logs", gatewayLogFile)
}
