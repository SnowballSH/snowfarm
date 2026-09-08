package claude

import (
	"os"
	"strings"
	"testing"
	"time"
)

func TestLimitMarker(t *testing.T) {
	h := newHarness(t)
	reset := h.now.Add(time.Hour)
	h.writeMarker(reset)

	if code := h.run("-p", "print the git HEAD"); code != ExitLimitWindow {
		t.Fatalf("exit %d inside a limit window, want %d (stderr: %s)", code, ExitLimitWindow, h.stderr.String())
	}
	if want := "Claude Code limit: marker; resets " + reset.Format(time.RFC3339); !strings.Contains(h.stdout.String(), want) {
		t.Errorf("stdout %q, want it to carry %q", h.stdout.String(), want)
	}
	if h.ranClaude() {
		t.Error("the wrapper execed claude inside a limit window")
	}
	records := h.records()
	if len(records) != 1 {
		t.Fatalf("records %v, want one", records)
	}
	if got := records[0]["limit_kind"]; got != KindMarker {
		t.Errorf("limit_kind %v, want %q", got, KindMarker)
	}
	if got := records[0]["reset_at"]; got != reset.Format(time.RFC3339) {
		t.Errorf("reset_at %v, want %v", got, reset.Format(time.RFC3339))
	}

	expired := h.now.Add(-time.Hour)
	h.writeMarker(expired)
	h.reset()
	if code := h.run("-p", "print the git HEAD"); code != ExitOK {
		t.Fatalf("exit %d with an expired marker, want %d (stderr: %s)", code, ExitOK, h.stderr.String())
	}
	if !h.ranClaude() {
		t.Error("an expired marker still blocked the run")
	}
	if _, err := os.Stat(h.config().MarkerPath()); err != nil {
		t.Errorf("the wrapper touched the guard-owned marker: %v", err)
	}
}

func TestReadMarker(t *testing.T) {
	now := time.Date(2026, 9, 7, 9, 30, 0, 0, time.UTC)
	dir := t.TempDir()
	path := dir + "/limit-until"

	if _, active, err := ReadMarker(path, now); err != nil || active {
		t.Errorf("absent marker: active=%v err=%v", active, err)
	}

	future := now.Add(90 * time.Minute)
	if err := os.WriteFile(path, []byte(future.Format(time.RFC3339)+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	switch reset, active, err := ReadMarker(path, now); {
	case err != nil || !active:
		t.Errorf("future marker: active=%v err=%v", active, err)
	case !reset.Equal(future):
		t.Errorf("future marker: reset %v, want %v", reset, future)
	}

	if err := os.WriteFile(path, []byte(now.Add(-time.Second).Format(time.RFC3339)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, active, err := ReadMarker(path, now); err != nil || active {
		t.Errorf("expired marker: active=%v err=%v", active, err)
	}

	if err := os.WriteFile(path, []byte("soon"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := ReadMarker(path, now); err == nil {
		t.Error("an unreadable marker was ignored rather than reported")
	}
}

func TestParseResetText(t *testing.T) {
	now := time.Date(2026, 9, 7, 9, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		text string
		want time.Time
	}{
		{"3:45pm", time.Date(2026, 9, 7, 15, 45, 0, 0, time.UTC)},
		{"9:00am", time.Date(2026, 9, 8, 9, 0, 0, 0, time.UTC)},
		{"12:00 AM", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)},
		{"Mon 12:00am", time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC)},
		{"Tue 12:00am", time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)},
		{"Monday 11:00pm", time.Date(2026, 9, 7, 23, 0, 0, 0, time.UTC)},
		{"18:15", time.Date(2026, 9, 7, 18, 15, 0, 0, time.UTC)},
	} {
		got, ok := ParseResetText(tc.text, now)
		if !ok {
			t.Errorf("%q: not parsed", tc.text)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%q: got %v, want %v", tc.text, got, tc.want)
		}
	}
	for _, text := range []string{"", "when the moon is right", "25:00"} {
		if got, ok := ParseResetText(text, now); ok {
			t.Errorf("%q parsed as %v", text, got)
		}
	}
}
