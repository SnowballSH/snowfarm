package guard

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/discord"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

const controlUsage = "controls: status | pause <manager> | resume <manager> | stop <task> | runs"

// mentionToken is the bot mention Discord puts in front of a command typed at
// the supervisor. The command follows it.
var mentionToken = regexp.MustCompile(`<@[!&]?\d+>`)

// Controls is the operator's console in #farm-control. It acts only on
// messages the operator wrote in that channel: everything else on the guild,
// including a message from another agent quoting a command, is data.
type Controls struct {
	Roster   *atomic.Pointer[roster.Roster]
	Board    *board.Reader
	Loop     *dispatch.Loop
	Ledger   *dispatch.Ledger
	Breaker  *Breaker
	Post     func(ctx context.Context, text string)
	Now      func() time.Time
	Log      *slog.Logger
	Channel  string
	Operator string
}

// Run consumes the events the guard fans out to it until ctx ends. A command
// that fails is reported and the console stays open.
func (c *Controls) Run(ctx context.Context, events <-chan discord.Event) error {
	for {
		select {
		case <-ctx.Done():
			return nil
		case event, open := <-events:
			if !open {
				return nil
			}
			if err := c.Handle(ctx, event); err != nil {
				c.log().Error("control command failed", "content", event.Content, "error", err)
				c.post(ctx, fmt.Sprintf("that command failed: %v", err))
			}
		}
	}
}

func (c *Controls) Handle(ctx context.Context, event discord.Event) error {
	if event.Kind != discord.EventMessageCreate || event.ChannelID != c.Channel || event.AuthorID != c.Operator {
		return nil
	}
	fields := strings.Fields(mentionToken.ReplaceAllString(event.Content, " "))
	if len(fields) == 0 {
		return nil
	}
	switch verb, args := fields[0], fields[1:]; {
	case verb == "status" && len(args) == 0:
		return c.status(ctx)
	case verb == "runs" && len(args) == 0:
		return c.runs(ctx)
	case verb == "pause" && len(args) == 1:
		return c.pause(ctx, args[0])
	case verb == "resume" && len(args) == 1:
		return c.resume(ctx, args[0])
	case verb == "stop" && len(args) == 1:
		return c.stop(ctx, args[0])
	default:
		c.post(ctx, controlUsage)
		return nil
	}
}

func (c *Controls) status(ctx context.Context) error {
	counts, err := c.Board.Counts(ctx)
	if err != nil {
		return err
	}
	running, err := c.Board.Running(ctx)
	if err != nil {
		return err
	}
	pauses, err := c.Ledger.Pauses()
	if err != nil {
		return err
	}
	c.post(ctx, fmt.Sprintf("cards: %s; runs in flight: %d; paused: %s",
		cardCounts(counts), len(running), pausedManagers(pauses)))
	return nil
}

func cardCounts(counts map[string]int) string {
	if len(counts) == 0 {
		return "none"
	}
	parts := make([]string, 0, len(counts))
	for _, status := range slices.Sorted(maps.Keys(counts)) {
		parts = append(parts, fmt.Sprintf("%s %d", status, counts[status]))
	}
	return strings.Join(parts, ", ")
}

func pausedManagers(pauses []dispatch.Pause) string {
	if len(pauses) == 0 {
		return "nobody"
	}
	parts := make([]string, 0, len(pauses))
	for _, pause := range pauses {
		if pause.Until.IsZero() {
			parts = append(parts, fmt.Sprintf("%s (%s, until resumed)", pause.Agent, pause.Reason))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s (%s, until %s)", pause.Agent, pause.Reason, pause.Until.Format(time.TimeOnly)))
	}
	return strings.Join(parts, ", ")
}

func (c *Controls) runs(ctx context.Context) error {
	running := c.Loop.Running()
	if len(running) == 0 {
		c.post(ctx, "no run is in flight")
		return nil
	}
	now := c.now()
	lines := make([]string, 0, len(running))
	for _, card := range running {
		unit, ok := c.Ledger.UnitForTask(card.ID)
		if !ok {
			unit = "unit unknown"
		}
		lines = append(lines, fmt.Sprintf("%s (%s) in %s, running %s",
			card.ID, card.Assignee, unit, now.Sub(card.StartedAt).Round(time.Minute)))
	}
	c.post(ctx, "runs in flight: "+strings.Join(lines, "; "))
	return nil
}

func (c *Controls) pause(ctx context.Context, agent string) error {
	manages, err := c.manages(ctx, agent)
	if err != nil || !manages {
		return err
	}
	if err := c.Breaker.Pause(ctx, agent, pauseOperator, time.Time{}); err != nil {
		return err
	}
	c.post(ctx, fmt.Sprintf("%s: paused by the operator; it stays down until `resume %s`", agent, agent))
	return nil
}

func (c *Controls) resume(ctx context.Context, agent string) error {
	manages, err := c.manages(ctx, agent)
	if err != nil || !manages {
		return err
	}
	if err := c.Breaker.Resume(ctx, agent); err != nil {
		return err
	}
	c.post(ctx, fmt.Sprintf("%s: resumed by the operator; any pause the breaker had set is cleared", agent))
	return nil
}

// manages reports whether the name is a manager gateway the guard runs, and
// answers the operator itself when it is not: a name the guard cannot act on
// is a typo to show, not an error to report.
func (c *Controls) manages(ctx context.Context, name string) (bool, error) {
	r := c.Roster.Load()
	if r == nil {
		return false, errors.New("controls: the guard holds no roster")
	}
	agent, known := r.Agent(name)
	switch {
	case !known:
		c.post(ctx, fmt.Sprintf("%q is not an agent in the roster", name))
	case agent.Tier != roster.TierManager:
		c.post(ctx, fmt.Sprintf("%s is a worker: only a manager has a gateway to pause or resume", name))
	case !agent.Enabled:
		c.post(ctx, fmt.Sprintf("%s is not enabled, so the guard does not run its gateway", name))
	default:
		return true, nil
	}
	return false, nil
}

func (c *Controls) stop(ctx context.Context, taskID string) error {
	stopped, err := c.Loop.Stop(ctx, taskID)
	if err != nil {
		return err
	}
	if !stopped {
		c.post(ctx, fmt.Sprintf("the board reports no run for card %s", taskID))
	}
	return nil
}

func (c *Controls) post(ctx context.Context, text string) {
	if c.Post != nil {
		c.Post(ctx, text)
	}
}

func (c *Controls) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now().UTC()
}

func (c *Controls) log() *slog.Logger {
	if c.Log != nil {
		return c.Log
	}
	return slog.Default()
}
