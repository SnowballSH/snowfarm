// Package hygiene is the guard's five-minute pass over the board and the
// agents' rendered profiles. It detects and reverses what the boundary cannot
// prevent — a card aimed at an agent that does not exist, a worker creating
// work for another agent, a worker forging a manager's notifier, a profile
// that no longer matches what apply rendered — and reports the rest.
package hygiene

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	strandedAfter = 30 * time.Minute
	blockKind     = "transient"

	configFile = "config.yaml"
	soulFile   = "SOUL.md"

	statusRunning = "running"

	baselineMode = 0o640
)

var runUnitName = regexp.MustCompile(`^snowfarm-run-([a-z][a-z0-9-]*)-\d+\.service$`)

// Sweep holds no state of its own: Roster is the pointer the guard stores
// into, so every pass reads the enabled set a reload may just have changed.
// Capacity reports the dispatch loop's live occupancy, which is what
// separates a card the farm ignored from one merely queued behind the cap.
type Sweep struct {
	Roster     *atomic.Pointer[roster.Roster]
	Board      *board.Reader
	Units      unitctl.Controller
	Post       func(ctx context.Context, text string)
	Metrics    *metrics.Registry
	HomeRoot   string
	Baseline   string
	Capacity   func() (occupied, capacity int, paused bool)
	UnitForPID func(pid int) (string, error)
}

// ProfileDigests is the baseline apply records and the sweep compares
// against: the SHA-256 of each enabled agent's two rendered files.
type ProfileDigests struct {
	Profiles map[string]ProfileDigest `json:"profiles"`
}

type ProfileDigest struct {
	Config string `json:"config"`
	Soul   string `json:"soul"`
}

func (h *Sweep) Run(ctx context.Context) error {
	r := h.Roster.Load()
	if r == nil {
		return errors.New("hygiene sweep: the guard holds no roster")
	}
	workers := agentNames(r.EnabledWorkers())
	managers := agentNames(r.EnabledManagers())
	blocked := map[string]bool{}
	return errors.Join(
		h.exportCounts(ctx),
		h.sweepStrangers(ctx, r, workers, blocked),
		h.sweepCrossCreated(ctx, r, workers, blocked),
		h.sweepStranded(ctx, workers),
		h.sweepNotifierSubs(ctx, workers, managers),
		h.sweepProfiles(ctx, r),
	)
}

func (h *Sweep) exportCounts(ctx context.Context) error {
	counts, err := h.Board.Counts(ctx)
	if err != nil {
		return err
	}
	h.Metrics.Cards.Reset()
	for status, count := range counts {
		h.Metrics.Cards.WithLabelValues(status).Set(float64(count))
	}
	return nil
}

// sweepStrangers covers every non-terminal card whose assignee is not a farm
// worker, "default" among them. It is status-independent because `default` is
// spawnable by every user: a card caught only once it reaches ready is caught
// after the run it asked for has already started, so a running one is stopped
// before it is blocked.
func (h *Sweep) sweepStrangers(ctx context.Context, r *roster.Roster, workers []string, blocked map[string]bool) error {
	queued, err := h.Board.UnknownAssignees(ctx, workers)
	errs := []error{err}
	for _, card := range queued {
		errs = append(errs, h.blockStranger(ctx, r, card.ID, card.Status, card.Assignee, false, blocked))
	}
	running, err := h.Board.Running(ctx)
	errs = append(errs, err)
	for _, card := range running {
		if card.Assignee == "" || slices.Contains(workers, card.Assignee) {
			continue
		}
		stopped, err := h.stopRun(ctx, card)
		errs = append(errs, err,
			h.blockStranger(ctx, r, card.ID, statusRunning, card.Assignee, stopped, blocked))
	}
	return errors.Join(errs...)
}

func (h *Sweep) blockStranger(ctx context.Context, r *roster.Roster, id, status, assignee string, stopped bool, blocked map[string]bool) error {
	if blocked[id] {
		return nil
	}
	reason := fmt.Sprintf("assignee %q is not a farm worker; reassign with hermes kanban assign", assignee)
	if err := h.block(ctx, r, id, reason, blocked); err != nil {
		return err
	}
	h.post(ctx, fmt.Sprintf("card %s (%s): %s; %s", id, status, reason, aftermath(stopped)))
	return nil
}

func aftermath(stopped bool) string {
	if stopped {
		return "the run was stopped and the card is blocked (transient)"
	}
	return "the card is blocked (transient)"
}

// stopRun ends the run behind a card the sweep is about to block. A worker
// pid that names no run unit is a run that has already gone, not a failure:
// the card is still blocked, which is what stops it being claimed again.
func (h *Sweep) stopRun(ctx context.Context, card board.RunningCard) (bool, error) {
	if card.WorkerPID <= 0 {
		return false, nil
	}
	unit, live := h.runUnitFor(card.WorkerPID)
	if !live {
		return false, nil
	}
	match := runUnitName.FindStringSubmatch(unit)
	if match == nil {
		return false, fmt.Errorf("card %s: %q is not a farm run unit", card.ID, unit)
	}
	if err := h.Units.RunStop(ctx, match[1], unit); err != nil {
		return false, err
	}
	return true, nil
}

// sweepCrossCreated is detection, not prevention: created_by is a column any
// board writer sets, so what this buys is that the abuse is visible and slow
// rather than silent.
func (h *Sweep) sweepCrossCreated(ctx context.Context, r *roster.Roster, workers []string, blocked map[string]bool) error {
	cards, err := h.Board.CrossCreated(ctx, workers)
	errs := []error{err}
	for _, card := range cards {
		if blocked[card.ID] {
			continue
		}
		reason := fmt.Sprintf("worker %s created this card for %s; workers may not create cards for other agents",
			card.CreatedBy, card.Assignee)
		if err := h.block(ctx, r, card.ID, reason, blocked); err != nil {
			errs = append(errs, err)
			continue
		}
		h.post(ctx, fmt.Sprintf("card %s (%s): %s; the card is blocked (transient)", card.ID, card.Status, reason))
	}
	return errors.Join(errs...)
}

// sweepStranded counts a ready card only when the farm could have claimed it
// and did not. A card queued behind a full cap, or behind a paused loop, is
// backpressure rather than an anomaly, and reporting it would make the soak's
// "stranded cards 0" assertion fire on ordinary queueing.
func (h *Sweep) sweepStranded(ctx context.Context, workers []string) error {
	if !h.hasSpareSlot() {
		h.Metrics.CardsStranded.Set(0)
		return nil
	}
	cards, err := h.Board.Stranded(ctx, strandedAfter)
	if err != nil {
		return err
	}
	var waiting []string
	for _, card := range cards {
		if slices.Contains(workers, card.Assignee) {
			waiting = append(waiting, fmt.Sprintf("%s (%s)", card.ID, card.Assignee))
		}
	}
	h.Metrics.CardsStranded.Set(float64(len(waiting)))
	if len(waiting) > 0 {
		h.post(ctx, fmt.Sprintf("ready cards older than %s while the farm had a free run slot: %s",
			strandedAfter, strings.Join(waiting, ", ")))
	}
	return nil
}

func (h *Sweep) hasSpareSlot() bool {
	if h.Capacity == nil {
		return false
	}
	occupied, capacity, paused := h.Capacity()
	return !paused && occupied < capacity
}

// sweepNotifierSubs undoes the one forgery the pinned schema can attribute: a
// manager notifier on a card a worker created. The unsubscribe runs as that
// manager, the only user whose profile the row is stamped with, and carries
// exactly the four keys of the row's primary key.
func (h *Sweep) sweepNotifierSubs(ctx context.Context, workers, managers []string) error {
	subs, err := h.Board.ForgedNotifierSubs(ctx, workers, managers)
	errs := []error{err}
	for _, sub := range subs {
		args := []string{"notify-unsubscribe", sub.TaskID, "--platform", sub.Platform, "--chat-id", sub.ChatID}
		if sub.Thread != "" {
			args = append(args, "--thread-id", sub.Thread)
		}
		if _, err := h.Units.Kanban(ctx, sub.NotifierProfile, args...); err != nil {
			errs = append(errs, err)
			continue
		}
		h.Metrics.NotifySubBlocksTotal.WithLabelValues(sub.Creator, sub.NotifierProfile).Inc()
		h.post(ctx, fmt.Sprintf(
			"worker %s subscribed manager %s as the notifier of card %s; the subscription on %s was removed",
			sub.Creator, sub.NotifierProfile, sub.TaskID, sub.Target()))
	}
	return errors.Join(errs...)
}

// sweepProfiles is the fallback for the immutable flags: it hashes both
// rendered files of every enabled agent against the baseline. A file the
// sweep cannot read is drift — the flags and the ownership are exactly what a
// tampering agent would have to defeat, and a read that fails is evidence of
// that attempt, not of health.
func (h *Sweep) sweepProfiles(ctx context.Context, r *roster.Roster) error {
	recorded, err := readDigests(h.Baseline)
	if err != nil {
		return err
	}
	seeded := false
	for _, agent := range r.EnabledAgents() {
		drift, adopted := h.checkProfile(ctx, agent, recorded)
		seeded = seeded || adopted
		h.Metrics.ProfileDrift.WithLabelValues(agent.Name).Set(drift)
	}
	if !seeded {
		return nil
	}
	return writeDigests(h.Baseline, recorded)
}

func (h *Sweep) checkProfile(ctx context.Context, agent roster.Agent, recorded ProfileDigests) (drift float64, adopted bool) {
	current, err := h.digestProfile(agent)
	if err != nil {
		h.post(ctx, fmt.Sprintf("agent %s: its rendered profile could not be read, which the sweep reads as drift: %v",
			agent.Name, err))
		return 1, false
	}
	want, known := recorded.Profiles[agent.Name]
	if !known {
		recorded.Profiles[agent.Name] = current
		return 0, true
	}
	changed := changedFiles(want, current)
	if len(changed) == 0 {
		return 0, false
	}
	h.post(ctx, fmt.Sprintf("agent %s: its rendered profile no longer matches the baseline (%s); it was changed after apply rendered it",
		agent.Name, strings.Join(changed, ", ")))
	return 1, false
}

func changedFiles(want, current ProfileDigest) []string {
	var changed []string
	if want.Config != current.Config {
		changed = append(changed, configFile)
	}
	if want.Soul != current.Soul {
		changed = append(changed, soulFile)
	}
	return changed
}

func (h *Sweep) digestProfile(agent roster.Agent) (ProfileDigest, error) {
	home := agent.HermesHome(roster.Farm{HomeRoot: h.HomeRoot})
	config, configErr := digest(filepath.Join(home, configFile))
	soul, soulErr := digest(filepath.Join(home, soulFile))
	if err := errors.Join(configErr, soulErr); err != nil {
		return ProfileDigest{}, err
	}
	return ProfileDigest{Config: config, Soul: soul}, nil
}

func digest(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

// readDigests treats an absent or empty baseline as an empty one: apply
// creates the file before there is anything to record in it.
func readDigests(path string) (ProfileDigests, error) {
	empty := ProfileDigests{Profiles: map[string]ProfileDigest{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return empty, nil
	}
	if err != nil {
		return empty, fmt.Errorf("read profile baseline: %w", err)
	}
	if len(strings.TrimSpace(string(data))) == 0 {
		return empty, nil
	}
	var recorded ProfileDigests
	if err := json.Unmarshal(data, &recorded); err != nil {
		return empty, fmt.Errorf("read profile baseline %s: %w", path, err)
	}
	if recorded.Profiles == nil {
		recorded.Profiles = map[string]ProfileDigest{}
	}
	return recorded, nil
}

func writeDigests(path string, recorded ProfileDigests) (err error) {
	data, marshalErr := json.Marshal(recorded)
	if marshalErr != nil {
		return fmt.Errorf("write profile baseline: %w", marshalErr)
	}
	temp, createErr := os.CreateTemp(filepath.Dir(path), ".profile-hashes-*")
	if createErr != nil {
		return fmt.Errorf("write profile baseline: %w", createErr)
	}
	name := temp.Name()
	defer func() {
		if err != nil {
			err = errors.Join(fmt.Errorf("write profile baseline %s: %w", path, err), os.Remove(name))
		}
	}()
	if err = writeAll(temp, data); err != nil {
		return err
	}
	// #nosec G302 -- apply owns this path as snowfarm:snowfarm 0640, and a
	// tighter mode written here would show as drift on the next plan.
	if err = os.Chmod(name, baselineMode); err != nil {
		return err
	}
	return os.Rename(name, path)
}

func writeAll(file *os.File, data []byte) error {
	_, err := file.Write(data)
	return errors.Join(err, file.Close())
}

func (h *Sweep) block(ctx context.Context, r *roster.Roster, id, reason string, blocked map[string]bool) error {
	managers := r.EnabledManagers()
	if len(managers) == 0 {
		return fmt.Errorf("card %s needs blocking, but no enabled manager can run the block", id)
	}
	if _, err := h.Units.Kanban(ctx, managers[0].Name, "block", id, reason, "--kind", blockKind); err != nil {
		return err
	}
	blocked[id] = true
	return nil
}

func (h *Sweep) runUnitFor(pid int) (string, bool) {
	resolve := h.UnitForPID
	if resolve == nil {
		resolve = dispatch.UnitForPID
	}
	unit, err := resolve(pid)
	if err != nil {
		return "", false
	}
	return unit, true
}

func (h *Sweep) post(ctx context.Context, text string) {
	if h.Post != nil {
		h.Post(ctx, text)
	}
}

func agentNames(agents []roster.Agent) []string {
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		names = append(names, agent.Name)
	}
	return names
}
