package hygiene

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	_ "modernc.org/sqlite"
)

const hestiaUnit = "snowfarm-run-hestia-7.service"

type unitCall struct {
	verb  string
	agent string
	args  []string
}

type fakeUnits struct {
	mu    sync.Mutex
	calls []unitCall
}

func (f *fakeUnits) Kanban(_ context.Context, agent string, args ...string) ([]byte, error) {
	f.record(unitCall{verb: "kanban", agent: agent, args: slices.Clone(args)})
	return nil, nil
}

func (f *fakeUnits) RunStop(_ context.Context, agent, unit string) error {
	f.record(unitCall{verb: "run-stop", agent: agent, args: []string{unit}})
	return nil
}

func (f *fakeUnits) record(call unitCall) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeUnits) subverb(sub string) []unitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []unitCall
	for _, call := range f.calls {
		if call.verb == "kanban" && len(call.args) > 0 && call.args[0] == sub {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeUnits) stops() []unitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []unitCall
	for _, call := range f.calls {
		if call.verb == "run-stop" {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeUnits) Gateway(context.Context, string, string) ([]byte, error) {
	return nil, errors.New("gateway is not part of the hygiene sweep")
}

func (f *fakeUnits) RunDispatch(context.Context, string) (string, error) {
	return "", errors.New("run-dispatch is not part of the hygiene sweep")
}

func (f *fakeUnits) RunMaintenance(context.Context, string) (string, error) {
	return "", errors.New("run-maintenance is not part of the hygiene sweep")
}

func (f *fakeUnits) RunList(context.Context, string) ([]string, error) {
	return nil, errors.New("run-list is not part of the hygiene sweep")
}

type card struct {
	id        string
	status    string
	assignee  string
	createdBy string
	createdAt time.Time
	workerPID int
}

type sub struct {
	taskID   string
	chatID   string
	threadID string
	chatType string
	notifier string
}

type harness struct {
	t        *testing.T
	sweep    *Sweep
	units    *fakeUnits
	reg      *metrics.Registry
	home     string
	baseline string

	mu        sync.Mutex
	posts     []string
	occupied  int
	capacity  int
	paused    bool
	units4PID map[int]string
}

func testRoster() *roster.Roster {
	return &roster.Roster{
		Farm: roster.Farm{HomeRoot: "/var/lib/farm"},
		Agents: []roster.Agent{
			{Name: "atlas", Tier: roster.TierManager, Enabled: true},
			{Name: "hestia", Tier: roster.TierWorker, Enabled: true},
			{Name: "argus", Tier: roster.TierWorker, Enabled: true},
			{Name: "euclid", Tier: roster.TierWorker, Enabled: false},
		},
		Guard: roster.GuardConfig{MaxConcurrentRuns: 1},
	}
}

func pointerTo[T any](value *T) *atomic.Pointer[T] {
	var p atomic.Pointer[T]
	p.Store(value)
	return &p
}

func newHarness(t *testing.T, r *roster.Roster, cards []card, subs []sub) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:         t,
		units:     &fakeUnits{},
		reg:       metrics.New(),
		home:      filepath.Join(dir, "farm"),
		baseline:  filepath.Join(dir, "profile-hashes.json"),
		capacity:  1,
		units4PID: map[int]string{},
	}
	path := filepath.Join(dir, "kanban.db")
	writeBoard(t, path, cards, subs)
	reader, err := board.Open(path)
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close board: %v", err)
		}
	})
	writeProfiles(t, h.home, enabledNames(r)...)
	writeBaseline(t, h.baseline, h.home, enabledNames(r)...)
	h.sweep = &Sweep{
		Roster:   pointerTo(r),
		Board:    reader,
		Units:    h.units,
		Post:     func(_ context.Context, text string) { h.addPost(text) },
		Metrics:  h.reg,
		HomeRoot: h.home,
		Baseline: h.baseline,
		Capacity: h.reportCapacity,
		UnitForPID: func(pid int) (string, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			unit, ok := h.units4PID[pid]
			if !ok {
				return "", fmt.Errorf("pid %d belongs to no run unit", pid)
			}
			return unit, nil
		},
	}
	return h
}

func enabledNames(r *roster.Roster) []string {
	var out []string
	for _, agent := range r.EnabledAgents() {
		out = append(out, agent.Name)
	}
	return out
}

func (h *harness) addPost(text string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts = append(h.posts, text)
}

func (h *harness) reportCapacity() (int, int, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.occupied, h.capacity, h.paused
}

func (h *harness) setCapacity(occupied, capacity int, paused bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.occupied, h.capacity, h.paused = occupied, capacity, paused
}

func (h *harness) mapPID(pid int, unit string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.units4PID[pid] = unit
}

func (h *harness) run() {
	h.t.Helper()
	if err := h.sweep.Run(context.Background()); err != nil {
		h.t.Fatalf("sweep: %v", err)
	}
}

func (h *harness) postList() []string {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.posts)
}

func writeBoard(t *testing.T, path string, cards []card, subs []sub) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open board writer: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Fatalf("close board writer: %v", err)
		}
	}()
	schema, err := os.ReadFile(filepath.Join("..", "board", "testdata", "schema.sql"))
	if err != nil {
		t.Fatalf("read board schema: %v", err)
	}
	if _, err := db.Exec(string(schema)); err != nil {
		t.Fatalf("apply board schema: %v", err)
	}
	for _, c := range cards {
		createdAt := c.createdAt
		if createdAt.IsZero() {
			createdAt = time.Now().UTC()
		}
		_, err := db.Exec(
			`INSERT INTO tasks (id, title, status, assignee, created_by, created_at, worker_pid)
			 VALUES (?, ?, ?, ?, ?, ?, ?)`,
			c.id, "card "+c.id, c.status, c.assignee, c.createdBy, createdAt.Unix(),
			nullable(c.workerPID))
		if err != nil {
			t.Fatalf("insert card %s: %v", c.id, err)
		}
	}
	for _, s := range subs {
		chatType := s.chatType
		if chatType == "" {
			chatType = "thread"
		}
		_, err := db.Exec(
			`INSERT INTO kanban_notify_subs (task_id, platform, chat_id, thread_id, chat_type,
			 notifier_profile, created_at) VALUES (?, 'discord', ?, ?, ?, ?, ?)`,
			s.taskID, s.chatID, s.threadID, chatType, s.notifier, time.Now().UTC().Unix())
		if err != nil {
			t.Fatalf("insert subscription %s: %v", s.taskID, err)
		}
	}
}

func nullable(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func gaugeValue(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("gauge %v: %v", labels, err)
	}
	return testutil.ToFloat64(gauge)
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	counter, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("counter %v: %v", labels, err)
	}
	return testutil.ToFloat64(counter)
}

func blockArgs(t *testing.T, calls []unitCall, id string) []string {
	t.Helper()
	for _, call := range calls {
		if len(call.args) > 1 && call.args[1] == id {
			return call.args
		}
	}
	t.Fatalf("no block call for %s in %+v", id, calls)
	return nil
}

func postsMentioning(posts []string, needle string) []string {
	var out []string
	for _, text := range posts {
		if strings.Contains(text, needle) {
			out = append(out, text)
		}
	}
	return out
}

func TestSweepBlocksUnknownAssignee(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "ready", assignee: "nobody", createdBy: "atlas"},
		{id: "t_2", status: "ready", assignee: "hestia", createdBy: "atlas"},
	}, nil)

	h.run()

	blocks := h.units.subverb("block")
	if len(blocks) != 1 {
		t.Fatalf("block calls = %+v, want one", blocks)
	}
	if blocks[0].agent != "atlas" {
		t.Errorf("block ran as %q, want atlas", blocks[0].agent)
	}
	want := []string{"block", "t_1",
		`assignee "nobody" is not a farm worker; reassign with hermes kanban assign`,
		"--kind", "transient"}
	if !slices.Equal(blocks[0].args, want) {
		t.Errorf("block argv\n got %q\nwant %q", blocks[0].args, want)
	}
	if got := postsMentioning(h.postList(), "t_1"); len(got) != 1 {
		t.Errorf("posts naming t_1 = %q, want one", got)
	}
	if got := postsMentioning(h.postList(), "t_2"); len(got) != 0 {
		t.Errorf("posts naming t_2 = %q, want none", got)
	}
}

func TestSweepReportsStranded(t *testing.T) {
	stale := time.Now().UTC().Add(-45 * time.Minute)
	for _, tc := range []struct {
		name     string
		occupied int
		capacity int
		paused   bool
		want     float64
	}{
		{name: "idle below the cap", occupied: 0, capacity: 1, want: 1},
		{name: "at the cap", occupied: 1, capacity: 1, want: 0},
		{name: "paused", occupied: 0, capacity: 1, paused: true, want: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testRoster(), []card{
				{id: "t_1", status: "ready", assignee: "hestia", createdBy: "atlas", createdAt: stale},
			}, nil)
			h.setCapacity(tc.occupied, tc.capacity, tc.paused)

			h.run()

			if got := testutil.ToFloat64(h.reg.CardsStranded); got != tc.want {
				t.Errorf("cards_stranded = %v, want %v", got, tc.want)
			}
			posts := postsMentioning(h.postList(), "t_1")
			if tc.want == 0 && len(posts) != 0 {
				t.Errorf("posts = %q, want none", posts)
			}
			if tc.want == 1 && len(posts) != 1 {
				t.Errorf("posts = %q, want one naming t_1", posts)
			}
			if got := h.units.subverb("block"); len(got) != 0 {
				t.Errorf("stranded card blocked: %+v", got)
			}
		})
	}
}

func TestSweepIgnoresAFreshReadyCard(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "ready", assignee: "hestia", createdBy: "atlas",
			createdAt: time.Now().UTC().Add(-10 * time.Minute)},
	}, nil)

	h.run()

	if got := testutil.ToFloat64(h.reg.CardsStranded); got != 0 {
		t.Errorf("cards_stranded = %v, want 0", got)
	}
	if got := h.postList(); len(got) != 0 {
		t.Errorf("posts = %q, want none", got)
	}
}

func TestSweepExportsCounts(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "ready", assignee: "hestia", createdBy: "atlas"},
		{id: "t_2", status: "ready", assignee: "argus", createdBy: "atlas"},
		{id: "t_3", status: "done", assignee: "hestia", createdBy: "atlas"},
		{id: "t_4", status: "blocked", assignee: "argus", createdBy: "atlas"},
	}, nil)

	h.run()

	counts, err := h.sweep.Board.Counts(context.Background())
	if err != nil {
		t.Fatalf("counts: %v", err)
	}
	if len(counts) != 3 {
		t.Fatalf("board counts = %v, want three statuses", counts)
	}
	for status, want := range counts {
		if got := gaugeValue(t, h.reg.Cards, status); got != float64(want) {
			t.Errorf("cards{%s} = %v, want %v", status, got, want)
		}
	}
}

func TestSweepBlocksCrossCreatedCard(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "ready", assignee: "argus", createdBy: "hestia"},
		{id: "t_2", status: "todo", assignee: "argus", createdBy: "hestia"},
		{id: "t_3", status: "running", assignee: "argus", createdBy: "hestia"},
		{id: "t_4", status: "done", assignee: "argus", createdBy: "hestia"},
		{id: "t_5", status: "ready", assignee: "hestia", createdBy: "hestia"},
	}, nil)

	h.run()

	blocks := h.units.subverb("block")
	if len(blocks) != 3 {
		t.Fatalf("block calls = %+v, want three", blocks)
	}
	for _, id := range []string{"t_1", "t_2", "t_3"} {
		args := blockArgs(t, blocks, id)
		if !strings.Contains(args[2], "hestia") || !strings.Contains(args[2], "argus") {
			t.Errorf("block reason for %s = %q, wants both agents named", id, args[2])
		}
		if !slices.Equal(args[3:], []string{"--kind", "transient"}) {
			t.Errorf("block %s argv tail = %q, want [--kind transient]", id, args[3:])
		}
		if got := postsMentioning(h.postList(), id); len(got) != 1 {
			t.Errorf("posts naming %s = %q, want one", id, got)
		}
	}
	for _, id := range []string{"t_4", "t_5"} {
		if got := postsMentioning(h.postList(), id); len(got) != 0 {
			t.Errorf("posts naming %s = %q, want none", id, got)
		}
	}
}

func TestSweepBlocksForgedNotifierSub(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "ready", assignee: "hestia", createdBy: "hestia"},
		{id: "t_2", status: "ready", assignee: "hestia", createdBy: "atlas"},
	}, []sub{
		{taskID: "t_1", chatID: "c1", threadID: "th1", notifier: "atlas"},
		{taskID: "t_1", chatID: "c2", chatType: "channel", notifier: "hestia"},
		{taskID: "t_2", chatID: "c3", threadID: "th3", notifier: "atlas"},
	})

	h.run()

	removed := h.units.subverb("notify-unsubscribe")
	if len(removed) != 1 {
		t.Fatalf("notify-unsubscribe calls = %+v, want one", removed)
	}
	if removed[0].agent != "atlas" {
		t.Errorf("notify-unsubscribe ran as %q, want the notifier manager atlas", removed[0].agent)
	}
	want := []string{"notify-unsubscribe", "t_1", "--platform", "discord",
		"--chat-id", "c1", "--thread-id", "th1"}
	if !slices.Equal(removed[0].args, want) {
		t.Errorf("notify-unsubscribe argv\n got %q\nwant %q", removed[0].args, want)
	}
	posts := postsMentioning(h.postList(), "t_1")
	if len(posts) != 1 {
		t.Fatalf("posts naming t_1 = %q, want one", posts)
	}
	for _, needle := range []string{"hestia", "atlas", "th1"} {
		if !strings.Contains(posts[0], needle) {
			t.Errorf("post %q does not name %q", posts[0], needle)
		}
	}
	if got := counterValue(t, h.reg.NotifySubBlocksTotal, "hestia", "atlas"); got != 1 {
		t.Errorf("notify_sub_blocks_total{hestia,atlas} = %v, want 1", got)
	}
}

func TestSweepMissesForeignCardForgery(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "ready", assignee: "hestia", createdBy: "atlas"},
	}, []sub{
		{taskID: "t_1", chatID: "c1", threadID: "th1", notifier: "atlas"},
	})

	h.run()

	if got := h.units.subverb("notify-unsubscribe"); len(got) != 0 {
		t.Errorf("notify-unsubscribe calls = %+v, want none", got)
	}
	if got := h.postList(); len(got) != 0 {
		t.Errorf("posts = %q, want none", got)
	}
}

func TestSweepBlocksDefaultAssigneeWhileRunning(t *testing.T) {
	h := newHarness(t, testRoster(), []card{
		{id: "t_1", status: "running", assignee: "default", createdBy: "hestia", workerPID: 4242},
	}, nil)
	h.mapPID(4242, hestiaUnit)

	h.run()

	stops := h.units.stops()
	if len(stops) != 1 {
		t.Fatalf("run-stop calls = %+v, want one", stops)
	}
	if stops[0].agent != "hestia" || stops[0].args[0] != hestiaUnit {
		t.Errorf("run-stop = %+v, want hestia and %s", stops[0], hestiaUnit)
	}
	blocks := h.units.subverb("block")
	if len(blocks) != 1 {
		t.Fatalf("block calls = %+v, want one", blocks)
	}
	args := blockArgs(t, blocks, "t_1")
	if !strings.Contains(args[2], `"default"`) {
		t.Errorf("block reason = %q, want it to name the default assignee", args[2])
	}
	if !slices.Equal(args[3:], []string{"--kind", "transient"}) {
		t.Errorf("block argv tail = %q, want [--kind transient]", args[3:])
	}
	if got := postsMentioning(h.postList(), "t_1"); len(got) != 1 {
		t.Errorf("posts naming t_1 = %q, want one", got)
	}
}

func TestSweepBlocksARunningCardItCannotStop(t *testing.T) {
	for _, tc := range []struct {
		name string
		pid  int
	}{
		{name: "no worker pid", pid: 0},
		{name: "a pid that names no run unit", pid: 999},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, testRoster(), []card{
				{id: "t_1", status: "running", assignee: "default", createdBy: "hestia", workerPID: tc.pid},
			}, nil)

			h.run()

			if got := h.units.stops(); len(got) != 0 {
				t.Errorf("run-stop calls = %+v, want none", got)
			}
			if got := h.units.subverb("block"); len(got) != 1 {
				t.Fatalf("block calls = %+v, want one", got)
			}
			if got := postsMentioning(h.postList(), "t_1"); len(got) != 1 {
				t.Errorf("posts naming t_1 = %q, want one", got)
			}
		})
	}
}

func writeProfiles(t *testing.T, root string, agents ...string) {
	t.Helper()
	for _, agent := range agents {
		dir := filepath.Join(root, agent, ".hermes", "profiles", agent)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("make %s: %v", dir, err)
		}
		write(t, filepath.Join(dir, "config.yaml"), "agent:\n  name: "+agent+"\n")
		write(t, filepath.Join(dir, "SOUL.md"), "# "+agent+"\n")
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func hashOf(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func writeBaseline(t *testing.T, path, root string, agents ...string) {
	t.Helper()
	profiles := map[string]map[string]string{}
	for _, agent := range agents {
		dir := filepath.Join(root, agent, ".hermes", "profiles", agent)
		profiles[agent] = map[string]string{
			"config": hashOf(t, filepath.Join(dir, "config.yaml")),
			"soul":   hashOf(t, filepath.Join(dir, "SOUL.md")),
		}
	}
	data, err := json.Marshal(map[string]any{"profiles": profiles})
	if err != nil {
		t.Fatalf("marshal baseline: %v", err)
	}
	write(t, path, string(data))
}

func TestSweepDetectsProfileDrift(t *testing.T) {
	h := newHarness(t, testRoster(), nil, nil)

	h.run()

	if got := h.postList(); len(got) != 0 {
		t.Fatalf("unmodified tree posted %q, want nothing", got)
	}
	for _, agent := range []string{"atlas", "hestia", "argus"} {
		if got := gaugeValue(t, h.reg.ProfileDrift, agent); got != 0 {
			t.Errorf("profile_drift{%s} = %v, want 0", agent, got)
		}
	}

	write(t, filepath.Join(h.home, "argus", ".hermes", "profiles", "argus", "config.yaml"),
		"agent:\n  name: argus\n  reasoning_effort: minimal\n")

	h.run()

	posts := postsMentioning(h.postList(), "argus")
	if len(posts) != 1 {
		t.Fatalf("posts naming argus = %q, want one", posts)
	}
	if got := gaugeValue(t, h.reg.ProfileDrift, "argus"); got != 1 {
		t.Errorf("profile_drift{argus} = %v, want 1", got)
	}
	if got := gaugeValue(t, h.reg.ProfileDrift, "hestia"); got != 0 {
		t.Errorf("profile_drift{hestia} = %v, want 0", got)
	}
}

func TestSweepTreatsUnreadableAsDrift(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 directory, so the fail-closed path cannot be exercised")
	}
	h := newHarness(t, testRoster(), nil, nil)
	sealed := filepath.Join(h.home, "argus", ".hermes", "profiles", "argus")
	if err := os.Chmod(sealed, 0o000); err != nil {
		t.Fatalf("chmod %s: %v", sealed, err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(sealed, 0o755); err != nil {
			t.Errorf("restore %s: %v", sealed, err)
		}
	})

	h.run()

	posts := postsMentioning(h.postList(), "argus")
	if len(posts) != 1 {
		t.Fatalf("posts naming argus = %q, want exactly one", posts)
	}
	if got := gaugeValue(t, h.reg.ProfileDrift, "argus"); got != 1 {
		t.Errorf("profile_drift{argus} = %v, want 1", got)
	}
}

func TestSweepSeedsAnUnrecordedProfile(t *testing.T) {
	h := newHarness(t, testRoster(), nil, nil)
	if err := os.Remove(h.baseline); err != nil {
		t.Fatalf("remove baseline: %v", err)
	}

	h.run()

	if got := h.postList(); len(got) != 0 {
		t.Fatalf("first sweep posted %q, want nothing", got)
	}
	data, err := os.ReadFile(h.baseline)
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	var recorded struct {
		Profiles map[string]struct {
			Config string `json:"config"`
			Soul   string `json:"soul"`
		} `json:"profiles"`
	}
	if err := json.Unmarshal(data, &recorded); err != nil {
		t.Fatalf("decode baseline: %v", err)
	}
	if len(recorded.Profiles) != 3 {
		t.Fatalf("baseline records %d agents, want 3", len(recorded.Profiles))
	}
	want := hashOf(t, filepath.Join(h.home, "argus", ".hermes", "profiles", "argus", "config.yaml"))
	if got := recorded.Profiles["argus"].Config; got != want {
		t.Errorf("baseline config digest for argus = %q, want %q", got, want)
	}

	write(t, filepath.Join(h.home, "argus", ".hermes", "profiles", "argus", "SOUL.md"), "# not argus\n")

	h.run()

	if got := postsMentioning(h.postList(), "argus"); len(got) != 1 {
		t.Errorf("posts naming argus = %q, want one after the seeded file changed", got)
	}
}

func TestSweepSkipsDisabledAgents(t *testing.T) {
	h := newHarness(t, testRoster(), nil, nil)

	h.run()

	families, err := h.reg.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, family := range families {
		if family.GetName() != "snowfarm_profile_drift" {
			continue
		}
		for _, metric := range family.GetMetric() {
			for _, label := range metric.GetLabel() {
				if label.GetValue() == "euclid" {
					t.Error("the sweep hashed a disabled agent's profile")
				}
			}
		}
	}
}
