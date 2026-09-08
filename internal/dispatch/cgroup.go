package dispatch

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// procFS is the process filesystem the guard reads. It is a type rather than
// a constant so the tests can answer from a fixture tree.
type procFS string

const defaultProcFS procFS = "/proc"

var runUnitLeaf = regexp.MustCompile(`snowfarm-run-[a-z][a-z0-9-]*-\d+\.service`)

// UnitForPID names the transient run unit a worker belongs to, for a card the
// ledger has no spawn row for — a run the guard inherited across its own
// restart.
func UnitForPID(pid int) (string, error) { return defaultProcFS.unitForPID(pid) }

func (p procFS) unitForPID(pid int) (string, error) {
	path := filepath.Join(string(p), strconv.Itoa(pid), "cgroup")
	data, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	matches := runUnitLeaf.FindAllString(string(data), -1)
	if len(matches) == 0 {
		return "", fmt.Errorf("%s names no snowfarm run unit", path)
	}
	return matches[len(matches)-1], nil
}

// pidAlive reads /proc/<pid>/status, which is readable across users, and
// treats a process that is gone or reaped-but-unwaited as dead.
func pidAlive(pid int) bool { return defaultProcFS.alive(pid) }

func (p procFS) alive(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(filepath.Join(string(p), strconv.Itoa(pid), "status"))
	if err != nil {
		return false
	}
	for line := range strings.Lines(string(data)) {
		state, ok := strings.CutPrefix(line, "State:")
		if !ok {
			continue
		}
		return !strings.HasPrefix(strings.TrimSpace(state), "Z")
	}
	return true
}
