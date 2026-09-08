package guard

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/SnowballSH/snowfarm/internal/claude"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

// markerMode is the mode apply gives every other file in shared/. The guard
// runs with Group=farm-agents, so a marker it creates with this mode is
// readable by every agent and writable by none of them.
const markerMode = fs.FileMode(0o640)

// ClaudeTail folds each agent's own Claude Code run log into the farm's
// counters and writes the farm-wide limit marker from what it finds there.
// The wrapper only ever reads that marker: shared/ is not agent-writable, so
// the guard is its only writer and — for a window that has ended — its only
// remover.
type ClaudeTail struct {
	Roster  *atomic.Pointer[roster.Roster]
	Ledger  *dispatch.Ledger
	Post    func(ctx context.Context, text string)
	Metrics *metrics.Registry
	Now     func() time.Time
	Log     *slog.Logger
}

// limit is a window the farm is out of: how far it runs, and who reported it.
type limit struct {
	agent string
	kind  string
	reset time.Time
}

// Tick reads every run log written since the last pass, then settles the
// marker on the furthest window those lines reported.
func (t *ClaudeTail) Tick(ctx context.Context) error {
	r := t.Roster.Load()
	if r == nil {
		return errors.New("claude tail: the guard holds no roster")
	}
	now := t.now()
	var (
		errs   []error
		newest limit
	)
	for _, agent := range r.EnabledAgents() {
		records, err := t.read(r.Farm.ClaudeDir, agent.Name)
		if err != nil {
			errs = append(errs, err)
		}
		for _, record := range records {
			if reported, ok := t.count(agent.Name, record); ok && reported.reset.After(newest.reset) {
				newest = reported
			}
		}
	}
	return errors.Join(append(errs, t.sweepMarker(ctx, r.Farm.ClaudeDir, newest, now))...)
}

// read returns the records written since the last pass and moves the agent's
// offset past them. A line still being written is left for the next pass, and
// a log shorter than the offset was truncated, so reading starts again at its
// beginning: everything in it now was written after the last pass.
func (t *ClaudeTail) read(dir, agent string) ([]claude.Record, error) {
	key := claudeOffsetKey(agent)
	offset, _, err := t.Ledger.Offset(key)
	if err != nil {
		return nil, err
	}
	path := claude.Config{Dir: dir, Agent: agent}.RunsPath()
	file, err := os.Open(path) // #nosec G304 -- the path is the roster's own Claude tree and the agent's name
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = file.Close() }()

	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() < offset {
		offset = 0
	}
	if _, err := file.Seek(offset, io.SeekStart); err != nil {
		return nil, err
	}
	records, read, err := t.decode(file, path)
	return records, errors.Join(err, t.Ledger.SetOffset(key, offset+read))
}

// decode reads the complete lines the reader holds, skipping any that is not
// a run record: one unreadable line must not stop the pass counting the runs
// after it, nor hold the offset where re-reading it is all the guard ever
// does.
func (t *ClaudeTail) decode(source io.Reader, path string) ([]claude.Record, int64, error) {
	reader := bufio.NewReader(source)
	var (
		records []claude.Record
		read    int64
	)
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return records, read, nil
		}
		if err != nil {
			return records, read, fmt.Errorf("read %s: %w", path, err)
		}
		read += int64(len(line))
		if strings.TrimSpace(line) == "" {
			continue
		}
		var record claude.Record
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.log().Error("a line of a Claude Code run log was not a run record and was skipped", "path", path, "error", err)
			continue
		}
		records = append(records, record)
	}
}

// count folds one record into the farm's counters and reports the limit it
// carries. The agent is the log the line was read from rather than the name
// inside it: an agent's own run log is the one path under the Claude tree it
// may write, so what a line says about itself is a claim and the file it sits
// in is a fact. A run the marker refused never reached Claude Code, so it is
// neither a run nor a limit hit — counting one would report both again on
// every attempt for as long as the window lasts.
func (t *ClaudeTail) count(agent string, record claude.Record) (limit, bool) {
	if record.LimitKind == claude.KindMarker {
		return limit{}, false
	}
	t.Metrics.ClaudeRunsTotal.WithLabelValues(agent, record.Model).Inc()
	if record.DurationMS > 0 {
		t.Metrics.ClaudeRunSecondsTotal.WithLabelValues(agent).Add((time.Duration(record.DurationMS) * time.Millisecond).Seconds())
	}
	if record.LimitKind == "" {
		return limit{}, false
	}
	kind := strings.ToLower(record.LimitKind)
	t.Metrics.ClaudeLimitHitsTotal.WithLabelValues(kind).Inc()
	reset, err := time.Parse(time.RFC3339, record.ResetAt)
	if err != nil {
		t.log().Error("a Claude Code limit was recorded with no readable reset time, so it moved no marker",
			"agent", agent, "kind", kind, "reset_at", record.ResetAt)
		return limit{}, false
	}
	return limit{agent: agent, kind: kind, reset: reset}, true
}

// sweepMarker settles the farm-wide window: a marker whose time has passed —
// or that no longer reads as a time at all, which would fail every run rather
// than refuse it — is removed, and a limit reaching further than the window
// the farm already holds replaces it. Only the first hit of a window is
// posted; extending one the farm is already sitting out says nothing new.
func (t *ClaudeTail) sweepMarker(ctx context.Context, dir string, newest limit, now time.Time) error {
	path := claude.Config{Dir: dir}.MarkerPath()
	current, active, err := claude.ReadMarker(path, now)
	if err != nil {
		t.log().Error("the Claude Code limit marker holds no readable time, so it was removed: every run would have failed on it",
			"path", path, "error", err)
	}
	if !active {
		current = time.Time{}
		if err := removeMarker(path); err != nil {
			return err
		}
	}
	if !newest.reset.After(current) || !newest.reset.After(now) {
		return nil
	}
	if err := writeMarker(path, newest.reset); err != nil {
		return err
	}
	if !active {
		t.post(ctx, fmt.Sprintf("Claude Code limit hit (%s) by %s; resets %s",
			newest.kind, newest.agent, newest.reset.UTC().Format(time.RFC3339)))
	}
	return nil
}

// writeMarker installs the window under a name the wrapper only ever finds
// whole: a reader that caught a half-written file would read no time at all
// and fail the run it was meant to refuse.
func writeMarker(path string, reset time.Time) error {
	temp := path + ".tmp"
	if err := os.WriteFile(temp, []byte(reset.UTC().Format(time.RFC3339)+"\n"), markerMode); err != nil {
		return err
	}
	if err := os.Rename(temp, path); err != nil {
		return errors.Join(err, os.Remove(temp))
	}
	return nil
}

func removeMarker(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func (t *ClaudeTail) post(ctx context.Context, text string) {
	if t.Post != nil {
		t.Post(ctx, text)
	}
}

func (t *ClaudeTail) now() time.Time {
	if t.Now != nil {
		return t.Now()
	}
	return time.Now().UTC()
}

func (t *ClaudeTail) log() *slog.Logger {
	if t.Log != nil {
		return t.Log
	}
	return slog.Default()
}

func claudeOffsetKey(agent string) string { return "claude:" + agent }
