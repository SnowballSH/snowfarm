package dispatch

import (
	"context"
	"database/sql"
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
	"github.com/SnowballSH/snowfarm/internal/journal"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	_ "modernc.org/sqlite"
)

const (
	testInterval   = 30 * time.Second
	testDefaultMax = 2 * time.Hour
	testGrace      = 2*testInterval + stopGrace

	verbRunDispatch    = "run-dispatch"
	verbRunMaintenance = "run-maintenance"
	verbRunStop        = "run-stop"
	verbKanban         = "kanban"
)

type unitCall struct {
	verb    string
	agent   string
	args    []string
	started time.Time
	ended   time.Time
}

type fakeUnits struct {
	mu         sync.Mutex
	calls      []unitCall
	live       map[string][]string
	kanbanFail map[string]int
	delay      time.Duration
	seq        int
}

func newFakeUnits() *fakeUnits {
	return &fakeUnits{live: map[string][]string{}, kanbanFail: map[string]int{}}
}

func (f *fakeUnits) Gateway(_ context.Context, agent, verb string) ([]byte, error) {
	f.record(unitCall{verb: "gateway", agent: agent, args: []string{verb}})
	return nil, nil
}

func (f *fakeUnits) RunDispatch(_ context.Context, agent string) (string, error) {
	return f.pass(agent, verbRunDispatch)
}

func (f *fakeUnits) RunMaintenance(_ context.Context, agent string) (string, error) {
	return f.pass(agent, verbRunMaintenance)
}

func (f *fakeUnits) pass(agent, verb string) (string, error) {
	f.mu.Lock()
	f.seq++
	unit := fmt.Sprintf("snowfarm-run-%s-%d.service", agent, f.seq)
	f.mu.Unlock()
	f.record(unitCall{verb: verb, agent: agent, args: []string{unit}})
	return unit, nil
}

func (f *fakeUnits) RunStop(_ context.Context, agent, unit string) error {
	f.record(unitCall{verb: verbRunStop, agent: agent, args: []string{unit}})
	return nil
}

func (f *fakeUnits) RunList(_ context.Context, agent string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.live[agent]), nil
}

func (f *fakeUnits) Kanban(_ context.Context, agent string, args ...string) ([]byte, error) {
	f.record(unitCall{verb: verbKanban, agent: agent, args: args})
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(args) > 0 && f.kanbanFail[args[0]] > 0 {
		f.kanbanFail[args[0]]--
		return nil, fmt.Errorf("kanban %s: exit status 1", args[0])
	}
	return nil, nil
}

func (f *fakeUnits) failKanban(sub string, times int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.kanbanFail[sub] = times
}

func (f *fakeUnits) record(call unitCall) {
	f.mu.Lock()
	delay := f.delay
	f.mu.Unlock()
	call.started = time.Now()
	time.Sleep(delay)
	call.ended = time.Now()
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
}

func (f *fakeUnits) verbs(verb string) []unitCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []unitCall
	for _, call := range f.calls {
		if call.verb == verb {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeUnits) agents(verb string) []string {
	var out []string
	for _, call := range f.verbs(verb) {
		out = append(out, call.agent)
	}
	return out
}

func (f *fakeUnits) kanban(sub string) []unitCall {
	var out []unitCall
	for _, call := range f.verbs(verbKanban) {
		if len(call.args) > 0 && call.args[0] == sub {
			out = append(out, call)
		}
	}
	return out
}

func (f *fakeUnits) setLive(agent string, units ...string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.live[agent] = units
}

type journalRead struct {
	uid  int
	unit string
}

type fakeJournal struct {
	mu       sync.Mutex
	byUnit   map[string][]journal.Entry
	fallback []journal.Entry
	reads    []journalRead
}

func newFakeJournal() *fakeJournal {
	return &fakeJournal{byUnit: map[string][]journal.Entry{}}
}

func (f *fakeJournal) UserUnit(_ context.Context, uid int, unit string, _ time.Time) ([]journal.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads = append(f.reads, journalRead{uid: uid, unit: unit})
	if entries, ok := f.byUnit[unit]; ok {
		return entries, nil
	}
	return f.fallback, nil
}

func (f *fakeJournal) GatewayLog(context.Context, string, int64) ([]string, int64, error) {
	return nil, 0, errors.New("not used")
}

func (f *fakeJournal) set(unit string, entries ...journal.Entry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.byUnit[unit] = entries
}

func (f *fakeJournal) unitsRead() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for _, read := range f.reads {
		out = append(out, read.unit)
	}
	return out
}

func payload(t *testing.T, name string) []journal.Entry {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return []journal.Entry{
		{Message: "hermes: kanban dispatch starting", Priority: 6},
		{Message: strings.TrimSpace(string(data)), Priority: 6},
	}
}

type post struct {
	channel string
	text    string
}

type card struct {
	id         string
	assignee   string
	startedAt  time.Time
	maxRuntime time.Duration
	workerPID  int
}

type harness struct {
	t       *testing.T
	loop    *Loop
	units   *fakeUnits
	journal *fakeJournal
	ledger  *Ledger
	reg     *metrics.Registry
	path    string

	mu    sync.Mutex
	now   time.Time
	posts []post
}

func testRoster(concurrent int, workers ...string) *roster.Roster {
	r := &roster.Roster{
		Farm: roster.Farm{HomeRoot: "/var/lib/farm", KanbanHome: "/srv/snowfarm/kanban", UIDBase: 6000},
		Guard: roster.GuardConfig{
			MaxConcurrentRuns: concurrent,
			DispatchInterval:  roster.Duration(testInterval),
			DefaultMaxRuntime: roster.Duration(testDefaultMax),
		},
	}
	for _, name := range workers {
		r.Agents = append(r.Agents, roster.Agent{Name: name, Tier: roster.TierWorker, Enabled: true})
	}
	return r
}

func newHarness(t *testing.T, r *roster.Roster, cards ...card) *harness {
	t.Helper()
	dir := t.TempDir()
	h := &harness{
		t:       t,
		units:   newFakeUnits(),
		journal: newFakeJournal(),
		reg:     metrics.New(),
		path:    filepath.Join(dir, "kanban.db"),
		now:     time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC),
	}
	h.writeBoard(cards...)
	reader, err := board.Open(h.path)
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Errorf("close board: %v", err)
		}
	})
	ledger, err := OpenLedger(filepath.Join(dir, "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Errorf("close ledger: %v", err)
		}
	})
	h.ledger = ledger

	h.loop = &Loop{
		Roster:   pointerTo(r),
		UIDs:     pointerTo(uidsOf(r)),
		Board:    reader,
		Units:    h.units,
		Journal:  h.journal,
		Ledger:   ledger,
		Post:     func(_ context.Context, text string) { h.addPost(post{text: text}) },
		PostTo:   func(_ context.Context, channel, text string) { h.addPost(post{channel: channel, text: text}) },
		Metrics:  h.reg,
		Now:      h.clock,
		PassWait: 50 * time.Millisecond,
		PassPoll: 5 * time.Millisecond,
	}
	return h
}

func pointerTo[T any](value *T) *atomic.Pointer[T] {
	var p atomic.Pointer[T]
	p.Store(value)
	return &p
}

func uidsOf(r *roster.Roster) *map[string]int {
	uids := make(map[string]int, len(r.Agents))
	for _, a := range r.Agents {
		uids[a.Name] = r.UID(a)
	}
	return &uids
}

func (h *harness) clock() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.now
}

func (h *harness) advance(d time.Duration) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.now = h.now.Add(d)
}

func (h *harness) addPost(p post) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.posts = append(h.posts, p)
}

func (h *harness) allPosts() []post {
	h.mu.Lock()
	defer h.mu.Unlock()
	return slices.Clone(h.posts)
}

func (h *harness) tick() {
	h.t.Helper()
	if err := h.loop.Tick(context.Background()); err != nil {
		h.t.Fatalf("tick: %v", err)
	}
}

func (h *harness) writeBoard(cards ...card) {
	h.t.Helper()
	fresh := false
	if _, err := os.Stat(h.path); errors.Is(err, os.ErrNotExist) {
		fresh = true
	}
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		h.t.Fatalf("open board writer: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			h.t.Fatalf("close board writer: %v", err)
		}
	}()
	if fresh {
		schema, err := os.ReadFile(filepath.Join("..", "board", "testdata", "schema.sql"))
		if err != nil {
			h.t.Fatalf("read board schema: %v", err)
		}
		if _, err := db.Exec(string(schema)); err != nil {
			h.t.Fatalf("apply board schema: %v", err)
		}
	}
	if _, err := db.Exec(`DELETE FROM tasks`); err != nil {
		h.t.Fatalf("clear tasks: %v", err)
	}
	for _, c := range cards {
		_, err := db.Exec(
			`INSERT INTO tasks (id, title, status, assignee, created_by, created_at,
			 started_at, worker_pid, max_runtime_seconds)
			 VALUES (?, ?, 'running', ?, 'atlas', ?, ?, ?, ?)`,
			c.id, "card "+c.id, c.assignee, c.startedAt.Unix(), c.startedAt.Unix(),
			nullable(c.workerPID), nullable(int(c.maxRuntime/time.Second)))
		if err != nil {
			h.t.Fatalf("insert card %s: %v", c.id, err)
		}
	}
}

func (h *harness) subscribe(taskID, chatID, threadID string) {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		h.t.Fatalf("open board writer: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			h.t.Fatalf("close board writer: %v", err)
		}
	}()
	_, err = db.Exec(
		`INSERT INTO kanban_notify_subs (task_id, platform, chat_id, thread_id, chat_type,
		 notifier_profile, created_at) VALUES (?, 'discord', ?, ?, 'thread', 'atlas', ?)`,
		taskID, chatID, threadID, h.clock().Unix())
	if err != nil {
		h.t.Fatalf("insert subscription: %v", err)
	}
}

func nullable(n int) any {
	if n == 0 {
		return nil
	}
	return n
}

func counterValue(t *testing.T, vec *prometheus.CounterVec, labels ...string) float64 {
	t.Helper()
	counter, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("counter %v: %v", labels, err)
	}
	return testutil.ToFloat64(counter)
}

func gaugeValue(t *testing.T, vec *prometheus.GaugeVec, labels ...string) float64 {
	t.Helper()
	gauge, err := vec.GetMetricWithLabelValues(labels...)
	if err != nil {
		t.Fatalf("gauge %v: %v", labels, err)
	}
	return testutil.ToFloat64(gauge)
}

func TestTickDispatchesIdleWorkers(t *testing.T) {
	r := testRoster(2, "hestia", "argus", "euclid", "daedalus", "hypatia")
	h := newHarness(t, r)
	h.journal.fallback = payload(t, "dispatch-idle.json")
	h.journal.set("snowfarm-run-hestia-1.service", payload(t, "dispatch-spawned.json")...)

	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia", "argus"}) {
		t.Fatalf("first tick dispatched %v, want [hestia argus]", got)
	}
	for _, agent := range []string{"hestia", "argus"} {
		passes, err := h.ledger.PendingPasses(agent)
		if err != nil {
			t.Fatal(err)
		}
		if len(passes) != 1 || passes[0].Kind != PassSpawn {
			t.Fatalf("%s pending passes %+v, want one spawn pass", agent, passes)
		}
	}

	h.advance(testInterval)
	h.tick()

	unit, ok := h.ledger.UnitForTask("t_1")
	if !ok || unit != "snowfarm-run-hestia-1.service" {
		t.Fatalf("UnitForTask(t_1) = %q, %v; want hestia's pass unit", unit, ok)
	}
	if got := counterValue(t, h.reg.RunsStartedTotal, "hestia"); got != 1 {
		t.Fatalf("runs_started_total{hestia} = %v, want 1", got)
	}
}

func TestTickRespectsFarmCap(t *testing.T) {
	r := testRoster(2, "hestia", "argus", "euclid")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r,
		card{id: "t_a", assignee: "hestia", startedAt: started, maxRuntime: time.Hour},
		card{id: "t_b", assignee: "argus", startedAt: started, maxRuntime: time.Hour},
	)
	h.journal.fallback = payload(t, "dispatch-idle.json")

	h.tick()

	if got := h.units.agents(verbRunDispatch); got != nil {
		t.Fatalf("dispatched %v at the cap, want none", got)
	}
	if got := counterValue(t, h.reg.DispatchSkippedTotal, skipFarmCap); got == 0 {
		t.Fatal("dispatch_skipped_total{farm_cap} did not increment")
	}
}

func TestTickCapHoldsWithinOneTick(t *testing.T) {
	r := testRoster(1, "hestia", "argus", "euclid", "daedalus", "hypatia")
	h := newHarness(t, r)
	h.journal.fallback = payload(t, "dispatch-idle.json")
	h.journal.set("snowfarm-run-hestia-1.service", payload(t, "dispatch-spawned.json")...)

	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("first tick dispatched %v, want exactly [hestia]", got)
	}

	h.advance(testInterval)
	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("after the spawn the loop dispatched %v, want no second pass", got)
	}
}

func TestTickReservesForUnresolvedPass(t *testing.T) {
	r := testRoster(1, "hestia", "argus")
	h := newHarness(t, r)
	unit := "snowfarm-run-hestia-1.service"
	h.units.setLive("hestia", unit)

	h.tick()
	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("first tick dispatched %v, want [hestia]", got)
	}

	h.advance(testInterval)
	h.tick()
	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("an unresolved pass did not hold the slot: dispatched %v", got)
	}

	h.journal.set(unit, payload(t, "dispatch-idle.json")...)
	h.advance(testInterval)
	h.tick()
	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia", "argus"}) {
		t.Fatalf("after the pass resolved the loop dispatched %v, want [hestia argus]", got)
	}
}

func TestTickRetainsReservationForLiveNoJSONPass(t *testing.T) {
	r := testRoster(1, "hestia", "argus")
	h := newHarness(t, r)
	unit := "snowfarm-run-hestia-1.service"
	h.units.setLive("hestia", unit)

	h.tick()
	h.advance(testInterval)
	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("a live pass with no JSON released its slot: dispatched %v", got)
	}
	passes, err := h.ledger.PendingPasses("hestia")
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 1 {
		t.Fatalf("pending passes %+v, want the live pass retained", passes)
	}
	if got := counterValue(t, h.reg.DispatchSkippedTotal, skipNoJSON); got != 0 {
		t.Fatalf("dispatch_skipped_total{no_json} = %v for a live pass, want 0", got)
	}

	h.units.setLive("hestia")
	h.advance(testInterval)
	h.tick()

	if got := counterValue(t, h.reg.DispatchSkippedTotal, skipNoJSON); got != 1 {
		t.Fatalf("dispatch_skipped_total{no_json} = %v once the unit exited, want 1", got)
	}
}

func TestTickReleasesReservationOnCrashedPass(t *testing.T) {
	r := testRoster(1, "hestia", "argus")
	h := newHarness(t, r)

	h.tick()
	if got := len(h.units.verbs(verbRunDispatch)); got != 1 {
		t.Fatalf("first tick issued %d spawn passes, want 1", got)
	}

	h.advance(testInterval)
	h.tick()

	passes, err := h.ledger.PendingPasses("hestia")
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 0 {
		t.Fatalf("crashed pass %+v still reserves a slot", passes)
	}
	if got := len(h.units.verbs(verbRunDispatch)); got != 2 {
		t.Fatalf("issued %d spawn passes, want the freed slot taken again", got)
	}
}

func TestTickReissuesCrashedPass(t *testing.T) {
	r := testRoster(2, "hestia", "argus")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.set("snowfarm-run-argus-2.service", payload(t, "dispatch-idle.json")...)

	h.tick()
	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("first tick dispatched %v, want [hestia]", got)
	}

	h.advance(testInterval)
	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia", "hestia"}) {
		t.Fatalf("dispatched %v, want the crashed pass re-issued to hestia", got)
	}
	if got := counterValue(t, h.reg.DispatchSkippedTotal, skipNoJSON); got != 1 {
		t.Fatalf("dispatch_skipped_total{no_json} = %v, want 1", got)
	}
}

func TestTickTreatsLockLossAsIdle(t *testing.T) {
	r := testRoster(1, "hestia", "argus")
	h := newHarness(t, r)
	unit := "snowfarm-run-hestia-1.service"
	h.units.setLive("hestia", unit)
	h.journal.set(unit, payload(t, "dispatch-idle.json")...)

	h.tick()
	h.advance(testInterval)
	h.tick()

	if got := counterValue(t, h.reg.DispatchSkippedTotal, skipNoJSON); got != 0 {
		t.Fatalf("dispatch_skipped_total{no_json} = %v for a lock-loser, want 0", got)
	}
	passes, err := h.ledger.PendingPasses("hestia")
	if err != nil {
		t.Fatal(err)
	}
	if len(passes) != 0 {
		t.Fatalf("an empty-but-valid result left %+v pending", passes)
	}
	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia", "argus"}) {
		t.Fatalf("dispatched %v, want the released slot given to argus", got)
	}
}

func TestTickHousekeepsAboveTheCap(t *testing.T) {
	r := testRoster(1, "argus", "hestia")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")

	h.tick()

	if got := h.units.agents(verbRunMaintenance); !slices.Equal(got, []string{"argus"}) {
		t.Fatalf("maintenance passes %v, want [argus] even at the cap", got)
	}
	if got := h.units.agents(verbRunDispatch); got != nil {
		t.Fatalf("dispatched %v at the cap, want none", got)
	}
}

func TestTickAlwaysHousekeepsBusyWorkers(t *testing.T) {
	r := testRoster(2, "argus", "hestia", "euclid", "daedalus", "hypatia")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r,
		card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour},
		card{id: "t_b", assignee: "hestia", startedAt: started, maxRuntime: time.Hour},
	)
	h.journal.fallback = payload(t, "dispatch-idle.json")

	h.tick()

	got := h.units.agents(verbRunMaintenance)
	slices.Sort(got)
	if !slices.Equal(got, []string{"argus", "hestia"}) {
		t.Fatalf("maintenance passes %v, want both busy workers and no idle one", got)
	}
}

func TestTickHousekeepsWithMaintenance(t *testing.T) {
	r := testRoster(3, "argus", "hestia")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")

	h.tick()

	for _, call := range h.units.verbs(verbRunDispatch) {
		if call.agent == "argus" {
			t.Fatal("a busy worker was housekept with run-dispatch, not run-maintenance")
		}
	}
	if got := h.units.agents(verbRunMaintenance); !slices.Equal(got, []string{"argus"}) {
		t.Fatalf("maintenance passes %v, want [argus]", got)
	}
}

func TestHousekeepingFlagOffSkipsMaintenancePass(t *testing.T) {
	off := false
	r := testRoster(2, "argus", "hestia")
	r.Guard.HousekeepingPasses = &off
	started := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC).Add(-(time.Hour + testGrace + time.Minute))
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	h.units.setLive("argus", "snowfarm-run-argus-7.service")
	if err := h.ledger.RecordSpawn("argus", "snowfarm-run-argus-7.service", "t_a", h.clock()); err != nil {
		t.Fatal(err)
	}

	h.tick()

	if got := h.units.agents(verbRunMaintenance); got != nil {
		t.Fatalf("maintenance passes %v with housekeeping_passes false, want none", got)
	}
	if got := len(h.units.kanban("reclaim")); got != 1 {
		t.Fatalf("the backstop reclaimed %d times, want 1 even with housekeeping off", got)
	}
}

func TestOccupiedExcludesMaintenancePasses(t *testing.T) {
	r := testRoster(3, "argus", "hestia", "euclid")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	earlier := h.clock().Add(-10 * time.Second)
	if err := h.ledger.RecordPass("argus", "snowfarm-run-argus-0.service", PassMaintenance, earlier); err != nil {
		t.Fatal(err)
	}
	h.units.setLive("argus", "snowfarm-run-argus-0.service")
	h.journal.set("snowfarm-run-argus-0.service")

	h.tick()

	got := h.units.agents(verbRunDispatch)
	slices.Sort(got)
	if !slices.Equal(got, []string{"euclid", "hestia"}) {
		t.Fatalf("spawn passes %v, want both idle workers under a cap of 3", got)
	}
}

func TestTickPausesWhenModelgateDown(t *testing.T) {
	r := testRoster(3, "argus", "hestia")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	h.loop.Paused.Store(true)

	h.tick()

	if got := h.units.agents(verbRunDispatch); got != nil {
		t.Fatalf("dispatched %v while paused, want none", got)
	}
	if got := h.units.agents(verbRunMaintenance); !slices.Equal(got, []string{"argus"}) {
		t.Fatalf("maintenance passes %v while paused, want [argus]", got)
	}
}

func TestGuardScopesToEnabled(t *testing.T) {
	r := testRoster(2, "argus", "hestia")
	r.Agents[0].Enabled = false
	h := newHarness(t, r)
	h.journal.fallback = payload(t, "dispatch-idle.json")

	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"hestia"}) {
		t.Fatalf("dispatched %v, want only the enabled worker", got)
	}
}

func TestTickSerialisesPassesWithinATick(t *testing.T) {
	r := testRoster(5, "hestia", "argus", "euclid", "daedalus", "hypatia")
	h := newHarness(t, r)
	h.journal.fallback = payload(t, "dispatch-idle.json")
	h.units.delay = 2 * time.Millisecond

	h.tick()

	calls := h.units.verbs(verbRunDispatch)
	if len(calls) != 5 {
		t.Fatalf("issued %d spawn passes, want 5", len(calls))
	}
	for i := 1; i < len(calls); i++ {
		if calls[i].started.Before(calls[i-1].ended) {
			t.Fatalf("pass %d began before pass %d finished", i, i-1)
		}
	}
	read := h.journal.unitsRead()
	previous := -1
	for i, call := range calls[:len(calls)-1] {
		at := slices.Index(read, call.args[0])
		if at < 0 {
			t.Fatalf("pass %d (%s) was never waited on", i, call.args[0])
		}
		if at <= previous {
			t.Fatalf("pass %d was waited on out of order", i)
		}
		previous = at
	}
}

func TestTickStopsOverrunningRunOnlyAsBackstop(t *testing.T) {
	r := testRoster(2, "argus", "hestia")
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	overrunning := now.Add(-(2*time.Hour + time.Minute))
	within := now.Add(-10 * time.Minute)
	h := newHarness(t, r,
		card{id: "t_over", assignee: "argus", startedAt: overrunning, maxRuntime: 2 * time.Hour},
		card{id: "t_ok", assignee: "hestia", startedAt: within, maxRuntime: 2 * time.Hour},
	)
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-argus-9.service"
	h.units.setLive("argus", unit)
	if err := h.ledger.RecordSpawn("argus", unit, "t_over", h.clock()); err != nil {
		t.Fatal(err)
	}
	h.subscribe("t_over", "chan-1", "thread-1")

	h.tick()

	if got := len(h.units.verbs(verbRunStop)); got != 0 {
		t.Fatalf("%d run stops inside the detection grace, want none", got)
	}

	h.advance(2 * time.Minute)
	h.tick()

	stops := h.units.verbs(verbRunStop)
	if len(stops) != 1 || stops[0].agent != "argus" || stops[0].args[0] != unit {
		t.Fatalf("run stops %+v, want one stop of argus's unit", stops)
	}
	reclaims := h.units.kanban("reclaim")
	if len(reclaims) != 1 || !slices.Equal(reclaims[0].args, []string{"reclaim", "t_over"}) {
		t.Fatalf("reclaims %+v, want one reclaim of t_over", reclaims)
	}
	blocks := h.units.kanban("block")
	if len(blocks) != 1 || !slices.Contains(blocks[0].args, "--kind") || !slices.Contains(blocks[0].args, "transient") {
		t.Fatalf("blocks %+v, want one transient block", blocks)
	}
	if got := counterValue(t, h.reg.RunStopsTotal, "argus", reasonMaxRuntime); got != 1 {
		t.Fatalf("run_stops_total{argus,max_runtime} = %v, want 1", got)
	}
	if got := h.allPosts(); len(got) != 1 || got[0].channel != "thread-1" {
		t.Fatalf("posts %+v, want one to the card's thread", got)
	}
	if _, ok := h.ledger.UnitForTask("t_ok"); ok {
		t.Fatal("a card inside its budget was recorded stopped")
	}
}

func TestTickStopsRunWithNoMaxRuntime(t *testing.T) {
	r := testRoster(2, "argus")
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	started := now.Add(-(testDefaultMax + testGrace - time.Minute))
	h := newHarness(t, r, card{id: "t_open", assignee: "argus", startedAt: started})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-argus-9.service"
	h.units.setLive("argus", unit)
	if err := h.ledger.RecordSpawn("argus", unit, "t_open", h.clock()); err != nil {
		t.Fatal(err)
	}

	h.tick()

	if got := len(h.units.verbs(verbRunStop)); got != 0 {
		t.Fatalf("%d run stops just short of the default bound, want none", got)
	}
	if got := gaugeValue(t, h.reg.CardsWithoutMaxRuntime, "argus"); got != 1 {
		t.Fatalf("cards_without_max_runtime{argus} = %v, want 1", got)
	}
	advisories := h.allPosts()
	if len(advisories) != 1 || !strings.Contains(advisories[0].text, "without a max_runtime") {
		t.Fatalf("posts %+v, want one max_runtime advisory", advisories)
	}

	h.advance(2 * time.Minute)
	h.tick()

	if got := len(h.units.verbs(verbRunStop)); got != 1 {
		t.Fatalf("run stops %d past the default bound, want 1", got)
	}
	if got := len(h.units.kanban("reclaim")); got != 1 {
		t.Fatalf("reclaims %d, want 1", got)
	}
	blocks := h.units.kanban("block")
	if len(blocks) != 1 || !slices.Contains(blocks[0].args, "transient") {
		t.Fatalf("blocks %+v, want one transient block", blocks)
	}
	if got := counterValue(t, h.reg.RunStopsTotal, "argus", reasonNoMaxRuntime); got != 1 {
		t.Fatalf("run_stops_total{argus,no_max_runtime} = %v, want 1", got)
	}
	if got := len(h.allPosts()); got != 2 {
		t.Fatalf("%d posts, want the one advisory plus one stop notice", got)
	}
}

func TestBackstopFallsBackToStatusWhenNoSub(t *testing.T) {
	r := testRoster(2, "argus")
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	started := now.Add(-(time.Hour + testGrace + time.Minute))
	h := newHarness(t, r, card{id: "t_over", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-argus-9.service"
	h.units.setLive("argus", unit)
	if err := h.ledger.RecordSpawn("argus", unit, "t_over", h.clock()); err != nil {
		t.Fatal(err)
	}

	h.tick()

	posts := h.allPosts()
	if len(posts) != 1 || posts[0].channel != "" {
		t.Fatalf("posts %+v, want one on the status channel", posts)
	}
}

func TestBackstopPostsToOriginatingThread(t *testing.T) {
	r := testRoster(2, "argus")
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	started := now.Add(-(time.Hour + testGrace + time.Minute))
	h := newHarness(t, r, card{id: "t_over", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-argus-9.service"
	h.units.setLive("argus", unit)
	if err := h.ledger.RecordSpawn("argus", unit, "t_over", h.clock()); err != nil {
		t.Fatal(err)
	}
	h.subscribe("t_over", "chan-1", "thread-7")

	h.tick()

	posts := h.allPosts()
	if len(posts) != 1 || posts[0].channel != "thread-7" {
		t.Fatalf("posts %+v, want one on thread-7", posts)
	}
	if !strings.Contains(posts[0].text, "t_over") {
		t.Fatalf("post %q does not name the card", posts[0].text)
	}
}

func TestTickClassifiesFinishedRuns(t *testing.T) {
	r := testRoster(2, "argus", "hestia")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r,
		card{id: "t_done", assignee: "argus", startedAt: started, maxRuntime: time.Hour},
		card{id: "t_limit", assignee: "hestia", startedAt: started, maxRuntime: time.Hour},
	)
	h.journal.fallback = payload(t, "dispatch-idle.json")
	limitUnit := "snowfarm-run-hestia-9.service"
	if err := h.ledger.RecordSpawn("hestia", limitUnit, "t_limit", h.clock()); err != nil {
		t.Fatal(err)
	}
	h.journal.set(limitUnit, journal.Entry{Message: "Main process exited, code=exited, status=75/n/a", Priority: 3})
	h.event("t_done", "completed")

	h.tick()
	h.writeBoard()
	h.advance(testInterval)
	h.tick()

	if got := counterValue(t, h.reg.RunsFinishedTotal, "argus", "done"); got != 1 {
		t.Fatalf("runs_finished_total{argus,done} = %v, want 1", got)
	}
	if got := counterValue(t, h.reg.RunsFinishedTotal, "hestia", outcomeRateLimited); got != 1 {
		t.Fatalf("runs_finished_total{hestia,rate_limited} = %v, want 1", got)
	}
	reclaims := h.units.kanban("reclaim")
	if len(reclaims) != 1 || reclaims[0].agent != "hestia" {
		t.Fatalf("reclaims %+v, want the rate-limited card released by hestia", reclaims)
	}
	if got := len(h.units.kanban("block")); got != 0 {
		t.Fatalf("blocked %d cards, want a rate limit to carry no failure", got)
	}
}

func (h *harness) event(taskID, kind string) {
	h.t.Helper()
	db, err := sql.Open("sqlite", h.path)
	if err != nil {
		h.t.Fatalf("open board writer: %v", err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			h.t.Fatalf("close board writer: %v", err)
		}
	}()
	if _, err := db.Exec(
		`INSERT INTO task_events (task_id, kind, created_at) VALUES (?, ?, ?)`,
		taskID, kind, h.clock().Unix()); err != nil {
		h.t.Fatalf("insert event: %v", err)
	}
}

func TestRunningReportsTheLastSnapshot(t *testing.T) {
	r := testRoster(2, "argus")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_a", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")

	if got := h.loop.Running(); got != nil {
		t.Fatalf("Running() before the first tick = %+v, want nil", got)
	}

	h.tick()

	got := h.loop.Running()
	if len(got) != 1 || got[0].ID != "t_a" {
		t.Fatalf("Running() = %+v, want the one running card", got)
	}
	if value := testutil.ToFloat64(h.reg.RunsInFlight); value != 1 {
		t.Fatalf("runs_in_flight = %v, want 1", value)
	}
}

func TestTickReadsEachPassAsItsOwnAgent(t *testing.T) {
	r := testRoster(1, "hestia")
	h := newHarness(t, r)
	h.journal.fallback = payload(t, "dispatch-idle.json")

	h.tick()

	h.journal.mu.Lock()
	defer h.journal.mu.Unlock()
	if len(h.journal.reads) == 0 {
		t.Fatal("no journal read for the issued pass")
	}
	agent, _ := r.Agent("hestia")
	if got := h.journal.reads[0].uid; got != r.UID(agent) {
		t.Fatalf("journal read as uid %d, want hestia's %d", got, r.UID(agent))
	}
}

func TestTickCountsOnlyNewSpawnsAgainstTheCap(t *testing.T) {
	r := testRoster(2, "hestia", "argus")
	started := time.Date(2026, 9, 7, 11, 55, 0, 0, time.UTC)
	h := newHarness(t, r, card{id: "t_1", assignee: "hestia", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-hestia-0.service"
	h.journal.set(unit, payload(t, "dispatch-spawned.json")...)
	if err := h.ledger.RecordPass("hestia", unit, PassSpawn, h.clock().Add(-10*time.Second)); err != nil {
		t.Fatal(err)
	}

	h.tick()

	if got := h.units.agents(verbRunDispatch); !slices.Equal(got, []string{"argus"}) {
		t.Fatalf("dispatched %v; a spawn the running set already holds took a second slot", got)
	}
}

func TestBackstopStopsEachRunOnce(t *testing.T) {
	r := testRoster(2, "argus")
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	started := now.Add(-(time.Hour + testGrace + time.Minute))
	h := newHarness(t, r, card{id: "t_over", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-argus-9.service"
	h.units.setLive("argus", unit)
	if err := h.ledger.RecordSpawn("argus", unit, "t_over", h.clock()); err != nil {
		t.Fatal(err)
	}

	h.tick()
	h.advance(testInterval)
	h.tick()

	if got := len(h.units.kanban("reclaim")); got != 1 {
		t.Fatalf("%d reclaims of a card that stayed running, want 1", got)
	}
	if got := len(h.allPosts()); got != 1 {
		t.Fatalf("%d posts for one stopped run, want 1", got)
	}
}

func TestBackstopRetriesAStopThatDidNotLand(t *testing.T) {
	r := testRoster(2, "argus")
	now := time.Date(2026, 9, 7, 12, 0, 0, 0, time.UTC)
	started := now.Add(-(time.Hour + testGrace + time.Minute))
	h := newHarness(t, r, card{id: "t_over", assignee: "argus", startedAt: started, maxRuntime: time.Hour})
	h.journal.fallback = payload(t, "dispatch-idle.json")
	unit := "snowfarm-run-argus-9.service"
	h.units.setLive("argus", unit)
	if err := h.ledger.RecordSpawn("argus", unit, "t_over", h.clock()); err != nil {
		t.Fatal(err)
	}
	h.units.failKanban("reclaim", 1)

	if err := h.loop.Tick(context.Background()); err == nil {
		t.Fatal("a tick whose reclaim failed reported no error")
	}
	if stopped, err := h.ledger.Stopped("t_over", unit); err != nil || stopped {
		t.Fatalf("Stopped after a failed reclaim = %v, %v; want false, nil", stopped, err)
	}
	if got := counterValue(t, h.reg.RunStopsTotal, "argus", reasonMaxRuntime); got != 0 {
		t.Fatalf("run_stops_total{argus,max_runtime} = %v after a failed reclaim, want 0", got)
	}

	h.advance(testInterval)
	h.tick()

	if got := len(h.units.kanban("reclaim")); got != 2 {
		t.Fatalf("%d reclaims, want the failed one retried exactly once", got)
	}
	if got := counterValue(t, h.reg.RunStopsTotal, "argus", reasonMaxRuntime); got != 1 {
		t.Fatalf("run_stops_total{argus,max_runtime} = %v after the retry landed, want 1", got)
	}

	h.advance(testInterval)
	h.tick()

	if got := len(h.units.kanban("reclaim")); got != 2 {
		t.Fatalf("%d reclaims, want no further retry once the stop landed", got)
	}
	posts := h.allPosts()
	if len(posts) != 2 {
		t.Fatalf("posts %+v, want one retry advisory and one stop notice", posts)
	}
	if !strings.Contains(posts[0].text, "could not reclaim and block it") {
		t.Fatalf("first post %q, want the failed-stop advisory", posts[0].text)
	}
	if !strings.Contains(posts[1].text, "reclaimed and blocked (transient)") {
		t.Fatalf("second post %q, want the stop notice", posts[1].text)
	}
}
