package dispatch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/SnowballSH/snowfarm/internal/journal"
)

func fixture(t *testing.T, name string) string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return strings.TrimSpace(string(data))
}

func TestParseResultFindsJSON(t *testing.T) {
	entries := []journal.Entry{
		{Message: "hermes: warning: kanban board schema is older than the CLI", Priority: 4},
		{Message: fixture(t, "dispatch-spawned.json"), Priority: 6},
		{Message: "Main process exited, code=exited, status=0/SUCCESS", Priority: 6},
	}

	result, ok, err := ParseResult(entries)
	if err != nil || !ok {
		t.Fatalf("ParseResult = %v, %v; want a parsed result", ok, err)
	}
	if len(result.Spawned) != 1 {
		t.Fatalf("spawned %+v, want one task", result.Spawned)
	}
	if result.Spawned[0].TaskID != "t_1" || result.Spawned[0].Assignee != "hestia" {
		t.Fatalf("spawned[0] = %+v, want t_1 assigned to hestia", result.Spawned[0])
	}
	if result.Spawned[0].Workspace == "" {
		t.Fatal("spawned[0] carries no workspace")
	}
	if len(result.SkippedNonspawnable) != 1 {
		t.Fatalf("skipped_nonspawnable %v, want one entry", result.SkippedNonspawnable)
	}
}

func TestParseResultReadsIdlePass(t *testing.T) {
	entries := []journal.Entry{{Message: fixture(t, "dispatch-idle.json"), Priority: 6}}

	result, ok, err := ParseResult(entries)
	if err != nil || !ok {
		t.Fatalf("ParseResult = %v, %v; want a parsed empty result", ok, err)
	}
	if len(result.Spawned) != 0 {
		t.Fatalf("spawned %+v, want none", result.Spawned)
	}
}

func TestParseResultBindsSnakeCaseFields(t *testing.T) {
	entries := []journal.Entry{{Message: `{"spawned": [], "skipped_per_profile_capped": ` +
		`[{"task_id": "t_2", "assignee": "argus", "current": 1}]}`, Priority: 6}}

	result, ok, err := ParseResult(entries)
	if err != nil || !ok {
		t.Fatalf("ParseResult = %v, %v; want a parsed result", ok, err)
	}
	capped := result.SkippedPerProfileCapped
	if len(capped) != 1 || capped[0].TaskID != "t_2" || capped[0].Assignee != "argus" || capped[0].Current != 1 {
		t.Fatalf("skipped_per_profile_capped = %+v, want t_2/argus/1", capped)
	}
}

func TestParseResultReportsNoJSON(t *testing.T) {
	entries := []journal.Entry{
		{Message: "Traceback (most recent call last):", Priority: 3},
		{Message: "ModuleNotFoundError: No module named 'hermes_cli'", Priority: 3},
	}

	_, ok, err := ParseResult(entries)
	if err != nil {
		t.Fatalf("ParseResult errored on a crash journal: %v", err)
	}
	if ok {
		t.Fatal("ParseResult found a result in a crash journal")
	}
}

func TestParseResultRejectsMalformedResult(t *testing.T) {
	entries := []journal.Entry{{Message: `{"spawned": "all of them"}`, Priority: 6}}

	if _, _, err := ParseResult(entries); err == nil {
		t.Fatal("ParseResult accepted a result whose spawned field is not a list")
	}
}

func TestExitStatusReadsTheManagerLine(t *testing.T) {
	entries := []journal.Entry{
		{Message: `{"spawned": []}`, Priority: 6},
		{Message: "Main process exited, code=exited, status=75/n/a", Priority: 3},
	}

	status, ok := ExitStatus(entries)
	if !ok || status != 75 {
		t.Fatalf("ExitStatus = %d, %v; want 75", status, ok)
	}
}

func TestExitStatusIsAbsentWhileTheUnitRuns(t *testing.T) {
	entries := []journal.Entry{{Message: `{"spawned": []}`, Priority: 6}}

	if status, ok := ExitStatus(entries); ok {
		t.Fatalf("ExitStatus = %d, true; want no status for a live unit", status)
	}
}
