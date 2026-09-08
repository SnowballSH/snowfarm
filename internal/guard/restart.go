package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	// restartSpacing keeps two managers from re-IDENTIFYing at once, so a
	// night's restarts cost two sessions minutes apart rather than a burst.
	restartSpacing = 2 * time.Minute

	// forceGrace is how long past the window's end the guard still catches
	// up on a manager it could not restart quietly. A guard that was down
	// for the whole window skips the night rather than restarting managers
	// in the middle of the operator's morning.
	forceGrace = time.Hour
)

// Restarter is the nightly drained restart: inside its window it restarts
// each enabled manager whose gateway is between turns, two minutes apart, and
// once the window closes it restarts whatever is left and says so.
type Restarter struct {
	Roster  *atomic.Pointer[roster.Roster]
	Units   unitctl.Controller
	Ledger  *dispatch.Ledger
	Turns   *Turns
	Post    func(ctx context.Context, text string)
	Metrics *metrics.Registry
	Now     func() time.Time
	Log     *slog.Logger

	mu        sync.Mutex
	restarted map[string]string
	last      time.Time
	disarmed  bool
}

func (rs *Restarter) Tick(ctx context.Context) error {
	r := rs.Roster.Load()
	if r == nil {
		return errors.New("drained restarter: the guard holds no roster")
	}
	if r.Guard.RestartWindow == "" {
		return nil
	}
	// Without a completion pattern a turn's end is unobservable, and the
	// only reading left — the age of its start line — is the one that
	// aborts running turns.
	if r.Guard.TurnCompletePattern == "" {
		rs.disarm()
		return nil
	}
	now := rs.now()
	start, end, err := restartWindow(r.Guard.RestartWindow, now)
	if err != nil {
		return err
	}
	if now.Before(start) || now.After(end.Add(forceGrace)) {
		return nil
	}
	return rs.pass(ctx, r, start.Format(time.DateOnly), !now.Before(end), now)
}

func (rs *Restarter) pass(ctx context.Context, r *roster.Roster, day string, force bool, now time.Time) error {
	for _, manager := range r.EnabledManagers() {
		if rs.done(manager.Name, day) {
			continue
		}
		_, paused, err := rs.Ledger.Paused(manager.Name)
		if err != nil {
			return err
		}
		if paused {
			rs.mark(manager.Name, day, time.Time{})
			continue
		}
		_, open, err := rs.Turns.open(manager.Name)
		if err != nil {
			return err
		}
		if open && !force {
			continue
		}
		if rs.tooSoon(now) {
			return nil
		}
		if _, err := rs.Units.Gateway(ctx, manager.Name, "restart"); err != nil {
			return err
		}
		rs.mark(manager.Name, day, now)
		if err := rs.Turns.reset(manager.Name); err != nil {
			return err
		}
		rs.Metrics.ManagerRestartsTotal.WithLabelValues(manager.Name).Inc()
		if force {
			rs.post(ctx, fmt.Sprintf(
				"%s: the %s restart window closed with a turn still in flight, so the gateway was restarted anyway",
				manager.Name, r.Guard.RestartWindow))
		}
		return nil
	}
	return nil
}

func (rs *Restarter) done(agent, day string) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return rs.restarted[agent] == day
}

func (rs *Restarter) mark(agent, day string, at time.Time) {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.restarted == nil {
		rs.restarted = map[string]string{}
	}
	rs.restarted[agent] = day
	if !at.IsZero() {
		rs.last = at
	}
}

func (rs *Restarter) tooSoon(now time.Time) bool {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	return !rs.last.IsZero() && now.Sub(rs.last) < restartSpacing
}

func (rs *Restarter) disarm() {
	rs.mu.Lock()
	defer rs.mu.Unlock()
	if rs.disarmed {
		return
	}
	rs.disarmed = true
	rs.log().Warn("the drained restarter is disarmed: guard.turn_complete_pattern is empty, so no turn can be proved finished")
}

func (rs *Restarter) post(ctx context.Context, text string) {
	if rs.Post != nil {
		rs.Post(ctx, text)
	}
}

func (rs *Restarter) now() time.Time {
	if rs.Now != nil {
		return rs.Now()
	}
	return time.Now().UTC()
}

func (rs *Restarter) log() *slog.Logger {
	if rs.Log != nil {
		return rs.Log
	}
	return slog.Default()
}

// restartWindow places "HH:MM-HH:MM" on the day now falls in, in UTC, which
// is the zone the design states the window in.
func restartWindow(spec string, now time.Time) (start, end time.Time, err error) {
	from, to, ok := strings.Cut(spec, "-")
	if !ok {
		return time.Time{}, time.Time{}, fmt.Errorf("guard.restart_window %q is not HH:MM-HH:MM", spec)
	}
	start, err = clockOn(now, strings.TrimSpace(from))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	end, err = clockOn(now, strings.TrimSpace(to))
	if err != nil {
		return time.Time{}, time.Time{}, err
	}
	if !end.After(start) {
		return time.Time{}, time.Time{}, fmt.Errorf("guard.restart_window %q does not end after it starts", spec)
	}
	return start, end, nil
}

func clockOn(day time.Time, hhmm string) (time.Time, error) {
	at, err := time.Parse("15:04", hhmm)
	if err != nil {
		return time.Time{}, fmt.Errorf("guard.restart_window: %w", err)
	}
	day = day.UTC()
	return time.Date(day.Year(), day.Month(), day.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC), nil
}
