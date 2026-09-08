package claude

import (
	"strings"
	"testing"
	"time"
)

var streamNow = time.Date(2026, 9, 7, 9, 30, 0, 0, time.UTC)

func TestParseStreamLimits(t *testing.T) {
	resetsAt := time.Date(2026, 9, 9, 4, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name     string
		stream   string
		isLimit  bool
		kind     string
		resetAt  time.Time
		text     string
		isError  bool
		apiError int
	}{
		{
			name: "rate limit event wins over the message",
			stream: `{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1}}
{"type":"rate_limit_event","rate_limit_info":{"status":"rejected","resetsAt":` + itoa(resetsAt.Unix()) + `}}
{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your weekly limit · resets Mon 12:00am"}`,
			isLimit: true, kind: "weekly", resetAt: resetsAt,
			text: "You've hit your weekly limit · resets Mon 12:00am", isError: true, apiError: 429,
		},
		{
			name:    "message reset without an event",
			stream:  `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"You've hit your weekly limit · resets Mon 12:00am"}`,
			isLimit: true, kind: "weekly", resetAt: time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC),
			text: "You've hit your weekly limit · resets Mon 12:00am", isError: true, apiError: 429,
		},
		{
			name:    "unparseable reset falls back to half an hour",
			stream:  `{"type":"result","subtype":"success","is_error":true,"result":"You've hit your Opus limit · resets when the moon is right"}`,
			isLimit: true, kind: "opus", resetAt: streamNow.Add(30 * time.Minute),
			text: "You've hit your Opus limit · resets when the moon is right", isError: true,
		},
		{
			name:    "session limit",
			stream:  `{"type":"result","subtype":"success","is_error":true,"result":"You’ve hit your session limit · resets 3:45pm"}`,
			isLimit: true, kind: "session", resetAt: time.Date(2026, 9, 7, 15, 45, 0, 0, time.UTC),
			text: "You’ve hit your session limit · resets 3:45pm", isError: true,
		},
		{
			name:    "credits",
			stream:  `{"type":"result","subtype":"error_during_execution","is_error":true,"errorCode":"credits_required","result":"Credits required to continue"}`,
			isLimit: true, kind: KindCredits, resetAt: streamNow.Add(30 * time.Minute),
			text: "Credits required to continue", isError: true,
		},
		{
			name:    "fable credits",
			stream:  `{"type":"result","subtype":"success","is_error":false,"result":"Fable limit reached · continuing on Fable 5.1 uses usage credits"}`,
			isLimit: true, kind: KindFable, resetAt: streamNow.Add(30 * time.Minute),
			text: "Fable limit reached · continuing on Fable 5.1 uses usage credits",
		},
		{
			name:    "429 without a recognised message",
			stream:  `{"type":"result","subtype":"success","is_error":true,"api_error_status":429,"result":"upstream said no"}`,
			isLimit: true, kind: KindUnknown, resetAt: streamNow.Add(30 * time.Minute),
			text: "upstream said no", isError: true, apiError: 429,
		},
		{
			name: "a healthy run",
			stream: `{"type":"system","subtype":"init"}
{"type":"assistant","message":{"content":"working"}}
{"type":"result","subtype":"success","is_error":false,"num_turns":3,"duration_ms":1234,"total_cost_usd":0.0421,"result":"done: printed the git HEAD"}`,
			text: "done: printed the git HEAD",
		},
		{
			name:    "an ordinary failure is not a limit",
			stream:  `{"type":"result","subtype":"error_during_execution","is_error":true,"result":"the build failed"}`,
			text:    "the build failed",
			isError: true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream, err := ParseStream(strings.NewReader(tc.stream))
			if err != nil {
				t.Fatal(err)
			}
			result, complete := stream.Result()
			if !complete {
				t.Fatal("the stream carried no result object")
			}
			if result.Result != tc.text {
				t.Errorf("text %q, want %q", result.Result, tc.text)
			}
			if result.IsError != tc.isError {
				t.Errorf("is_error %v, want %v", result.IsError, tc.isError)
			}
			if result.APIErrorStatus != tc.apiError {
				t.Errorf("api_error_status %d, want %d", result.APIErrorStatus, tc.apiError)
			}
			limit, ok := stream.Limit(streamNow)
			if ok != tc.isLimit {
				t.Fatalf("limit %v (%+v), want %v", ok, limit, tc.isLimit)
			}
			if !ok {
				return
			}
			if limit.Kind != tc.kind {
				t.Errorf("limit kind %q, want %q", limit.Kind, tc.kind)
			}
			if !limit.ResetAt.Equal(tc.resetAt) {
				t.Errorf("reset_at %v, want %v", limit.ResetAt, tc.resetAt)
			}
		})
	}
}

func TestParseStreamIgnoresNoise(t *testing.T) {
	stream, err := ParseStream(strings.NewReader("not json at all\n\n{\"type\":\"result\",\"subtype\":\"success\",\"num_turns\":2,\"result\":\"fine\"}\n"))
	if err != nil {
		t.Fatal(err)
	}
	result, complete := stream.Result()
	if !complete || result.Result != "fine" || result.NumTurns != 2 {
		t.Errorf("result %+v (complete %v), want \"fine\" and 2 turns", result, complete)
	}

	if _, complete := mustParse(t, "{\"type\":\"system\",\"subtype\":\"init\"}\n").Result(); complete {
		t.Error("a stream with no result object reported one")
	}
}

func mustParse(t *testing.T, text string) Stream {
	t.Helper()
	stream, err := ParseStream(strings.NewReader(text))
	if err != nil {
		t.Fatal(err)
	}
	return stream
}

func TestResultLimitRecordsResetAt(t *testing.T) {
	h := newHarness(t)
	resetsAt := h.now.Add(38 * time.Hour)
	h.setEnv("FAKE_CLAUDE_MODE", "limit")
	h.setEnv("FAKE_CLAUDE_RESETS_AT", itoa(resetsAt.Unix()))

	if code := h.run("-p", "print the git HEAD"); code != ExitLimitHit {
		t.Fatalf("exit %d on a limit, want %d (stderr: %s)", code, ExitLimitHit, h.stderr.String())
	}
	if want := "Claude Code limit: weekly; resets " + resetsAt.Format(time.RFC3339); !strings.Contains(h.stdout.String(), want) {
		t.Errorf("stdout %q, want it to carry %q", h.stdout.String(), want)
	}
	record := h.record(t)
	if record["limit_kind"] != "weekly" {
		t.Errorf("limit_kind %v, want weekly", record["limit_kind"])
	}
	if record["reset_at"] != resetsAt.Format(time.RFC3339) {
		t.Errorf("reset_at %v, want %v", record["reset_at"], resetsAt.Format(time.RFC3339))
	}
	if record["api_error_status"] != float64(429) || record["is_error"] != true {
		t.Errorf("record %v, want api_error_status 429 and is_error true", record)
	}
}

func TestResultLimitWithoutResetsAt(t *testing.T) {
	h := newHarness(t)
	h.setEnv("FAKE_CLAUDE_MODE", "limit")

	if code := h.run("-p", "print the git HEAD"); code != ExitLimitHit {
		t.Fatalf("exit %d on a limit, want %d (stderr: %s)", code, ExitLimitHit, h.stderr.String())
	}
	want := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC).Format(time.RFC3339)
	record := h.record(t)
	if record["limit_kind"] != "weekly" {
		t.Errorf("limit_kind %v, want weekly", record["limit_kind"])
	}
	if record["reset_at"] != want {
		t.Errorf("reset_at %v, want %v resolved from the message", record["reset_at"], want)
	}
}

func TestResultCredits(t *testing.T) {
	h := newHarness(t)
	h.setEnv("FAKE_CLAUDE_MODE", "credits")

	if code := h.run("-p", "print the git HEAD"); code != ExitLimitHit {
		t.Fatalf("exit %d on credits_required, want %d (stderr: %s)", code, ExitLimitHit, h.stderr.String())
	}
	record := h.record(t)
	if record["limit_kind"] != KindCredits {
		t.Errorf("limit_kind %v, want %q", record["limit_kind"], KindCredits)
	}
	if record["reset_at"] != h.now.Add(30*time.Minute).Format(time.RFC3339) {
		t.Errorf("reset_at %v, want the half-hour fallback", record["reset_at"])
	}
}

func TestResultError(t *testing.T) {
	h := newHarness(t)
	h.setEnv("FAKE_CLAUDE_MODE", "error")

	if code := h.run("-p", "print the git HEAD"); code != ExitError {
		t.Fatalf("exit %d on a failed run, want %d (stderr: %s)", code, ExitError, h.stderr.String())
	}
	if !strings.Contains(h.stdout.String(), "the build failed") {
		t.Errorf("stdout %q, want claude's own result text", h.stdout.String())
	}
	record := h.record(t)
	if record["is_error"] != true || record["limit_kind"] != "" {
		t.Errorf("record %v, want is_error true and no limit_kind", record)
	}
}
