package guard

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/discord"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/journal"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/unitctl"

	_ "modernc.org/sqlite"
)

const (
	rosterFixture = "../roster/testdata/farm.yaml"
	boardSchema   = "../board/testdata/schema.sql"

	testGuildID       = "100000000000000001"
	testOperatorID    = "100000000000000002"
	testSupervisorApp = "100000000000000003"
	testAtlasApp      = "200000000000000001"
	testIrisApp       = "200000000000000002"
	testStranger      = "900000000000000009"
)

// testRoster is the committed fixture with its homes moved under the test's
// own directory, so a gateway log the test writes is the one the guard reads.
func testRoster(t *testing.T, home string) *roster.Roster {
	t.Helper()
	r, err := roster.Load(rosterFixture)
	if err != nil {
		t.Fatalf("load roster: %v", err)
	}
	r.Farm.HomeRoot = home
	return r
}

func held(r *roster.Roster) *atomic.Pointer[roster.Roster] {
	var p atomic.Pointer[roster.Roster]
	p.Store(r)
	return &p
}

func openLedger(t *testing.T) *dispatch.Ledger {
	t.Helper()
	ledger, err := dispatch.OpenLedger(filepath.Join(t.TempDir(), "ledger.db"))
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	t.Cleanup(func() {
		if err := ledger.Close(); err != nil {
			t.Fatalf("close ledger: %v", err)
		}
	})
	return ledger
}

// appendLog writes lines to an agent's gateway log where the breaker looks
// for it, creating the directories apply would have made.
func appendLog(t *testing.T, r *roster.Roster, name string, lines ...string) {
	t.Helper()
	agent, ok := r.Agent(name)
	if !ok {
		t.Fatalf("no agent %s in the fixture", name)
	}
	path := gatewayLogPath(r, agent)
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		t.Fatalf("make log dir: %v", err)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		t.Fatalf("open %s: %v", path, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
	}()
	for _, line := range lines {
		if _, err := fmt.Fprintln(file, line); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
	}
}

// watch is the breaker's first pass over the logs as they already stand: it
// records where each one ends and acts on nothing, which is what keeps a
// guard starting on a host that has been running for months from reading the
// whole history as a rate. Every test that means to exercise a rate arms the
// breaker first and then writes the lines it is about.
func watch(t *testing.T, breaker *Breaker, env *breakerEnv) {
	t.Helper()
	r := env.roster.Load()
	for _, manager := range r.EnabledManagers() {
		appendLog(t, r, manager.Name)
	}
	if err := breaker.Tick(context.Background()); err != nil {
		t.Fatalf("first pass: %v", err)
	}
}

// rewriteLog replaces an agent's gateway log where it stands, which is what a
// copytruncate rotation does to it: the file keeps its name and loses what the
// reader had already accounted for.
func rewriteLog(t *testing.T, r *roster.Roster, name string, lines ...string) {
	t.Helper()
	agent, ok := r.Agent(name)
	if !ok {
		t.Fatalf("no agent %s in the fixture", name)
	}
	if err := os.WriteFile(gatewayLogPath(r, agent), []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatalf("rewrite %s's log: %v", name, err)
	}
}

func turnLine(user string) string {
	return "2026-09-07T04:00:00Z INFO discord adapter handling message from user " + user + " in channel 500"
}

func completeLine(user string) string {
	return "2026-09-07T04:00:00Z INFO discord adapter finished handling message from user " + user + " in channel 500"
}

func mentionLine(user string) string {
	return "2026-09-07T04:00:00Z INFO discord adapter posted reply <@" + user + "> please take a look"
}

// noiseLine is an ordinary log line no pattern matches, which is what most of
// a gateway log is.
func noiseLine() string {
	return "2026-09-07T04:00:00Z INFO discord adapter idle, waiting for the next message on the gateway"
}

func burstLine() string {
	return "2026-09-07T04:00:00Z WARN discord rest request failed: HTTP 429 too many requests"
}

func tripLine() string {
	return "2026-09-07T04:00:00Z ERROR discord adapter circuit breaker opened after 5 consecutive failures"
}

func repeat(line string, n int) []string {
	lines := make([]string, 0, n)
	for range n {
		lines = append(lines, line)
	}
	return lines
}

// clock is a test's hand on the guard's time.
type clock struct {
	mu sync.Mutex
	at time.Time
}

func newClock(at time.Time) *clock { return &clock{at: at} }

func (c *clock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.at = c.at.Add(d)
}

// recorder collects the guard's posts.
type recorder struct {
	mu    sync.Mutex
	posts []string
}

func (p *recorder) post(_ context.Context, text string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.posts = append(p.posts, text)
}

func (p *recorder) all() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return slices.Clone(p.posts)
}

func (p *recorder) matching(substr string) []string {
	var out []string
	for _, text := range p.all() {
		if strings.Contains(text, substr) {
			out = append(out, text)
		}
	}
	return out
}

// fakeUnits answers for farm-unitctl and records every call it was given.
type fakeUnits struct {
	mu       sync.Mutex
	calls    []string
	units    map[string][]string
	show     map[string]string
	inactive map[string]bool
	fail     map[string]error
	seq      int
}

var _ unitctl.Controller = (*fakeUnits)(nil)

func newFakeUnits() *fakeUnits {
	return &fakeUnits{
		units:    map[string][]string{},
		show:     map[string]string{},
		inactive: map[string]bool{},
		fail:     map[string]error{},
	}
}

func (f *fakeUnits) record(format string, args ...any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, fmt.Sprintf(format, args...))
}

func (f *fakeUnits) made() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.calls)
}

func (f *fakeUnits) count(call string) int {
	return len(slices.DeleteFunc(f.made(), func(made string) bool { return made != call }))
}

func (f *fakeUnits) Gateway(_ context.Context, agent, verb string) ([]byte, error) {
	f.record("gateway %s %s", agent, verb)
	if err := f.fail["gateway "+agent]; err != nil {
		return nil, err
	}
	switch verb {
	case "is-active":
		f.mu.Lock()
		down := f.inactive[agent]
		f.mu.Unlock()
		if down {
			return []byte("inactive\n"), fmt.Errorf("exit status 3")
		}
		return []byte("active\n"), nil
	case "show":
		f.mu.Lock()
		defer f.mu.Unlock()
		if out, ok := f.show[agent]; ok {
			return []byte(out), nil
		}
		return []byte("LoadState=loaded\nUnitFileState=enabled\nEnvironment=DISCORD_HOME_CHANNEL=400000000000000001\n"), nil
	default:
		f.mu.Lock()
		f.inactive[agent] = verb == "stop"
		f.mu.Unlock()
		return nil, nil
	}
}

func (f *fakeUnits) RunDispatch(_ context.Context, agent string) (string, error) {
	f.record("run-dispatch %s", agent)
	return f.unit(agent), nil
}

func (f *fakeUnits) RunMaintenance(_ context.Context, agent string) (string, error) {
	f.record("run-maintenance %s", agent)
	return f.unit(agent), nil
}

func (f *fakeUnits) unit(agent string) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.seq++
	return fmt.Sprintf("snowfarm-run-%s-%d.service", agent, f.seq)
}

func (f *fakeUnits) RunStop(_ context.Context, agent, unit string) error {
	f.record("run-stop %s %s", agent, unit)
	return nil
}

func (f *fakeUnits) RunList(_ context.Context, agent string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.units[agent]), nil
}

func (f *fakeUnits) Kanban(_ context.Context, agent string, args ...string) ([]byte, error) {
	f.record("kanban %s %s", agent, strings.Join(args, " "))
	return nil, nil
}

// fakeJournal tails real files for the gateway log — the production reader
// does no more than that — and answers for the run journal from a script.
type fakeJournal struct {
	journal.Reader
	mu      sync.Mutex
	entries []journal.Entry
}

func newFakeJournal(entries ...journal.Entry) *fakeJournal {
	return &fakeJournal{Reader: journal.NewExec(""), entries: entries}
}

func (f *fakeJournal) UserUnit(context.Context, int, string, time.Time) ([]journal.Entry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.entries), nil
}

// dispatchResult is the line a pass prints when it claimed nothing, which
// resolves the pass at once.
func dispatchResult() journal.Entry {
	return journal.Entry{Time: time.Now().UTC(), Message: `{"spawned": [], "skipped_nonspawnable": []}`}
}

var _ discord.Client = (*fakeClient)(nil)

// fakeClient is a guild in memory, plus the session budget the breaker asks
// about and a stream the test feeds events into.
type fakeClient struct {
	mu       sync.Mutex
	roles    []discord.Role
	channels []discord.Channel
	members  map[string][]string
	sends    []sentMessage
	limits   map[string]discord.SessionStartLimit
	events   chan discord.Event
	next     int
}

type sentMessage struct {
	channelID string
	content   string
}

func newFakeClient() *fakeClient {
	return &fakeClient{
		roles: []discord.Role{
			{ID: testGuildID, Name: "@everyone", Position: 0},
			{ID: "300000000000000001", Name: discord.RoleSupervisor, Position: 2, Permissions: discord.SupervisorPermissions},
		},
		members: map[string][]string{
			testSupervisorApp: {"300000000000000001"},
			testAtlasApp:      {},
			testIrisApp:       {},
		},
		limits: map[string]discord.SessionStartLimit{},
		events: make(chan discord.Event, 16),
	}
}

func (f *fakeClient) id() string {
	f.next++
	return fmt.Sprintf("40000000000000%04d", f.next)
}

func (f *fakeClient) Guild(context.Context) (discord.Guild, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return discord.Guild{ID: testGuildID, Roles: slices.Clone(f.roles), Channels: slices.Clone(f.channels)}, nil
}

func (f *fakeClient) CreateRole(_ context.Context, name string, perms uint64, _ string) (discord.Role, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	role := discord.Role{ID: f.id(), Name: name, Position: 1, Permissions: perms}
	f.roles = append(f.roles, role)
	return role, nil
}

func (f *fakeClient) EditRole(context.Context, string, uint64, string) error { return nil }

func (f *fakeClient) PositionRoles(_ context.Context, ids []string, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for i, id := range ids {
		for j := range f.roles {
			if f.roles[j].ID == id {
				f.roles[j].Position = len(ids) - i
			}
		}
	}
	return nil
}

func (f *fakeClient) AddMemberRole(_ context.Context, userID, roleID, _ string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.members[userID] = append(f.members[userID], roleID)
	return nil
}

func (f *fakeClient) GuildMemberRoles(_ context.Context, userID string) ([]string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	held, ok := f.members[userID]
	if !ok {
		return nil, fmt.Errorf("no member %s", userID)
	}
	return slices.Clone(held), nil
}

func (f *fakeClient) EditEveryonePermissions(context.Context, uint64, string) error { return nil }

func (f *fakeClient) CreateChannel(_ context.Context, c discord.ChannelSpec, _ string) (discord.Channel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	channel := discord.Channel{
		ID: f.id(), Name: c.Name, Type: c.Type, ParentID: c.ParentID,
		Position: len(f.channels), Overwrites: slices.Clone(c.Overwrites),
	}
	f.channels = append(f.channels, channel)
	return channel, nil
}

func (f *fakeClient) EditChannel(context.Context, string, discord.ChannelSpec, string) error {
	return nil
}

func (f *fakeClient) Send(_ context.Context, channelID, content string, _ bool) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sends = append(f.sends, sentMessage{channelID: channelID, content: content})
	return f.id(), nil
}

func (f *fakeClient) Messages(context.Context, string, string, int) ([]discord.Message, error) {
	return nil, nil
}

func (f *fakeClient) GatewayBot(_ context.Context, botToken string) (discord.SessionStartLimit, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	limit, ok := f.limits[botToken]
	if !ok {
		return discord.SessionStartLimit{}, fmt.Errorf("no session budget is scripted for that token")
	}
	return limit, nil
}

func (f *fakeClient) Events(ctx context.Context) (<-chan discord.Event, error) {
	go func() {
		<-ctx.Done()
	}()
	return f.events, nil
}

func (f *fakeClient) sent() []sentMessage {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.sends)
}

// writeBoard builds a Kanban database from Hermes' own schema with the cards
// a test needs running.
func writeBoard(t *testing.T, cards ...board.RunningCard) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), boardFile)
	writeBoardAt(t, path, cards...)
	return path
}

func writeBoardAt(t *testing.T, path string, cards ...board.RunningCard) {
	t.Helper()
	schema, err := os.ReadFile(boardSchema)
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
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
	for _, card := range cards {
		if _, err := db.Exec(
			`INSERT INTO tasks (id, title, status, assignee, created_by, created_at,
			 started_at, worker_pid, max_runtime_seconds)
			 VALUES (?, ?, 'running', ?, 'atlas', ?, ?, ?, ?)`,
			card.ID, "a card", card.Assignee, card.StartedAt.UTC().Unix(),
			card.StartedAt.UTC().Unix(), card.WorkerPID, int(card.MaxRuntime/time.Second)); err != nil {
			t.Fatalf("insert %s: %v", card.ID, err)
		}
	}
}

func openBoard(t *testing.T, cards ...board.RunningCard) *board.Reader {
	t.Helper()
	reader, err := board.Open(writeBoard(t, cards...))
	if err != nil {
		t.Fatalf("open board: %v", err)
	}
	t.Cleanup(func() {
		if err := reader.Close(); err != nil {
			t.Fatalf("close board: %v", err)
		}
	})
	return reader
}
