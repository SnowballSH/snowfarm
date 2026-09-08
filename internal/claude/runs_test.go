package claude

import (
	"maps"
	"os"
	"os/user"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunsLine(t *testing.T) {
	h := newHarness(t)

	if code := h.run("-p", "print the git HEAD", "--model", "claude-fable-5-1", "--effort", "high"); code != ExitOK {
		t.Fatalf("exit %d on a healthy run, want 0 (stderr: %s)", code, h.stderr.String())
	}
	if got := strings.TrimSpace(h.stdout.String()); got != "done: printed the git HEAD" {
		t.Errorf("stdout %q, want the result text", got)
	}

	record := h.record(t)
	fields := slices.Sorted(maps.Keys(record))
	want := []string{
		"agent", "api_error_status", "duration_ms", "effort", "is_error",
		"limit_kind", "model", "num_turns", "reset_at", "total_cost_usd", "ts",
	}
	if !slices.Equal(fields, want) {
		t.Fatalf("fields %q, want %q", fields, want)
	}
	ts, ok := record["ts"].(string)
	if !ok {
		t.Fatalf("ts %v is not a string", record["ts"])
	}
	if _, err := time.Parse(time.RFC3339, ts); err != nil {
		t.Errorf("ts %q: %v", ts, err)
	}
	for field, want := range map[string]any{
		"agent":            "hestia",
		"model":            "claude-fable-5-1",
		"effort":           "high",
		"num_turns":        float64(3),
		"duration_ms":      float64(1234),
		"is_error":         false,
		"api_error_status": float64(0),
		"limit_kind":       "",
		"reset_at":         "",
		"total_cost_usd":   0.0421,
	} {
		if got := record[field]; got != want {
			t.Errorf("%s = %v, want %v", field, got, want)
		}
	}
}

func TestTokenNeverInArgv(t *testing.T) {
	h := newHarness(t)
	if code := h.run("-p", "print the git HEAD"); code != ExitOK {
		t.Fatalf("exit %d, want 0 (stderr: %s)", code, h.stderr.String())
	}

	for _, arg := range h.argv() {
		if strings.Contains(arg, token) {
			t.Errorf("the token reached the argv: %q", arg)
		}
	}
	if got, ok := envLookup(h.childEnv(), "CLAUDE_CODE_OAUTH_TOKEN"); !ok || got != token {
		t.Fatalf("the child had no token in its environment (%q, present %v), so the argv check proves nothing", got, ok)
	}
	for _, surface := range []string{h.stdout.String(), h.stderr.String(), h.runsFile(t)} {
		if strings.Contains(surface, token) {
			t.Errorf("the token was written to %q", surface)
		}
	}
}

func TestAppendRecordIsOneLinePerRun(t *testing.T) {
	path := t.TempDir() + "/hestia.jsonl"
	if err := AppendRecord(path, Record{Agent: "hestia"}); err == nil {
		t.Fatal("a missing run ledger was created rather than reported")
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0o640)
	if err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		if err := AppendRecord(path, Record{Agent: "hestia", NumTurns: i}); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("%d lines, want 3: %q", len(lines), data)
	}
	for i, line := range lines {
		if !strings.Contains(line, `"num_turns":`+strconv.Itoa(i)) {
			t.Errorf("line %d is %q", i, line)
		}
	}
}

func TestIdentityFromUID(t *testing.T) {
	for _, tc := range []struct {
		name     string
		username string
		override string
		want     string
		wantErr  bool
	}{
		{name: "a farm user ignores the override", username: "farm-argus", override: "hestia", want: "argus"},
		{name: "a farm user needs no override", username: "farm-hestia", want: "hestia"},
		{name: "an unknown uid takes the override", username: "", override: "hestia", want: "hestia"},
		{name: "another user takes the override", username: "snowballsh", override: "hestia", want: "hestia"},
		{name: "another user without an override", username: "snowballsh", wantErr: true},
		{name: "an override that names a path", username: "snowballsh", override: "../../etc/passwd", wantErr: true},
		{name: "an empty farm user", username: "farm-", wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := AgentName(tc.username, tc.override)
			switch {
			case tc.wantErr && err == nil:
				t.Fatalf("got %q, want an error", got)
			case !tc.wantErr && err != nil:
				t.Fatal(err)
			case got != tc.want && !tc.wantErr:
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}

	me, err := user.LookupId(strconv.Itoa(os.Getuid()))
	if err == nil && strings.HasPrefix(me.Username, "farm-") {
		t.Skipf("this test process runs as %s", me.Username)
	}
	t.Setenv("FARM_AGENT", "hestia")
	name, err := CallerAgent()
	if err != nil {
		t.Fatal(err)
	}
	if name != "hestia" {
		t.Fatalf("CallerAgent = %q, want hestia", name)
	}
}
