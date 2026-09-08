package guard

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/SnowballSH/snowfarm/internal/claude"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

type claudeEnv struct {
	roster *atomic.Pointer[roster.Roster]
	dir    string
	ledger *dispatch.Ledger
	posts  *recorder
	reg    *metrics.Registry
	clock  *clock
}

// newClaudeTail builds the tailer over a Claude tree laid out as apply makes
// it: shared/ and runs/ under the roster's claude_dir, with no marker and no
// run log until a test writes one.
func newClaudeTail(t *testing.T) (*ClaudeTail, *claudeEnv) {
	t.Helper()
	r := testRoster(t, t.TempDir())
	r.Farm.ClaudeDir = t.TempDir()
	for _, sub := range []string{"shared", "runs"} {
		if err := os.MkdirAll(filepath.Join(r.Farm.ClaudeDir, sub), 0o750); err != nil {
			t.Fatalf("make %s: %v", sub, err)
		}
	}
	env := &claudeEnv{
		roster: held(r),
		dir:    r.Farm.ClaudeDir,
		ledger: openLedger(t),
		posts:  &recorder{},
		reg:    metrics.New(),
		clock:  newClock(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)),
	}
	tail := &ClaudeTail{
		Roster:  env.roster,
		Ledger:  env.ledger,
		Post:    env.posts.post,
		Metrics: env.reg,
		Now:     env.clock.now,
		Log:     slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	}
	return tail, env
}

func (e *claudeEnv) tick(t *testing.T, tail *ClaudeTail) {
	t.Helper()
	if err := tail.Tick(context.Background()); err != nil {
		t.Fatalf("tail pass: %v", err)
	}
}

func (e *claudeEnv) markerPath() string { return claude.Config{Dir: e.dir}.MarkerPath() }

func (e *claudeEnv) runsPath(agent string) string {
	return claude.Config{Dir: e.dir, Agent: agent}.RunsPath()
}

// marker is the window the wrapper would read, through the wrapper's own
// reader.
func (e *claudeEnv) marker(t *testing.T) (time.Time, bool) {
	t.Helper()
	reset, active, err := claude.ReadMarker(e.markerPath(), e.clock.now())
	if err != nil {
		t.Fatalf("read marker: %v", err)
	}
	return reset, active
}

func (e *claudeEnv) limitPosts() []string { return e.posts.matching("Claude Code limit hit") }

// appendRuns writes lines to an agent's own run log the way farm-claude does,
// creating the file apply would have installed.
func (e *claudeEnv) appendRuns(t *testing.T, agent string, records ...claude.Record) {
	t.Helper()
	file, err := os.OpenFile(e.runsPath(agent), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatalf("open %s's run log: %v", agent, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Fatalf("close %s's run log: %v", agent, err)
		}
	}()
	for _, record := range records {
		line, err := json.Marshal(record)
		if err != nil {
			t.Fatalf("marshal record: %v", err)
		}
		if _, err := file.Write(append(line, '\n')); err != nil {
			t.Fatalf("write %s's run log: %v", agent, err)
		}
	}
}

func (e *claudeEnv) appendRaw(t *testing.T, agent, text string) {
	t.Helper()
	file, err := os.OpenFile(e.runsPath(agent), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatalf("open %s's run log: %v", agent, err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Fatalf("close %s's run log: %v", agent, err)
		}
	}()
	if _, err := file.WriteString(text); err != nil {
		t.Fatalf("write %s's run log: %v", agent, err)
	}
}

func okRun(agent, model string, duration time.Duration) claude.Record {
	return claude.Record{
		TS:         "2026-09-20T11:00:00Z",
		Agent:      agent,
		Model:      model,
		Effort:     "xhigh",
		NumTurns:   4,
		DurationMS: duration.Milliseconds(),
	}
}

func limitRun(agent, kind string, reset time.Time) claude.Record {
	record := okRun(agent, claude.DefaultModel, 30*time.Second)
	record.IsError = true
	record.LimitKind = kind
	record.ResetAt = reset.Format(time.RFC3339)
	return record
}

func runs(t *testing.T, env *claudeEnv, agent, model string) float64 {
	t.Helper()
	return testutil.ToFloat64(env.reg.ClaudeRunsTotal.WithLabelValues(agent, model))
}

func limitHits(t *testing.T, env *claudeEnv, kind string) float64 {
	t.Helper()
	return testutil.ToFloat64(env.reg.ClaudeLimitHitsTotal.WithLabelValues(kind))
}

func runSeconds(t *testing.T, env *claudeEnv, agent string) float64 {
	t.Helper()
	return testutil.ToFloat64(env.reg.ClaudeRunSecondsTotal.WithLabelValues(agent))
}

// The wrapper reads the marker and can never write it: shared/ is not
// agent-writable, so a limit only becomes farm-wide when the guard reads the
// line that reported it. A second, nearer limit must not shorten the window
// the farm is already holding.
func TestTailWritesLimitMarker(t *testing.T) {
	tail, env := newClaudeTail(t)
	now := env.clock.now()
	reset := now.Add(2 * time.Hour)
	env.appendRuns(t, "hestia", limitRun("hestia", "weekly", reset))

	env.tick(t, tail)

	window, active := env.marker(t)
	if !active || !window.Equal(reset) {
		t.Fatalf("the marker holds %s (active %t), want %s", window, active, reset)
	}
	posts := env.limitPosts()
	if len(posts) != 1 || !strings.Contains(posts[0], "(weekly) by hestia") {
		t.Fatalf("posts %v, want one naming hestia's weekly limit", posts)
	}
	info, err := os.Stat(env.markerPath())
	if err != nil {
		t.Fatalf("stat marker: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o640 {
		t.Fatalf("the marker is %v: the guard's group is farm-agents, so 0640 is what every agent can read and none can write", perm)
	}
	shared, err := os.ReadDir(filepath.Dir(env.markerPath()))
	if err != nil {
		t.Fatalf("read shared: %v", err)
	}
	var names []string
	for _, entry := range shared {
		names = append(names, entry.Name())
	}
	if !slices.Equal(names, []string{"limit-until"}) {
		t.Fatalf("shared/ holds %v: a marker is written by rename, so a partial file is never visible under that name", names)
	}

	env.appendRuns(t, "hestia", limitRun("hestia", "session", now.Add(time.Hour)))
	env.tick(t, tail)

	if window, _ := env.marker(t); !window.Equal(reset) {
		t.Fatalf("the marker moved to %s, want the further window %s it already held", window, reset)
	}
	if posts := env.limitPosts(); len(posts) != 1 {
		t.Fatalf("posts %v, want the one post the first hit of the window makes", posts)
	}

	further := now.Add(3 * time.Hour)
	env.appendRuns(t, "hestia", limitRun("hestia", "weekly", further))
	env.tick(t, tail)

	if window, _ := env.marker(t); !window.Equal(further) {
		t.Fatalf("the marker holds %s, want the further window %s the farm is now out of", window, further)
	}
	if posts := env.limitPosts(); len(posts) != 1 {
		t.Fatalf("posts %v, want one: only the first hit of a window is news", posts)
	}
}

// The wrapper cannot unlink in a directory it cannot write, so a marker the
// guard does not remove outlives its limit and every run exits 4 until a
// human notices.
func TestTailClearsExpiredMarker(t *testing.T) {
	tail, env := newClaudeTail(t)
	write(t, env.markerPath(), env.clock.now().Add(-time.Minute).Format(time.RFC3339)+"\n")

	env.tick(t, tail)

	if _, err := os.Stat(env.markerPath()); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat marker: %v, want it removed", err)
	}
	if _, active := env.marker(t); active {
		t.Fatal("the wrapper still reads a limit window, so the next run would be refused")
	}
	if posts := env.posts.all(); len(posts) != 0 {
		t.Fatalf("posts %v, want none: a window ending is not a limit hit", posts)
	}
}

func TestTailCountsRuns(t *testing.T) {
	tail, env := newClaudeTail(t)
	reset := env.clock.now().Add(90 * time.Minute)
	env.appendRuns(t, "hestia",
		okRun("hestia", claude.DefaultModel, 12*time.Second),
		okRun("hestia", claude.DefaultModel, 3*time.Second))
	env.appendRuns(t, "argus", limitRun("argus", "session", reset))

	env.tick(t, tail)

	if got := runs(t, env, "hestia", claude.DefaultModel); got != 2 {
		t.Errorf("claude_runs_total{hestia,%s} = %v, want 2", claude.DefaultModel, got)
	}
	if got := limitHits(t, env, "session"); got != 1 {
		t.Errorf("claude_limit_hits_total{session} = %v, want 1", got)
	}
	if got := runSeconds(t, env, "hestia"); got != 15 {
		t.Errorf("claude_run_seconds_total{hestia} = %v, want 15", got)
	}
	posts := env.limitPosts()
	if len(posts) != 1 || !strings.Contains(posts[0], "(session) by argus; resets "+reset.Format(time.RFC3339)) {
		t.Fatalf("posts %v, want one naming argus's session limit and its reset", posts)
	}

	offset, known, err := env.ledger.Offset(claudeOffsetKey("hestia"))
	if err != nil {
		t.Fatalf("read offset: %v", err)
	}
	info, err := os.Stat(env.runsPath("hestia"))
	if err != nil {
		t.Fatalf("stat run log: %v", err)
	}
	if !known || offset != info.Size() {
		t.Fatalf("hestia's offset is %d (known %t), want the log's %d bytes", offset, known, info.Size())
	}

	env.tick(t, tail)

	if got := runs(t, env, "hestia", claude.DefaultModel); got != 2 {
		t.Errorf("a pass with no new lines counted %v runs for hestia, want 2", got)
	}
	if got := limitHits(t, env, "session"); got != 1 {
		t.Errorf("a pass with no new lines counted %v session limits, want 1", got)
	}
	if got := runSeconds(t, env, "hestia"); got != 15 {
		t.Errorf("a pass with no new lines counted %v seconds for hestia, want 15", got)
	}
	if posts := env.limitPosts(); len(posts) != 1 {
		t.Errorf("posts %v, want the one post the first pass made", posts)
	}
}

// The kind is a label value on a family A5's alert rules and dashboards name
// by their lowercase spelling, and Claude Code's own message capitalises two
// of the four.
func TestTailLowercasesTheLimitKind(t *testing.T) {
	tail, env := newClaudeTail(t)
	env.appendRuns(t, "hestia", limitRun("hestia", "Opus", env.clock.now().Add(time.Hour)))

	env.tick(t, tail)

	if got := limitHits(t, env, "opus"); got != 1 {
		t.Errorf("claude_limit_hits_total{opus} = %v, want 1", got)
	}
	if got := limitHits(t, env, "Opus"); got != 0 {
		t.Errorf("claude_limit_hits_total{Opus} = %v, want 0", got)
	}
	posts := env.limitPosts()
	if len(posts) != 1 || !strings.Contains(posts[0], "(opus) by hestia") {
		t.Fatalf("posts %v, want one naming hestia's opus limit", posts)
	}
}

// A run refused by the marker never reached Claude Code: counting it would
// report a run the farm did not make and a limit it did not hit, once per
// attempt, for as long as the window lasts.
func TestTailIgnoresMarkerRefusals(t *testing.T) {
	tail, env := newClaudeTail(t)
	refused := limitRun("hestia", claude.KindMarker, env.clock.now().Add(time.Hour))
	refused.DurationMS = 0
	env.appendRuns(t, "hestia", refused)

	env.tick(t, tail)

	if got := runs(t, env, "hestia", claude.DefaultModel); got != 0 {
		t.Errorf("claude_runs_total{hestia} = %v, want 0", got)
	}
	if got := limitHits(t, env, claude.KindMarker); got != 0 {
		t.Errorf("claude_limit_hits_total{marker} = %v, want 0", got)
	}
	if _, active := env.marker(t); active {
		t.Error("a refusal wrote the marker it was refused by")
	}
	if posts := env.posts.all(); len(posts) != 0 {
		t.Errorf("posts %v, want none", posts)
	}
}

// The run log an agent owns is the one path under the Claude tree it may
// write, so the name inside a line is a claim and the file it was read from
// is a fact.
func TestTailCountsUnderTheLogItReadFrom(t *testing.T) {
	tail, env := newClaudeTail(t)
	env.appendRuns(t, "hestia", okRun("argus", "claude-opus-5", time.Second))

	env.tick(t, tail)

	if got := runs(t, env, "hestia", "claude-opus-5"); got != 1 {
		t.Errorf("claude_runs_total{hestia,claude-opus-5} = %v, want 1", got)
	}
	if got := runs(t, env, "argus", "claude-opus-5"); got != 0 {
		t.Errorf("claude_runs_total{argus,claude-opus-5} = %v, want 0: hestia's log cannot spend argus's subscription", got)
	}
}

// A line still being written is not a line yet: consuming it would drop the
// run it describes, because the offset would move past bytes nothing counted.
func TestTailLeavesAPartialLine(t *testing.T) {
	tail, env := newClaudeTail(t)
	env.appendRuns(t, "hestia", okRun("hestia", claude.DefaultModel, time.Second))
	env.appendRaw(t, "hestia", `{"agent":"hestia","model":"claude-opus-5"`)

	env.tick(t, tail)

	if got := runs(t, env, "hestia", claude.DefaultModel); got != 1 {
		t.Fatalf("claude_runs_total{hestia,%s} = %v, want 1", claude.DefaultModel, got)
	}
	if got := runs(t, env, "hestia", "claude-opus-5"); got != 0 {
		t.Fatalf("a half-written line was counted as a run")
	}

	env.appendRaw(t, "hestia", ",\"duration_ms\":1000}\n")
	env.tick(t, tail)

	if got := runs(t, env, "hestia", "claude-opus-5"); got != 1 {
		t.Fatalf("claude_runs_total{hestia,claude-opus-5} = %v, want 1 once the line was finished", got)
	}
}

// A line that is not a run record is skipped rather than taking the pass
// down: one unreadable line must not stop the farm counting the runs after
// it, or freeze the offset and re-read them forever.
func TestTailSkipsAnUnreadableLine(t *testing.T) {
	tail, env := newClaudeTail(t)
	env.appendRaw(t, "hestia", "not a run record\n")
	env.appendRuns(t, "hestia", okRun("hestia", claude.DefaultModel, 2*time.Second))

	env.tick(t, tail)

	if got := runs(t, env, "hestia", claude.DefaultModel); got != 1 {
		t.Errorf("claude_runs_total{hestia,%s} = %v, want 1", claude.DefaultModel, got)
	}
	if got := runSeconds(t, env, "hestia"); got != 2 {
		t.Errorf("claude_run_seconds_total{hestia} = %v, want 2", got)
	}
}

// A run log shorter than the offset was truncated, so what is in it now was
// written after the last pass: resuming at the old offset would count nothing
// again until the file grew back past it.
func TestTailRereadsATruncatedLog(t *testing.T) {
	tail, env := newClaudeTail(t)
	env.appendRuns(t, "hestia",
		okRun("hestia", claude.DefaultModel, time.Second),
		okRun("hestia", claude.DefaultModel, time.Second))
	env.tick(t, tail)

	if err := os.Truncate(env.runsPath("hestia"), 0); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	env.appendRuns(t, "hestia", okRun("hestia", claude.DefaultModel, time.Second))
	env.tick(t, tail)

	if got := runs(t, env, "hestia", claude.DefaultModel); got != 3 {
		t.Errorf("claude_runs_total{hestia,%s} = %v, want 3", claude.DefaultModel, got)
	}
}

// A component nothing starts is invisible until an operator asks why the farm
// ran straight through a limit, so the wiring is proved on a whole guard: a
// limit line already in a run log becomes the farm's marker, a post and a
// scrapeable counter without anything else touching it.
func TestGuardTailsTheClaudeRunLogs(t *testing.T) {
	f := newGuardFixture(t, true)
	dir := filepath.Join(f.dir, "claude")
	reset := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	line, err := json.Marshal(limitRun("hestia", "weekly", reset))
	if err != nil {
		t.Fatalf("marshal record: %v", err)
	}
	write(t, claude.Config{Dir: dir, Agent: "hestia"}.RunsPath(), string(line)+"\n")

	f.run(t)

	waitFor(t, "the farm-wide limit marker", func() bool {
		held, active, err := claude.ReadMarker(claude.Config{Dir: dir}.MarkerPath(), time.Now().UTC())
		return err == nil && active && held.Equal(reset)
	})
	waitFor(t, "the limit post", func() bool {
		for _, post := range f.client.sent() {
			if strings.Contains(post.content, "Claude Code limit hit (weekly) by hestia") {
				return true
			}
		}
		return false
	})
	if body := scrape(t, f.guard.MetricsAddr()); !strings.Contains(body, "snowfarm_claude_limit_hits_total") {
		t.Fatalf("the exposition carries no snowfarm_claude_limit_hits_total:\n%s", body)
	}
}
