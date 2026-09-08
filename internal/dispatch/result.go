package dispatch

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/SnowballSH/snowfarm/internal/journal"
)

// Result is what `hermes kanban dispatch --json` prints. Every field carries
// an explicit json tag: the CLI emits snake_case and Go's case-insensitive
// fallback does not bridge task_id to TaskID, so an untagged field would bind
// nothing and decode without error.
type Result struct {
	Spawned []struct {
		TaskID    string `json:"task_id"`
		Assignee  string `json:"assignee"`
		Workspace string `json:"workspace"`
	} `json:"spawned"`
	SkippedUnassigned       []json.RawMessage `json:"skipped_unassigned"`
	SkippedNonspawnable     []json.RawMessage `json:"skipped_nonspawnable"`
	SkippedPerProfileCapped []struct {
		TaskID   string `json:"task_id"`
		Assignee string `json:"assignee"`
		Current  int    `json:"current"`
	} `json:"skipped_per_profile_capped"`
	AutoAssignedDefault []json.RawMessage `json:"auto_assigned_default"`
	Reclaimed           []json.RawMessage `json:"reclaimed"`
	Crashed             []json.RawMessage `json:"crashed"`
	TimedOut            []json.RawMessage `json:"timed_out"`
	Stale               []json.RawMessage `json:"stale"`
	AutoBlocked         []json.RawMessage `json:"auto_blocked"`
	Promoted            []json.RawMessage `json:"promoted"`
}

const spawnedKey = "spawned"

// ParseResult finds the pass's answer among the lines its unit logged. A pass
// prints warnings and tracebacks alongside its result, so a line is a
// candidate only when it is a JSON object carrying the one key every dispatch
// result has; a candidate that will not decode is an error rather than a
// silent miss, because reading it as "no result" would re-issue a pass that
// already ran.
func ParseResult(entries []journal.Entry) (Result, bool, error) {
	for _, entry := range entries {
		line := strings.TrimSpace(entry.Message)
		if !strings.HasPrefix(line, "{") || !strings.HasSuffix(line, "}") {
			continue
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &fields); err != nil {
			continue
		}
		if _, ok := fields[spawnedKey]; !ok {
			continue
		}
		var result Result
		if err := json.Unmarshal([]byte(line), &result); err != nil {
			return Result{}, false, fmt.Errorf("dispatch result %q: %w", line, err)
		}
		return result, true, nil
	}
	return Result{}, false, nil
}

var exitLine = regexp.MustCompile(`Main process exited, code=exited, status=(\d+)`)

// ExitStatus reads the run's exit status from the user manager's own message
// about the unit. The transient unit is --collect'ed, so it is gone by the
// time anything could ask systemctl for ExecMainStatus, and this line is the
// only place the status survives.
func ExitStatus(entries []journal.Entry) (int, bool) {
	for i := len(entries) - 1; i >= 0; i-- {
		match := exitLine.FindStringSubmatch(entries[i].Message)
		if match == nil {
			continue
		}
		status, err := strconv.Atoi(match[1])
		if err != nil {
			continue
		}
		return status, true
	}
	return 0, false
}
