// Package schedule turns the roster's schedules into Kanban cards. The
// runner creates each card as the schedule's notifier — the manager whose
// user may subscribe its own profile to the card it just made — and keys
// every creation to the day, so a farm that fires the same schedule twice
// gets one card rather than two.
package schedule

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	platformDiscord = "discord"
	chatTypeChannel = "channel"
	deliveryMode    = "notify+wake"
)

var createdLine = regexp.MustCompile(`Created (t_[0-9a-f]+)`)

// Runner owns the farm's cron scheduler. Roster is the shared pointer the
// guard stores into, so a reload changes what the runner registers without
// the runner holding a roster of its own.
type Runner struct {
	cron *cron.Cron

	Roster   *atomic.Pointer[roster.Roster]
	Units    unitctl.Controller
	Channels map[string]string
	Post     func(ctx context.Context, text string)
	Now      func() time.Time
	Log      *slog.Logger

	mu         sync.Mutex
	registered []string
}

func (s *Runner) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rebuild(ctx)
}

// Reload rebuilds the scheduler against the roster the guard has already
// stored. robfig/cron has no removal that survives a rebuild, so the old
// scheduler is stopped and a new one takes its entries: that is how a newly
// enabled agent gains its crons and a newly disabled one loses them.
func (s *Runner) Reload(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.halt()
	return s.rebuild(ctx)
}

// Scheduled names the schedules the current scheduler holds, in roster order.
// It is how a caller — and the reload test — sees that a phase flip actually
// registered a newly enabled agent's crons.
func (s *Runner) Scheduled() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.registered)
}

func (s *Runner) Stop() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.halt()
}

func (s *Runner) halt() {
	if s.cron == nil {
		return
	}
	s.cron.Stop()
	s.cron = nil
	s.registered = nil
}

func (s *Runner) rebuild(ctx context.Context) error {
	r := s.Roster.Load()
	if r == nil {
		return errors.New("schedule runner: the guard holds no roster")
	}
	loc, err := location(r.Farm.Location)
	if err != nil {
		return fmt.Errorf("schedule runner: farm location %q: %w", r.Farm.Location, err)
	}
	scheduler := cron.New(cron.WithLocation(loc))
	s.registered = nil
	var errs []error
	for _, sc := range r.Schedules {
		if !r.IsEnabled(sc.Assignee) || !r.IsEnabled(sc.Notifier) {
			s.log().Info("schedule skipped: an agent it names is not enabled",
				"schedule", sc.Name, "assignee", sc.Assignee, "notifier", sc.Notifier)
			continue
		}
		parsed, _, err := parseSchedule(sc)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		scheduler.Schedule(parsed, cron.FuncJob(func() { s.fire(ctx, sc) }))
		s.registered = append(s.registered, sc.Name)
	}
	s.cron = scheduler
	scheduler.Start()
	return errors.Join(errs...)
}

func (s *Runner) fire(ctx context.Context, sc roster.Schedule) {
	if err := s.Fire(ctx, sc); err != nil {
		s.log().Error("schedule failed", "schedule", sc.Name, "error", err)
		s.post(ctx, fmt.Sprintf("schedule %q did not create its card: %v", sc.Name, err))
	}
}

func (s *Runner) Fire(ctx context.Context, sc roster.Schedule) error {
	r := s.Roster.Load()
	if r == nil {
		return errors.New("schedule runner: the guard holds no roster")
	}
	if !r.IsEnabled(sc.Assignee) || !r.IsEnabled(sc.Notifier) {
		return fmt.Errorf("schedule %q names an agent that is not enabled: assignee %s, notifier %s",
			sc.Name, sc.Assignee, sc.Notifier)
	}
	id, err := s.create(ctx, sc)
	if err != nil {
		return err
	}
	channelID, ok := s.Channels[sc.Channel]
	if !ok {
		return fmt.Errorf("schedule %q: card %s was created, but no channel id is known for %s, so nothing was subscribed",
			sc.Name, id, sc.Channel)
	}
	_, err = s.Units.Kanban(ctx, sc.Notifier, "notify-subscribe", id,
		"--platform", platformDiscord,
		"--chat-id", channelID,
		"--chat-type", chatTypeChannel,
		"--notifier-profile", sc.Notifier,
		"--delivery-mode", deliveryMode)
	if err != nil {
		return fmt.Errorf("schedule %q: subscribe %s: %w", sc.Name, id, err)
	}
	return nil
}

func (s *Runner) create(ctx context.Context, sc roster.Schedule) (string, error) {
	args := []string{"create", sc.Title,
		"--body", sc.Body,
		"--assignee", sc.Assignee,
		"--priority", strconv.Itoa(sc.Priority),
	}
	if sc.MaxRuntime != "" {
		args = append(args, "--max-runtime", sc.MaxRuntime)
	}
	args = append(args, "--idempotency-key", s.key(sc), "--json")
	out, err := s.Units.Kanban(ctx, sc.Notifier, args...)
	if err != nil {
		return "", fmt.Errorf("schedule %q: create: %w", sc.Name, err)
	}
	id, ok := taskID(out)
	if !ok {
		return "", fmt.Errorf("schedule %q: create named no task: %q", sc.Name, strings.TrimSpace(string(out)))
	}
	return id, nil
}

// key is the schedule's own name and the day it fired, in the schedule's
// location: a card created at 08:00 New York belongs to that New York day,
// which is what makes a second fire of the same morning idempotent.
func (s *Runner) key(sc roster.Schedule) string {
	loc, err := location(sc.Location)
	if err != nil {
		loc = time.UTC
	}
	return sc.Name + "-" + s.now().In(loc).Format(time.DateOnly)
}

func (s *Runner) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now().UTC()
}

func (s *Runner) post(ctx context.Context, text string) {
	if s.Post != nil {
		s.Post(ctx, text)
	}
}

func (s *Runner) log() *slog.Logger {
	if s.Log != nil {
		return s.Log
	}
	return slog.Default()
}

// parseSchedule reads the entry's own location into the schedule itself,
// rather than into the scheduler, so schedules in different zones can share
// one cron.
func parseSchedule(sc roster.Schedule) (cron.Schedule, *time.Location, error) {
	loc, err := location(sc.Location)
	if err != nil {
		return nil, nil, fmt.Errorf("schedule %q: location %q: %w", sc.Name, sc.Location, err)
	}
	spec := sc.Cron
	if sc.Location != "" {
		spec = "CRON_TZ=" + sc.Location + " " + sc.Cron
	}
	parsed, err := cron.ParseStandard(spec)
	if err != nil {
		return nil, nil, fmt.Errorf("schedule %q: cron %q: %w", sc.Name, sc.Cron, err)
	}
	return parsed, loc, nil
}

func location(name string) (*time.Location, error) {
	if name == "" {
		return time.UTC, nil
	}
	return time.LoadLocation(name)
}

// taskID reads the created card out of whatever the CLI printed: the --json
// object under either key the pin may use, or the line it prints when the
// JSON shape is not the one documented.
func taskID(out []byte) (string, bool) {
	if id, ok := decodeID(out); ok {
		return id, true
	}
	for line := range strings.Lines(string(out)) {
		if id, ok := decodeID([]byte(line)); ok {
			return id, true
		}
	}
	if match := createdLine.FindSubmatch(out); match != nil {
		return string(match[1]), true
	}
	return "", false
}

func decodeID(data []byte) (string, bool) {
	var payload struct {
		ID     string `json:"id"`
		TaskID string `json:"task_id"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		return "", false
	}
	if payload.ID != "" {
		return payload.ID, true
	}
	return payload.TaskID, payload.TaskID != ""
}
