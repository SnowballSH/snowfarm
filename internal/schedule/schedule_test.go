package schedule

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/robfig/cron/v3"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	logChannel   = "#snowsys-log"
	logChannelID = "300000000000000001"
)

type kanbanCall struct {
	agent string
	args  []string
}

type fakeUnits struct {
	mu     sync.Mutex
	calls  []kanbanCall
	create []byte
	err    error
}

func newFakeUnits() *fakeUnits {
	return &fakeUnits{create: []byte(`{"task_id":"t_ab12cd34"}`)}
}

func (f *fakeUnits) Kanban(_ context.Context, agent string, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, kanbanCall{agent: agent, args: slices.Clone(args)})
	if f.err != nil {
		return nil, f.err
	}
	if len(args) > 0 && args[0] == "create" {
		return f.create, nil
	}
	return nil, nil
}

func (f *fakeUnits) subverb(sub string) []kanbanCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []kanbanCall
	for _, call := range f.calls {
		if len(call.args) > 0 && call.args[0] == sub {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeUnits) Gateway(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("gateway is not part of the schedule runner")
}

func (f *fakeUnits) RunDispatch(context.Context, string) (string, error) {
	return "", errors.New("run-dispatch is not part of the schedule runner")
}

func (f *fakeUnits) RunMaintenance(context.Context, string) (string, error) {
	return "", errors.New("run-maintenance is not part of the schedule runner")
}

func (f *fakeUnits) RunStop(context.Context, string, string) error {
	return errors.New("run-stop is not part of the schedule runner")
}

func (f *fakeUnits) RunList(context.Context, string) ([]string, error) {
	return nil, errors.New("run-list is not part of the schedule runner")
}

func morningCheck() roster.Schedule {
	return roster.Schedule{
		Name:       "snowsys-morning",
		Cron:       "0 8 * * *",
		Location:   "America/New_York",
		Title:      "Morning SnowSys check",
		Body:       "Read the SnowSys Grafana overview and report anything anomalous.",
		Assignee:   "argus",
		Notifier:   "atlas",
		Channel:    logChannel,
		MaxRuntime: "2h",
		Priority:   5,
	}
}

func weeklyDigest() roster.Schedule {
	sc := morningCheck()
	sc.Name = "research-digest"
	sc.Cron = "30 9 * * 1"
	sc.Location = "UTC"
	sc.Title = "Weekly research digest"
	return sc
}

func testRoster(schedules ...roster.Schedule) *roster.Roster {
	return &roster.Roster{
		Farm: roster.Farm{HomeRoot: "/var/lib/farm", Location: "America/New_York"},
		Agents: []roster.Agent{
			{Name: "atlas", Tier: roster.TierManager, Enabled: true},
			{Name: "argus", Tier: roster.TierWorker, Enabled: true},
		},
		Schedules: schedules,
	}
}

func pointerTo[T any](value *T) *atomic.Pointer[T] {
	var p atomic.Pointer[T]
	p.Store(value)
	return &p
}

// The clock sits at 02:00 UTC, which is the previous evening in New York, so
// a key built from UTC and one built from the schedule's own location differ.
func testClock() func() time.Time {
	return func() time.Time { return time.Date(2026, 9, 11, 2, 0, 0, 0, time.UTC) }
}

type testRunner struct {
	*Runner
	units *fakeUnits
	logs  *bytes.Buffer

	mu    sync.Mutex
	posts []string
}

func newRunner(t *testing.T, r *roster.Roster) *testRunner {
	t.Helper()
	tr := &testRunner{units: newFakeUnits(), logs: &bytes.Buffer{}}
	tr.Runner = &Runner{
		Roster:   pointerTo(r),
		Units:    tr.units,
		Channels: map[string]string{logChannel: logChannelID},
		Post:     func(_ context.Context, text string) { tr.addPost(text) },
		Now:      testClock(),
		Log:      slog.New(slog.NewTextHandler(tr.logs, &slog.HandlerOptions{Level: slog.LevelInfo})),
	}
	t.Cleanup(tr.Stop)
	return tr
}

func (tr *testRunner) addPost(text string) {
	tr.mu.Lock()
	defer tr.mu.Unlock()
	tr.posts = append(tr.posts, text)
}

func (tr *testRunner) entries(t *testing.T) []cron.Entry {
	t.Helper()
	if tr.cron == nil {
		return nil
	}
	return tr.cron.Entries()
}

func TestFireCreatesAndSubscribes(t *testing.T) {
	sc := morningCheck()
	tr := newRunner(t, testRoster(sc))

	if err := tr.Fire(context.Background(), sc); err != nil {
		t.Fatalf("Fire: %v", err)
	}

	created := tr.units.subverb("create")
	if len(created) != 1 {
		t.Fatalf("create calls = %d, want 1", len(created))
	}
	if created[0].agent != "atlas" {
		t.Errorf("create ran as %q, want atlas", created[0].agent)
	}
	wantCreate := []string{
		"create", "Morning SnowSys check",
		"--body", "Read the SnowSys Grafana overview and report anything anomalous.",
		"--assignee", "argus",
		"--priority", "5",
		"--max-runtime", "2h",
		"--idempotency-key", "snowsys-morning-2026-09-10",
		"--json",
	}
	if !slices.Equal(created[0].args, wantCreate) {
		t.Errorf("create argv\n got %q\nwant %q", created[0].args, wantCreate)
	}

	subscribed := tr.units.subverb("notify-subscribe")
	if len(subscribed) != 1 {
		t.Fatalf("notify-subscribe calls = %d, want 1", len(subscribed))
	}
	if subscribed[0].agent != "atlas" {
		t.Errorf("notify-subscribe ran as %q, want atlas", subscribed[0].agent)
	}
	wantSubscribe := []string{
		"notify-subscribe", "t_ab12cd34",
		"--platform", "discord",
		"--chat-id", logChannelID,
		"--chat-type", "channel",
		"--notifier-profile", "atlas",
		"--delivery-mode", "notify+wake",
	}
	if !slices.Equal(subscribed[0].args, wantSubscribe) {
		t.Errorf("notify-subscribe argv\n got %q\nwant %q", subscribed[0].args, wantSubscribe)
	}
}

func TestFireReadsEitherCreatedIDShape(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		want string
	}{
		{"json id", `{"id":"t_ab12cd34","status":"ready"}`, "t_ab12cd34"},
		{"json task_id", `{"task_id":"t_ff00ff00"}`, "t_ff00ff00"},
		{"json after a preamble", "loaded profile atlas\n{\"task_id\":\"t_0123abcd\"}\n", "t_0123abcd"},
		{"pretty json", "{\n  \"task_id\": \"t_beef0001\"\n}\n", "t_beef0001"},
		{"created line", "Created t_deadbeef in ready\n", "t_deadbeef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sc := morningCheck()
			tr := newRunner(t, testRoster(sc))
			tr.units.create = []byte(tc.out)

			if err := tr.Fire(context.Background(), sc); err != nil {
				t.Fatalf("Fire: %v", err)
			}
			subscribed := tr.units.subverb("notify-subscribe")
			if len(subscribed) != 1 {
				t.Fatalf("notify-subscribe calls = %d, want 1", len(subscribed))
			}
			if got := subscribed[0].args[1]; got != tc.want {
				t.Errorf("subscribed task %q, want %q", got, tc.want)
			}
		})
	}
}

func TestFireRefusesUnreadableCreateOutput(t *testing.T) {
	sc := morningCheck()
	tr := newRunner(t, testRoster(sc))
	tr.units.create = []byte("nothing that names a task\n")

	err := tr.Fire(context.Background(), sc)
	if err == nil {
		t.Fatal("Fire accepted output naming no task id")
	}
	if got := len(tr.units.subverb("notify-subscribe")); got != 0 {
		t.Errorf("notify-subscribe calls = %d, want 0", got)
	}
}

func TestFireIsIdempotentPerDay(t *testing.T) {
	sc := morningCheck()
	tr := newRunner(t, testRoster(sc))

	for i := range 2 {
		if err := tr.Fire(context.Background(), sc); err != nil {
			t.Fatalf("Fire %d: %v", i, err)
		}
	}

	created := tr.units.subverb("create")
	if len(created) != 2 {
		t.Fatalf("create calls = %d, want 2", len(created))
	}
	first, second := keyOf(t, created[0].args), keyOf(t, created[1].args)
	if first != second {
		t.Errorf("second fire used key %q, first used %q", second, first)
	}
	if first != "snowsys-morning-2026-09-10" {
		t.Errorf("idempotency key = %q, want snowsys-morning-2026-09-10", first)
	}
	if got := len(tr.units.subverb("notify-subscribe")); got != 2 {
		t.Errorf("notify-subscribe calls = %d, want 2", got)
	}
}

func keyOf(t *testing.T, args []string) string {
	t.Helper()
	for i, arg := range args {
		if arg == "--idempotency-key" && i+1 < len(args) {
			return args[i+1]
		}
	}
	t.Fatalf("argv %q carries no --idempotency-key", args)
	return ""
}

func TestFireRefusesADisabledAgent(t *testing.T) {
	sc := morningCheck()
	r := testRoster(sc)
	r.Agents[1].Enabled = false
	tr := newRunner(t, r)

	if err := tr.Fire(context.Background(), sc); err == nil {
		t.Fatal("Fire created a card for a disabled assignee")
	}
	if got := len(tr.units.calls); got != 0 {
		t.Errorf("kanban calls = %d, want 0", got)
	}
}

func TestFireReportsAnUnknownChannel(t *testing.T) {
	sc := morningCheck()
	sc.Channel = "#not-reconciled"
	tr := newRunner(t, testRoster(sc))

	err := tr.Fire(context.Background(), sc)
	if err == nil || !strings.Contains(err.Error(), "#not-reconciled") {
		t.Fatalf("Fire error = %v, want one naming #not-reconciled", err)
	}
	if got := len(tr.units.subverb("create")); got != 1 {
		t.Errorf("create calls = %d, want 1", got)
	}
	if got := len(tr.units.subverb("notify-subscribe")); got != 0 {
		t.Errorf("notify-subscribe calls = %d, want 0", got)
	}
}

func TestStartHonoursLocation(t *testing.T) {
	sc := morningCheck()
	sched, loc, err := parseSchedule(sc)
	if err != nil {
		t.Fatalf("parseSchedule: %v", err)
	}
	for _, tc := range []struct {
		name string
		from time.Time
		want time.Time
	}{
		{"daylight saving", time.Date(2026, 7, 1, 0, 0, 0, 0, loc), time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
		{"standard time", time.Date(2026, 1, 1, 0, 0, 0, 0, loc), time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)},
		{"daylight saving from a UTC instant", time.Date(2026, 7, 1, 4, 0, 0, 0, time.UTC), time.Date(2026, 7, 1, 12, 0, 0, 0, time.UTC)},
		{"standard time from a UTC instant", time.Date(2026, 1, 1, 5, 0, 0, 0, time.UTC), time.Date(2026, 1, 1, 13, 0, 0, 0, time.UTC)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := sched.Next(tc.from).UTC(); !got.Equal(tc.want) {
				t.Errorf("next after %s = %s, want %s", tc.from, got, tc.want)
			}
		})
	}
}

func TestStartRegistersOneEntryPerSchedule(t *testing.T) {
	tr := newRunner(t, testRoster(morningCheck(), weeklyDigest()))

	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	entries := tr.entries(t)
	if len(entries) != 2 {
		t.Fatalf("registered %d entries, want 2", len(entries))
	}
	if got := tr.cron.Location().String(); got != "America/New_York" {
		t.Errorf("scheduler location = %q, want America/New_York", got)
	}
	wantLocations := []string{"America/New_York", "UTC"}
	for i, entry := range entries {
		spec, ok := entry.Schedule.(*cron.SpecSchedule)
		if !ok {
			t.Fatalf("entry %d schedule is %T, want *cron.SpecSchedule", i, entry.Schedule)
		}
		if got := spec.Location.String(); got != wantLocations[i] {
			t.Errorf("entry %d location = %q, want %q", i, got, wantLocations[i])
		}
	}
}

func TestStartSkipsDisabledSchedule(t *testing.T) {
	r := testRoster(morningCheck())
	r.Agents[1].Enabled = false
	tr := newRunner(t, r)

	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(tr.entries(t)); got != 0 {
		t.Fatalf("registered %d entries for a disabled assignee, want 0", got)
	}
	skips := skipLines(tr.logs.String())
	if len(skips) != 1 {
		t.Fatalf("logged %d skip lines, want 1: %q", len(skips), tr.logs.String())
	}
	if !strings.Contains(skips[0], "snowsys-morning") || !strings.Contains(skips[0], "argus") {
		t.Errorf("skip line %q names neither the schedule nor the agent", skips[0])
	}

	enabled := testRoster(morningCheck())
	tr.Roster.Store(enabled)
	if err := tr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(tr.entries(t)); got != 1 {
		t.Fatalf("registered %d entries after enabling argus, want 1", got)
	}
}

func skipLines(logs string) []string {
	var out []string
	for line := range strings.Lines(logs) {
		if strings.Contains(line, "skipped") {
			out = append(out, strings.TrimSpace(line))
		}
	}
	return out
}

func TestReloadDropsADisabledSchedule(t *testing.T) {
	tr := newRunner(t, testRoster(morningCheck()))
	if err := tr.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(tr.entries(t)); got != 1 {
		t.Fatalf("registered %d entries, want 1", got)
	}

	disabled := testRoster(morningCheck())
	disabled.Agents[0].Enabled = false
	tr.Roster.Store(disabled)
	if err := tr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(tr.entries(t)); got != 0 {
		t.Fatalf("registered %d entries for a disabled notifier, want 0", got)
	}
}

func TestReloadIsSafeBeforeStart(t *testing.T) {
	tr := newRunner(t, testRoster(morningCheck()))

	if err := tr.Reload(context.Background()); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	if got := len(tr.entries(t)); got != 1 {
		t.Fatalf("registered %d entries, want 1", got)
	}
}

func TestRunnerPostsAFailedFire(t *testing.T) {
	sc := morningCheck()
	tr := newRunner(t, testRoster(sc))
	tr.units.err = errors.New("exit status 2")

	tr.fire(context.Background(), sc)

	tr.mu.Lock()
	defer tr.mu.Unlock()
	if len(tr.posts) != 1 {
		t.Fatalf("posts = %d, want 1: %q", len(tr.posts), tr.posts)
	}
	if !strings.Contains(tr.posts[0], sc.Name) {
		t.Errorf("post %q does not name the schedule", tr.posts[0])
	}
}

func TestStartRefusesWithoutARoster(t *testing.T) {
	tr := newRunner(t, testRoster())
	tr.Roster.Store(nil)

	if err := tr.Start(context.Background()); err == nil {
		t.Fatal("Start accepted a runner holding no roster")
	}
}
