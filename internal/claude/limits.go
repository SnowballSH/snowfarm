package claude

import (
	"fmt"
	"os"
	"strings"
	"time"
)

// The kinds that do not come from a limit message. The four that do —
// session, weekly, opus, sonnet — are the message's own word, lowercased.
const (
	KindMarker  = "marker"
	KindCredits = "credits"
	KindFable   = "fable"
	KindUnknown = "unknown"
)

// FallbackReset is how long a limit whose reset time is unreadable is
// assumed to last: long enough to stop a retry loop, short enough that the
// farm does not sit out a window that has already passed.
const FallbackReset = 30 * time.Minute

// Limit is a window the farm is out of, and when it ends.
type Limit struct {
	Kind    string
	ResetAt time.Time
}

func (l Limit) Message() string {
	return fmt.Sprintf("Claude Code limit: %s; resets %s", l.Kind, l.ResetAt.Format(time.RFC3339))
}

// ReadMarker reports the farm-wide limit window the guard has written. An
// expired marker is left where it is: shared/ is not agent-writable, and the
// guard is the marker's only writer and only remover.
func ReadMarker(path string, now time.Time) (time.Time, bool, error) {
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return time.Time{}, false, nil
	}
	if err != nil {
		return time.Time{}, false, err
	}
	reset, err := time.Parse(time.RFC3339, strings.TrimSpace(string(data)))
	if err != nil {
		return time.Time{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return reset, reset.After(now), nil
}

var weekdays = map[string]time.Weekday{
	"sunday": time.Sunday, "sun": time.Sunday,
	"monday": time.Monday, "mon": time.Monday,
	"tuesday": time.Tuesday, "tue": time.Tuesday, "tues": time.Tuesday,
	"wednesday": time.Wednesday, "wed": time.Wednesday,
	"thursday": time.Thursday, "thu": time.Thursday, "thur": time.Thursday, "thurs": time.Thursday,
	"friday": time.Friday, "fri": time.Friday,
	"saturday": time.Saturday, "sat": time.Saturday,
}

var clockLayouts = []string{"3:04pm", "3pm", "15:04"}

// ParseResetText resolves the reset time a limit message states: a clock time
// today or tomorrow, or a weekday and a clock time on the next such day.
func ParseResetText(text string, now time.Time) (time.Time, bool) {
	fields := strings.Fields(strings.ToLower(strings.TrimSpace(text)))
	if len(fields) == 0 {
		return time.Time{}, false
	}
	weekday, onWeekday := weekdays[strings.TrimSuffix(fields[0], ".")]
	if onWeekday {
		fields = fields[1:]
	}
	clock, ok := parseClock(strings.Join(fields, ""))
	if !ok {
		return time.Time{}, false
	}
	start := time.Date(now.Year(), now.Month(), now.Day(), clock.Hour(), clock.Minute(), 0, 0, now.Location())
	if !onWeekday {
		if !start.After(now) {
			return start.AddDate(0, 0, 1), true
		}
		return start, true
	}
	for day := range 8 {
		candidate := start.AddDate(0, 0, day)
		if candidate.Weekday() == weekday && candidate.After(now) {
			return candidate, true
		}
	}
	return time.Time{}, false
}

func parseClock(text string) (time.Time, bool) {
	for _, layout := range clockLayouts {
		if clock, err := time.Parse(layout, text); err == nil {
			return clock, true
		}
	}
	return time.Time{}, false
}
