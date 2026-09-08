package board

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	platformDiscord = "discord"
	chatTypeThread  = "thread"
	outcomeDone     = "done"
	eventCompleted  = "completed"
)

type Reader struct {
	db   *sql.DB
	path string
}

type RunningCard struct {
	ID           string
	Assignee     string
	WorkerPID    int
	StartedAt    time.Time
	ClaimExpires time.Time
	MaxRuntime   time.Duration
	RunID        string
}

type CardRef struct {
	ID        string
	Assignee  string
	Title     string
	CreatedBy string
	Status    string
	CreatedAt time.Time
}

type NotifierSub struct {
	TaskID          string
	Platform        string
	ChatID          string
	ChatType        string
	Thread          string
	NotifierProfile string
	Creator         string
}

func (s NotifierSub) Target() string {
	if s.ChatType == chatTypeThread && s.Thread != "" {
		return s.Thread
	}
	return s.ChatID
}

// Open prepares a read-only handle on the board. It connects on the first
// read rather than here: on a fresh host the board does not exist until a
// Hermes process creates it, and refusing to open would stop the supervisor
// that has to reconcile the guild before any gateway can run. Every read
// reports the board as unreachable until the file appears.
func Open(path string) (*Reader, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, fmt.Errorf("open board %s: %w", path, err)
	}
	return &Reader{db: db, path: path}, nil
}

// Reachable reports whether the board can be read now.
func (r *Reader) Reachable(ctx context.Context) error {
	if err := r.db.PingContext(ctx); err != nil {
		return fmt.Errorf("open board %s: %w", r.path, err)
	}
	return nil
}

func (r *Reader) Close() error {
	return r.db.Close()
}

func (r *Reader) Counts(ctx context.Context) (map[string]int, error) {
	rows, err := collect(ctx, r.db, scanStatusCount, `SELECT status, COUNT(*) FROM tasks GROUP BY status`)
	if err != nil {
		return nil, fmt.Errorf("count cards: %w", err)
	}
	counts := make(map[string]int, len(rows))
	for _, row := range rows {
		counts[row.status] = row.count
	}
	return counts, nil
}

func (r *Reader) Running(ctx context.Context) ([]RunningCard, error) {
	cards, err := collect(ctx, r.db, scanRunningCard,
		`SELECT t.id, t.assignee, t.worker_pid,
		        COALESCE(r.started_at, t.started_at), t.claim_expires,
		        t.max_runtime_seconds, t.current_run_id
		 FROM tasks t LEFT JOIN task_runs r ON r.id = t.current_run_id
		 WHERE t.status = 'running' ORDER BY t.id`)
	if err != nil {
		return nil, fmt.Errorf("list running cards: %w", err)
	}
	return cards, nil
}

func (r *Reader) Stranded(ctx context.Context, olderThan time.Duration) ([]CardRef, error) {
	cards, err := collect(ctx, r.db, scanCardRef,
		`SELECT `+cardColumns+` FROM tasks WHERE status = 'ready' AND claim_lock IS NULL ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list stranded cards: %w", err)
	}
	cutoff := time.Now().UTC().Add(-olderThan)
	return filter(cards, func(card CardRef) bool {
		return !card.CreatedAt.IsZero() && card.CreatedAt.Before(cutoff)
	}), nil
}

func (r *Reader) UnknownAssignees(ctx context.Context, known []string) ([]CardRef, error) {
	cards, err := collect(ctx, r.db, scanCardRef,
		`SELECT `+cardColumns+` FROM tasks
		 WHERE status NOT IN ('done', 'blocked', 'archived') ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list unknown assignees: %w", err)
	}
	return filter(cards, func(card CardRef) bool {
		return card.Assignee != "" && !slices.Contains(known, card.Assignee)
	}), nil
}

func (r *Reader) CrossCreated(ctx context.Context, workers []string) ([]CardRef, error) {
	if len(workers) == 0 {
		return nil, nil
	}
	cards, err := collect(ctx, r.db, scanCardRef,
		`SELECT `+cardColumns+` FROM tasks
		 WHERE status NOT IN ('done', 'blocked', 'archived') ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("list cross-created cards: %w", err)
	}
	return filter(cards, func(card CardRef) bool {
		return slices.Contains(workers, card.CreatedBy) &&
			card.Assignee != "" && card.Assignee != card.CreatedBy
	}), nil
}

func (r *Reader) ForgedNotifierSubs(ctx context.Context, workers, managers []string) ([]NotifierSub, error) {
	if len(workers) == 0 || len(managers) == 0 {
		return nil, nil
	}
	rows, err := collect(ctx, r.db, scanNotifierRow,
		`SELECT `+notifierColumns+` FROM kanban_notify_subs s
		 LEFT JOIN tasks t ON t.id = s.task_id
		 ORDER BY s.task_id, s.chat_id, s.thread_id`)
	if err != nil {
		return nil, fmt.Errorf("list notifier subscriptions: %w", err)
	}
	forged := filter(rows, func(row notifierRow) bool {
		return slices.Contains(managers, row.sub.NotifierProfile) &&
			slices.Contains(workers, row.sub.Creator)
	})
	subs := make([]NotifierSub, 0, len(forged))
	for _, row := range forged {
		subs = append(subs, row.sub)
	}
	return subs, nil
}

func (r *Reader) NotifierSubForTask(ctx context.Context, taskID string) (NotifierSub, bool, error) {
	rows, err := collect(ctx, r.db, scanNotifierRow,
		`SELECT `+notifierColumns+` FROM kanban_notify_subs s
		 LEFT JOIN tasks t ON t.id = s.task_id
		 WHERE s.task_id = ? AND s.platform = ?
		 ORDER BY s.chat_id, s.thread_id`, taskID, platformDiscord)
	if err != nil {
		return NotifierSub{}, false, fmt.Errorf("notifier subscription for %s: %w", taskID, err)
	}
	newest, ok := newestBy(rows, func(row notifierRow) time.Time { return row.at })
	return newest.sub, ok, nil
}

func (r *Reader) LastOutcome(ctx context.Context, taskID string) (string, bool, error) {
	events, err := collect(ctx, r.db, scanEvent,
		`SELECT kind, created_at FROM task_events
		 WHERE task_id = ? AND kind IN ('completed', 'blocked', 'gave_up', 'crashed', 'timed_out')`,
		taskID)
	if err != nil {
		return "", false, fmt.Errorf("last outcome for %s: %w", taskID, err)
	}
	newest, ok := newestBy(events, func(event terminalEvent) time.Time { return event.at })
	if !ok {
		return "", false, nil
	}
	if newest.kind == eventCompleted {
		return outcomeDone, true, nil
	}
	return newest.kind, true, nil
}

func (r *Reader) Size() (dbBytes, walBytes int64, err error) {
	if dbBytes, err = fileSize(r.path); err != nil {
		return 0, 0, err
	}
	if walBytes, err = fileSize(r.path + "-wal"); err != nil {
		return 0, 0, err
	}
	return dbBytes, walBytes, nil
}

const cardColumns = `id, title, status, assignee, created_by, created_at`

const notifierColumns = `s.task_id, s.platform, s.chat_id, s.thread_id, s.chat_type,
	s.notifier_profile, s.created_at, t.created_by`

type notifierRow struct {
	sub NotifierSub
	at  time.Time
}

type statusCount struct {
	status string
	count  int
}

type terminalEvent struct {
	kind string
	at   time.Time
}

func scanStatusCount(rows *sql.Rows) (statusCount, error) {
	var row statusCount
	if err := rows.Scan(&row.status, &row.count); err != nil {
		return statusCount{}, err
	}
	return row, nil
}

func scanCardRef(rows *sql.Rows) (CardRef, error) {
	var (
		card                                  CardRef
		title, assignee, createdBy, createdAt sql.NullString
	)
	if err := rows.Scan(&card.ID, &title, &card.Status, &assignee, &createdBy, &createdAt); err != nil {
		return CardRef{}, err
	}
	when, err := stampOf(createdAt)
	if err != nil {
		return CardRef{}, fmt.Errorf("card %s created_at: %w", card.ID, err)
	}
	card.Title = title.String
	card.Assignee = assignee.String
	card.CreatedBy = createdBy.String
	card.CreatedAt = when
	return card, nil
}

func scanRunningCard(rows *sql.Rows) (RunningCard, error) {
	var (
		card                                     RunningCard
		assignee, runID, startedAt, claimExpires sql.NullString
		maxRuntime                               sql.NullString
		workerPID                                sql.NullInt64
	)
	if err := rows.Scan(&card.ID, &assignee, &workerPID, &startedAt, &claimExpires,
		&maxRuntime, &runID); err != nil {
		return RunningCard{}, err
	}
	started, err := stampOf(startedAt)
	if err != nil {
		return RunningCard{}, fmt.Errorf("card %s started_at: %w", card.ID, err)
	}
	expires, err := stampOf(claimExpires)
	if err != nil {
		return RunningCard{}, fmt.Errorf("card %s claim_expires: %w", card.ID, err)
	}
	runtime, err := durationOf(maxRuntime)
	if err != nil {
		return RunningCard{}, fmt.Errorf("card %s max_runtime_seconds: %w", card.ID, err)
	}
	card.Assignee = assignee.String
	card.RunID = runID.String
	card.WorkerPID = int(workerPID.Int64)
	card.StartedAt = started
	card.ClaimExpires = expires
	card.MaxRuntime = runtime
	return card, nil
}

func scanNotifierRow(rows *sql.Rows) (notifierRow, error) {
	var (
		row                                 notifierRow
		thread, chatType, notifier, creator sql.NullString
		createdAt                           sql.NullString
	)
	if err := rows.Scan(&row.sub.TaskID, &row.sub.Platform, &row.sub.ChatID, &thread,
		&chatType, &notifier, &createdAt, &creator); err != nil {
		return notifierRow{}, err
	}
	when, err := stampOf(createdAt)
	if err != nil {
		return notifierRow{}, fmt.Errorf("subscription %s created_at: %w", row.sub.TaskID, err)
	}
	row.sub.Thread = thread.String
	row.sub.ChatType = chatType.String
	row.sub.NotifierProfile = notifier.String
	row.sub.Creator = creator.String
	row.at = when
	return row, nil
}

func scanEvent(rows *sql.Rows) (terminalEvent, error) {
	var (
		event     terminalEvent
		createdAt sql.NullString
	)
	if err := rows.Scan(&event.kind, &createdAt); err != nil {
		return terminalEvent{}, err
	}
	when, err := stampOf(createdAt)
	if err != nil {
		return terminalEvent{}, fmt.Errorf("event %s created_at: %w", event.kind, err)
	}
	event.at = when
	return event, nil
}

func collect[T any](ctx context.Context, db *sql.DB, scan func(*sql.Rows) (T, error), stmt string, args ...any) ([]T, error) {
	rows, err := db.QueryContext(ctx, stmt, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var items []T
	for rows.Next() {
		item, err := scan(rows)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return items, nil
}

func filter[T any](items []T, keep func(T) bool) []T {
	var kept []T
	for _, item := range items {
		if keep(item) {
			kept = append(kept, item)
		}
	}
	return kept
}

func newestBy[T any](items []T, at func(T) time.Time) (T, bool) {
	var (
		newest T
		found  bool
	)
	for _, item := range items {
		if !found || !at(item).Before(at(newest)) {
			newest, found = item, true
		}
	}
	return newest, found
}

var stampLayouts = []string{
	time.RFC3339Nano,
	"2006-01-02T15:04:05.999999999",
	"2006-01-02 15:04:05.999999999Z07:00",
	"2006-01-02 15:04:05.999999999",
}

func stampOf(value sql.NullString) (time.Time, error) {
	if !value.Valid {
		return time.Time{}, nil
	}
	raw := strings.TrimSpace(value.String)
	if raw == "" {
		return time.Time{}, nil
	}
	for _, layout := range stampLayouts {
		if when, err := time.Parse(layout, raw); err == nil {
			return when.UTC(), nil
		}
	}
	if epoch, err := strconv.ParseFloat(raw, 64); err == nil {
		seconds, fraction := math.Modf(epoch)
		return time.Unix(int64(seconds), int64(fraction*float64(time.Second))).UTC(), nil
	}
	return time.Time{}, fmt.Errorf("unrecognised timestamp %q", raw)
}

func durationOf(value sql.NullString) (time.Duration, error) {
	raw := strings.TrimSpace(value.String)
	if !value.Valid || raw == "" {
		return 0, nil
	}
	if seconds, err := strconv.ParseFloat(raw, 64); err == nil {
		return time.Duration(seconds * float64(time.Second)), nil
	}
	if runtime, err := time.ParseDuration(raw); err == nil {
		return runtime, nil
	}
	return 0, fmt.Errorf("unrecognised duration %q", raw)
}

func fileSize(path string) (int64, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return 0, nil
	}
	if err != nil {
		return 0, fmt.Errorf("size of %s: %w", path, err)
	}
	return info.Size(), nil
}
