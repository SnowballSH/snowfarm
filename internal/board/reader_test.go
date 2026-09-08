package board

import (
	"context"
	"database/sql"
	"maps"
	"os"
	"path/filepath"
	"testing"
	"time"

	_ "modernc.org/sqlite"
)

var now = time.Now().UTC().Truncate(time.Second)

type taskRow struct {
	id           string
	title        string
	status       string
	assignee     string
	createdBy    string
	createdAt    time.Time
	claimLock    string
	claimExpires time.Time
	startedAt    time.Time
	workerPID    int
	maxRuntime   time.Duration
	runID        int
	runStartedAt time.Time
}

type eventRow struct {
	taskID    string
	kind      string
	createdAt time.Time
}

type subRow struct {
	taskID          string
	platform        string
	chatID          string
	threadID        string
	chatType        string
	notifierProfile string
	createdAt       time.Time
}

func baseTasks() []taskRow {
	return []taskRow{
		{
			id:        "t_stale",
			title:     "an hour in ready",
			status:    "ready",
			assignee:  "hestia",
			createdBy: "atlas",
			createdAt: now.Add(-time.Hour),
		},
		{
			id:        "t_fresh",
			title:     "just filed",
			status:    "ready",
			assignee:  "nobody",
			createdBy: "atlas",
			createdAt: now.Add(-time.Minute),
		},
		{
			id:           "t_running",
			title:        "in flight",
			status:       "running",
			assignee:     "hestia",
			createdBy:    "atlas",
			createdAt:    now.Add(-2 * time.Hour),
			claimLock:    "lock-1",
			claimExpires: now.Add(15 * time.Minute),
			startedAt:    now.Add(-5 * time.Minute),
			workerPID:    4242,
			maxRuntime:   2 * time.Hour,
			runID:        1,
		},
	}
}

func nullString(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func nullStamp(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC().Unix()
}

func runStart(task taskRow) time.Time {
	if !task.runStartedAt.IsZero() {
		return task.runStartedAt
	}
	return task.startedAt
}

func nullInt(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func writeBoard(t *testing.T, tasks []taskRow, events []eventRow, subs []subRow) string {
	t.Helper()
	schema, err := os.ReadFile(filepath.Join("testdata", "schema.sql"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	path := filepath.Join(t.TempDir(), "kanban.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open writer: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close writer: %v", err)
		}
	}()
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply schema: %v", err)
	}
	for _, task := range tasks {
		_, err := db.Exec(
			`INSERT INTO tasks (id, title, status, assignee, created_by, created_at,
			 claim_lock, claim_expires, started_at, worker_pid, max_runtime_seconds,
			 current_run_id)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			task.id, task.title, task.status, nullString(task.assignee), nullString(task.createdBy),
			task.createdAt.UTC().Unix(), nullString(task.claimLock),
			nullStamp(task.claimExpires), nullStamp(task.startedAt), nullInt(task.workerPID),
			nullInt(int(task.maxRuntime/time.Second)), nullInt(task.runID))
		if err != nil {
			t.Fatalf("insert task %s: %v", task.id, err)
		}
		if task.runID == 0 {
			continue
		}
		if _, err := db.Exec(
			`INSERT INTO task_runs (id, task_id, profile, status, claim_lock, claim_expires,
			 max_runtime_seconds, started_at)
			 VALUES (?, ?, ?, 'running', ?, ?, ?, ?)`,
			task.runID, task.id, nullString(task.assignee), nullString(task.claimLock),
			nullStamp(task.claimExpires), nullInt(int(task.maxRuntime/time.Second)),
			runStart(task).UTC().Unix()); err != nil {
			t.Fatalf("insert run for %s: %v", task.id, err)
		}
	}
	for _, event := range events {
		if _, err := db.Exec(
			`INSERT INTO task_events (task_id, kind, created_at) VALUES (?, ?, ?)`,
			event.taskID, event.kind, event.createdAt.UTC().Unix()); err != nil {
			t.Fatalf("insert event %s/%s: %v", event.taskID, event.kind, err)
		}
	}
	for _, sub := range subs {
		if _, err := db.Exec(
			`INSERT INTO kanban_notify_subs (task_id, platform, chat_id, thread_id,
			 chat_type, notifier_profile, delivery_mode, created_at)
			 VALUES (?, ?, ?, ?, ?, ?, 'notify+wake', ?)`,
			sub.taskID, sub.platform, sub.chatID, sub.threadID, sub.chatType,
			sub.notifierProfile, sub.createdAt.UTC().Unix()); err != nil {
			t.Fatalf("insert sub %s: %v", sub.taskID, err)
		}
	}
	return path
}

func openBoard(t *testing.T, tasks []taskRow, events []eventRow, subs []subRow) *Reader {
	t.Helper()
	reader, err := Open(writeBoard(t, tasks, events, subs))
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Fatalf("close reader: %v", err)
		}
	})
	return reader
}

func ids(refs []CardRef) []string {
	out := make([]string, 0, len(refs))
	for _, ref := range refs {
		out = append(out, ref.ID)
	}
	return out
}

func assertIDs(t *testing.T, got, want []string) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("ids = %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("ids = %v, want %v", got, want)
		}
	}
}

func TestCounts(t *testing.T) {
	reader := openBoard(t, baseTasks(), nil, nil)
	counts, err := reader.Counts(context.Background())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	want := map[string]int{"ready": 2, "running": 1}
	if !maps.Equal(counts, want) {
		t.Fatalf("counts = %v, want %v", counts, want)
	}
}

func TestRunning(t *testing.T) {
	reader := openBoard(t, baseTasks(), nil, nil)
	running, err := reader.Running(context.Background())
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("running = %+v, want one card", running)
	}
	got := running[0]
	want := RunningCard{
		ID:           "t_running",
		Assignee:     "hestia",
		WorkerPID:    4242,
		StartedAt:    now.Add(-5 * time.Minute),
		ClaimExpires: now.Add(15 * time.Minute),
		MaxRuntime:   2 * time.Hour,
		RunID:        "1",
	}
	if got.ID != want.ID || got.Assignee != want.Assignee || got.WorkerPID != want.WorkerPID ||
		got.RunID != want.RunID || got.MaxRuntime != want.MaxRuntime ||
		!got.StartedAt.Equal(want.StartedAt) || !got.ClaimExpires.Equal(want.ClaimExpires) {
		t.Fatalf("running card = %+v, want %+v", got, want)
	}
}

func TestRunningWithoutMaxRuntime(t *testing.T) {
	tasks := baseTasks()
	tasks[2].maxRuntime = 0
	tasks[2].claimExpires = time.Time{}
	reader := openBoard(t, tasks, nil, nil)
	running, err := reader.Running(context.Background())
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("running = %+v, want one card", running)
	}
	if running[0].MaxRuntime != 0 || !running[0].ClaimExpires.IsZero() {
		t.Fatalf("running card = %+v, want zero max runtime and claim expiry", running[0])
	}
}

func TestRunningMeasuresFromTheActiveRun(t *testing.T) {
	tasks := baseTasks()
	tasks[2].startedAt = now.Add(-3 * time.Hour)
	tasks[2].runStartedAt = now.Add(-5 * time.Minute)
	reader := openBoard(t, tasks, nil, nil)
	running, err := reader.Running(context.Background())
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("running = %+v, want one card", running)
	}
	if !running[0].StartedAt.Equal(now.Add(-5 * time.Minute)) {
		t.Fatalf("started at = %v, want the active run's start %v",
			running[0].StartedAt, now.Add(-5*time.Minute))
	}
}

func TestRunningWithoutARun(t *testing.T) {
	tasks := baseTasks()
	tasks[2].runID = 0
	reader := openBoard(t, tasks, nil, nil)
	running, err := reader.Running(context.Background())
	if err != nil {
		t.Fatalf("running: %v", err)
	}
	if len(running) != 1 {
		t.Fatalf("running = %+v, want one card", running)
	}
	if running[0].RunID != "" || !running[0].StartedAt.Equal(now.Add(-5*time.Minute)) {
		t.Fatalf("running card = %+v, want no run id and the task's own start", running[0])
	}
}

func TestStranded(t *testing.T) {
	reader := openBoard(t, baseTasks(), nil, nil)
	stranded, err := reader.Stranded(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("stranded: %v", err)
	}
	assertIDs(t, ids(stranded), []string{"t_stale"})
	if stranded[0].Title != "an hour in ready" || stranded[0].Status != "ready" ||
		stranded[0].CreatedBy != "atlas" || !stranded[0].CreatedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("stranded card = %+v", stranded[0])
	}
}

func TestStrandedIgnoresClaimedCards(t *testing.T) {
	tasks := baseTasks()
	tasks[0].claimLock = "lock-2"
	reader := openBoard(t, tasks, nil, nil)
	stranded, err := reader.Stranded(context.Background(), 30*time.Minute)
	if err != nil {
		t.Fatalf("stranded: %v", err)
	}
	assertIDs(t, ids(stranded), nil)
}

func TestUnknownAssignees(t *testing.T) {
	tasks := append(baseTasks(), taskRow{
		id: "t_todo", title: "unassigned", status: "todo", createdBy: "atlas", createdAt: now,
	})
	reader := openBoard(t, tasks, nil, nil)
	unknown, err := reader.UnknownAssignees(context.Background(), []string{"hestia"})
	if err != nil {
		t.Fatalf("unknown assignees: %v", err)
	}
	assertIDs(t, ids(unknown), []string{"t_fresh"})
	if unknown[0].Assignee != "nobody" {
		t.Fatalf("assignee = %q, want nobody", unknown[0].Assignee)
	}
}

func TestUnknownAssigneesCoversEveryNonTerminalStatus(t *testing.T) {
	var tasks []taskRow
	for _, status := range []string{"triage", "todo", "scheduled", "ready", "running", "review"} {
		tasks = append(tasks, taskRow{
			id: "t_" + status, title: status, status: status,
			assignee: "default", createdBy: "atlas", createdAt: now,
		})
	}
	for _, status := range []string{"done", "blocked", "archived"} {
		tasks = append(tasks, taskRow{
			id: "t_gone_" + status, title: status, status: status,
			assignee: "default", createdBy: "atlas", createdAt: now,
		})
	}
	reader := openBoard(t, tasks, nil, nil)
	unknown, err := reader.UnknownAssignees(context.Background(), []string{"hestia"})
	if err != nil {
		t.Fatalf("unknown assignees: %v", err)
	}
	assertIDs(t, ids(unknown), []string{
		"t_ready", "t_review", "t_running", "t_scheduled", "t_todo", "t_triage",
	})
}

func TestUnknownAssigneesWithoutKnownAgents(t *testing.T) {
	reader := openBoard(t, baseTasks(), nil, nil)
	unknown, err := reader.UnknownAssignees(context.Background(), nil)
	if err != nil {
		t.Fatalf("unknown assignees: %v", err)
	}
	assertIDs(t, ids(unknown), []string{"t_fresh", "t_running", "t_stale"})
}

func TestCrossCreated(t *testing.T) {
	tasks := []taskRow{
		{id: "t_cross", status: "ready", assignee: "argus", createdBy: "hestia", createdAt: now},
		{id: "t_cross_running", status: "running", assignee: "argus", createdBy: "hestia", createdAt: now},
		{id: "t_self", status: "ready", assignee: "hestia", createdBy: "hestia", createdAt: now},
		{id: "t_manager", status: "ready", assignee: "argus", createdBy: "atlas", createdAt: now},
		{id: "t_done", status: "done", assignee: "argus", createdBy: "hestia", createdAt: now},
		{id: "t_blocked", status: "blocked", assignee: "argus", createdBy: "hestia", createdAt: now},
		{id: "t_archived", status: "archived", assignee: "argus", createdBy: "hestia", createdAt: now},
		{id: "t_unassigned", status: "triage", createdBy: "hestia", createdAt: now},
	}
	reader := openBoard(t, tasks, nil, nil)
	cross, err := reader.CrossCreated(context.Background(), []string{"hestia", "argus"})
	if err != nil {
		t.Fatalf("cross created: %v", err)
	}
	assertIDs(t, ids(cross), []string{"t_cross", "t_cross_running"})
}

func TestCrossCreatedWithoutWorkers(t *testing.T) {
	tasks := []taskRow{{id: "t_cross", status: "ready", assignee: "argus", createdBy: "hestia", createdAt: now}}
	reader := openBoard(t, tasks, nil, nil)
	cross, err := reader.CrossCreated(context.Background(), nil)
	if err != nil {
		t.Fatalf("cross created: %v", err)
	}
	assertIDs(t, ids(cross), nil)
}

func TestForgedNotifierSubs(t *testing.T) {
	tasks := []taskRow{
		{id: "t_worker", status: "ready", assignee: "hestia", createdBy: "hestia", createdAt: now},
		{id: "t_manager", status: "ready", assignee: "hestia", createdBy: "atlas", createdAt: now},
	}
	subs := []subRow{
		{taskID: "t_worker", platform: "discord", chatID: "c1", threadID: "th1", chatType: "thread", notifierProfile: "atlas", createdAt: now},
		{taskID: "t_worker", platform: "discord", chatID: "c2", chatType: "channel", notifierProfile: "hestia", createdAt: now},
		{taskID: "t_manager", platform: "discord", chatID: "c3", threadID: "th3", chatType: "thread", notifierProfile: "atlas", createdAt: now},
	}
	reader := openBoard(t, tasks, nil, subs)
	forged, err := reader.ForgedNotifierSubs(context.Background(), []string{"hestia"}, []string{"atlas", "iris"})
	if err != nil {
		t.Fatalf("forged notifier subs: %v", err)
	}
	if len(forged) != 1 {
		t.Fatalf("forged = %+v, want one row", forged)
	}
	want := NotifierSub{
		TaskID: "t_worker", Platform: "discord", ChatID: "c1",
		ChatType: "thread", Thread: "th1", NotifierProfile: "atlas", Creator: "hestia",
	}
	if forged[0] != want {
		t.Fatalf("forged = %+v, want %+v", forged[0], want)
	}
}

func TestForgedNotifierSubsWithEmptyRosters(t *testing.T) {
	tasks := []taskRow{{id: "t_worker", status: "ready", createdBy: "hestia", createdAt: now}}
	subs := []subRow{{taskID: "t_worker", platform: "discord", chatID: "c1", notifierProfile: "atlas", createdAt: now}}
	reader := openBoard(t, tasks, nil, subs)
	for _, tc := range []struct {
		name     string
		workers  []string
		managers []string
	}{
		{name: "no workers", managers: []string{"atlas"}},
		{name: "no managers", workers: []string{"hestia"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			forged, err := reader.ForgedNotifierSubs(context.Background(), tc.workers, tc.managers)
			if err != nil {
				t.Fatalf("forged notifier subs: %v", err)
			}
			if len(forged) != 0 {
				t.Fatalf("forged = %+v, want none", forged)
			}
		})
	}
}

func TestNotifierSubForTask(t *testing.T) {
	tasks := []taskRow{
		{id: "t_1", status: "running", assignee: "hestia", createdBy: "atlas", createdAt: now},
		{id: "t_2", status: "running", assignee: "hestia", createdBy: "atlas", createdAt: now},
	}
	subs := []subRow{
		{taskID: "t_1", platform: "discord", chatID: "c1", threadID: "th1", chatType: "thread", notifierProfile: "atlas", createdAt: now.Add(-time.Hour)},
		{taskID: "t_1", platform: "discord", chatID: "c2", chatType: "channel", notifierProfile: "iris", createdAt: now},
		{taskID: "t_1", platform: "slack", chatID: "c3", chatType: "channel", notifierProfile: "atlas", createdAt: now.Add(time.Hour)},
	}
	reader := openBoard(t, tasks, nil, subs)
	sub, ok, err := reader.NotifierSubForTask(context.Background(), "t_1")
	if err != nil {
		t.Fatalf("notifier sub: %v", err)
	}
	if !ok {
		t.Fatal("notifier sub not found for t_1")
	}
	if sub.ChatID != "c2" || sub.NotifierProfile != "iris" || sub.Target() != "c2" {
		t.Fatalf("sub = %+v, want the newest discord row", sub)
	}
	if _, ok, err := reader.NotifierSubForTask(context.Background(), "t_2"); err != nil || ok {
		t.Fatalf("notifier sub for t_2 = %v, %v, want none", ok, err)
	}
}

func TestTarget(t *testing.T) {
	for _, tc := range []struct {
		name string
		sub  NotifierSub
		want string
	}{
		{name: "thread", sub: NotifierSub{ChatID: "c1", ChatType: "thread", Thread: "th1"}, want: "th1"},
		{name: "channel", sub: NotifierSub{ChatID: "c1", ChatType: "channel"}, want: "c1"},
		{name: "thread type without thread id", sub: NotifierSub{ChatID: "c1", ChatType: "thread"}, want: "c1"},
		{name: "thread id without thread type", sub: NotifierSub{ChatID: "c1", ChatType: "channel", Thread: "th1"}, want: "c1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.sub.Target(); got != tc.want {
				t.Fatalf("target = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLastOutcome(t *testing.T) {
	tasks := []taskRow{
		{id: "t_done", status: "done", createdAt: now},
		{id: "t_crashed", status: "ready", createdAt: now},
		{id: "t_quiet", status: "ready", createdAt: now},
	}
	events := []eventRow{
		{taskID: "t_done", kind: "claimed", createdAt: now.Add(-time.Hour)},
		{taskID: "t_done", kind: "blocked", createdAt: now.Add(-30 * time.Minute)},
		{taskID: "t_done", kind: "completed", createdAt: now.Add(-time.Minute)},
		{taskID: "t_crashed", kind: "crashed", createdAt: now.Add(-time.Minute)},
		{taskID: "t_crashed", kind: "reclaimed", createdAt: now},
		{taskID: "t_quiet", kind: "claimed", createdAt: now},
	}
	reader := openBoard(t, tasks, events, nil)
	for _, tc := range []struct {
		taskID string
		want   string
		found  bool
	}{
		{taskID: "t_done", want: "done", found: true},
		{taskID: "t_crashed", want: "crashed", found: true},
		{taskID: "t_quiet"},
		{taskID: "t_missing"},
	} {
		t.Run(tc.taskID, func(t *testing.T) {
			outcome, ok, err := reader.LastOutcome(context.Background(), tc.taskID)
			if err != nil {
				t.Fatalf("last outcome: %v", err)
			}
			if ok != tc.found || outcome != tc.want {
				t.Fatalf("last outcome = %q, %v, want %q, %v", outcome, ok, tc.want, tc.found)
			}
		})
	}
}

func TestSize(t *testing.T) {
	path := writeBoard(t, baseTasks(), nil, nil)
	reader, err := Open(path)
	if err != nil {
		t.Fatalf("open reader: %v", err)
	}
	defer func() {
		if err := reader.Close(); err != nil {
			t.Fatalf("close reader: %v", err)
		}
	}()
	dbBytes, walBytes, err := reader.Size()
	if err != nil {
		t.Fatalf("size: %v", err)
	}
	if dbBytes <= 0 {
		t.Fatalf("db bytes = %d, want a non-empty database", dbBytes)
	}
	if walBytes != 0 {
		t.Fatalf("wal bytes = %d, want 0 with no sidecar", walBytes)
	}
	if err := os.WriteFile(path+"-wal", make([]byte, 4096), 0o600); err != nil {
		t.Fatalf("write wal sidecar: %v", err)
	}
	if _, walBytes, err = reader.Size(); err != nil || walBytes != 4096 {
		t.Fatalf("wal bytes = %d, %v, want 4096", walBytes, err)
	}
}

func TestOpenIsReadOnly(t *testing.T) {
	reader := openBoard(t, baseTasks(), nil, nil)
	_, err := reader.db.ExecContext(context.Background(),
		`INSERT INTO tasks (id, title, status, created_at) VALUES ('t_new', 'x', 'ready', 1788782400)`)
	if err == nil {
		t.Fatal("insert through the reader succeeded, want a read-only failure")
	}
}

func TestOpenMissingDatabase(t *testing.T) {
	if _, err := Open(filepath.Join(t.TempDir(), "absent.db")); err == nil {
		t.Fatal("open of a missing database succeeded, want an error")
	}
}

func TestOpenAppliesBusyTimeout(t *testing.T) {
	reader := openBoard(t, baseTasks(), nil, nil)
	var timeout int
	if err := reader.db.QueryRowContext(context.Background(), `PRAGMA busy_timeout`).Scan(&timeout); err != nil {
		t.Fatalf("read busy timeout: %v", err)
	}
	if timeout != 5000 {
		t.Fatalf("busy timeout = %d, want 5000", timeout)
	}
}

func TestStampOf(t *testing.T) {
	want := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		value sql.NullString
		want  time.Time
		fails bool
	}{
		{name: "null", value: sql.NullString{}},
		{name: "empty", value: sql.NullString{String: "  ", Valid: true}},
		{name: "rfc3339", value: sql.NullString{String: "2026-09-07T12:00:00Z", Valid: true}, want: want},
		{name: "rfc3339 offset", value: sql.NullString{String: "2026-09-07T08:00:00-04:00", Valid: true}, want: want},
		{name: "rfc3339 fractional", value: sql.NullString{String: "2026-09-07T12:00:00.250Z", Valid: true}, want: want.Add(250 * time.Millisecond)},
		{name: "naive iso", value: sql.NullString{String: "2026-09-07T12:00:00", Valid: true}, want: want},
		{name: "sqlite text", value: sql.NullString{String: "2026-09-07 12:00:00", Valid: true}, want: want},
		{name: "sqlite text with zone", value: sql.NullString{String: "2026-09-07 12:00:00+00:00", Valid: true}, want: want},
		{name: "epoch seconds", value: sql.NullString{String: "1788782400", Valid: true}, want: want},
		{name: "epoch fractional", value: sql.NullString{String: "1788782400.5", Valid: true}, want: want.Add(500 * time.Millisecond)},
		{name: "nonsense", value: sql.NullString{String: "yesterday", Valid: true}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := stampOf(tc.value)
			if tc.fails {
				if err == nil {
					t.Fatalf("stampOf(%q) = %v, want an error", tc.value.String, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("stampOf(%q): %v", tc.value.String, err)
			}
			if !got.Equal(tc.want) {
				t.Fatalf("stampOf(%q) = %v, want %v", tc.value.String, got, tc.want)
			}
		})
	}
}

func TestDurationOf(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value sql.NullString
		want  time.Duration
		fails bool
	}{
		{name: "null", value: sql.NullString{}},
		{name: "empty", value: sql.NullString{String: "", Valid: true}},
		{name: "seconds", value: sql.NullString{String: "7200", Valid: true}, want: 2 * time.Hour},
		{name: "fractional seconds", value: sql.NullString{String: "1.5", Valid: true}, want: 1500 * time.Millisecond},
		{name: "go duration", value: sql.NullString{String: "2h30m", Valid: true}, want: 150 * time.Minute},
		{name: "nonsense", value: sql.NullString{String: "a while", Valid: true}, fails: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := durationOf(tc.value)
			if tc.fails {
				if err == nil {
					t.Fatalf("durationOf(%q) = %v, want an error", tc.value.String, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("durationOf(%q): %v", tc.value.String, err)
			}
			if got != tc.want {
				t.Fatalf("durationOf(%q) = %v, want %v", tc.value.String, got, tc.want)
			}
		})
	}
}
