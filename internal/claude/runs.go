package claude

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"regexp"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"
)

const userPrefix = "farm-"

var agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,15}$`)

// Record is one line of runs/<agent>.jsonl: what the guard counts, and what
// it writes the farm-wide limit marker from.
type Record struct {
	TS             string  `json:"ts"`
	Agent          string  `json:"agent"`
	Model          string  `json:"model"`
	Effort         string  `json:"effort"`
	NumTurns       int     `json:"num_turns"`
	DurationMS     int64   `json:"duration_ms"`
	IsError        bool    `json:"is_error"`
	APIErrorStatus int     `json:"api_error_status"`
	LimitKind      string  `json:"limit_kind"`
	ResetAt        string  `json:"reset_at"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
}

// AppendRecord adds one line to the agent's own run log, which is the only
// path under the farm's claude directory the agent may write. The file is
// never created here: apply installs it 0640 farm-<agent>:farm-agents.
func AppendRecord(path string, record Record) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("flock %s: %w", path, err)
	}
	line, err := json.Marshal(record)
	if err != nil {
		return err
	}
	if _, err := file.Write(append(line, '\n')); err != nil {
		return err
	}
	return file.Close()
}

// CallerAgent names the agent from the uid the run is under, so the run log
// is chosen by who is running rather than by a variable something has to
// remember to set.
func CallerAgent() (string, error) {
	username := ""
	if caller, err := user.LookupId(strconv.Itoa(os.Getuid())); err == nil {
		username = caller.Username
	}
	return AgentName(username, os.Getenv("FARM_AGENT"))
}

// AgentName honours FARM_AGENT only when the uid resolves to no farm- user,
// which is how a test names an agent; under a farm- uid it is ignored.
func AgentName(username, override string) (string, error) {
	if name, ok := strings.CutPrefix(username, userPrefix); ok {
		if !agentName.MatchString(name) {
			return "", fmt.Errorf("the caller's user %q does not name an agent", username)
		}
		return name, nil
	}
	switch {
	case override == "":
		return "", fmt.Errorf("uid %d is %q, not a farm- user, and FARM_AGENT is unset", os.Getuid(), username)
	case !agentName.MatchString(override):
		return "", fmt.Errorf("FARM_AGENT=%q does not name an agent", override)
	}
	return override, nil
}
