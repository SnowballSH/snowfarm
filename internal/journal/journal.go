// Package journal reads what one transient run unit produced -- both the
// entries the agent's own processes wrote and the user manager's messages
// about the unit, which carry its exit status -- and the gateway's own log
// file, which is a plain file the guard tails by offset rather than a journal
// stream.
package journal

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

type Entry struct {
	Time     time.Time
	Message  string
	Priority int
}

type Reader interface {
	UserUnit(ctx context.Context, uid int, unit string, since time.Time) ([]Entry, error)
	GatewayLog(ctx context.Context, path string, offset int64) ([]string, int64, error)
}

const (
	DefaultJournalctl = "journalctl"

	// journalctl prints __REALTIME_TIMESTAMP whatever the field selection,
	// so the selection carries only what an Entry adds to it.
	outputFields = "--output-fields=MESSAGE,PRIORITY"

	maxLine = 1 << 20
)

type execReader struct{ journalctl string }

// NewExec returns the Reader the guard runs on the host. An empty journalctl
// takes the name on PATH.
func NewExec(journalctl string) Reader {
	if journalctl == "" {
		journalctl = DefaultJournalctl
	}
	return &execReader{journalctl: journalctl}
}

func (r *execReader) UserUnit(ctx context.Context, uid int, unit string, since time.Time) ([]Entry, error) {
	if uid <= 0 {
		return nil, fmt.Errorf("uid %d is not an agent's uid", uid)
	}
	if unit == "" || strings.ContainsAny(unit, " \t\n") {
		return nil, fmt.Errorf("unit %q is not one unit name", unit)
	}
	args := []string{"--no-pager", "-o", "json", outputFields}
	if !since.IsZero() {
		args = append(args, "--since", "@"+strconv.FormatInt(since.Unix(), 10))
	}
	args = append(args, unitMatches(uid, unit)...)

	cmd := exec.CommandContext(ctx, r.journalctl, args...) // #nosec G204 -- the arguments are a fixed flag set plus a checked uid and unit name
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("%s %s: %w: %s", r.journalctl, strings.Join(args, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return parseEntries(stdout.Bytes())
}

// unitMatches is the selection systemd's own add_matches_for_user_unit builds,
// reduced to the two clauses this reader needs and spelled for journalctl,
// whose "+" argument separates disjunctive groups while terms within a group
// are ANDed. The agent's processes log with _SYSTEMD_USER_UNIT=, but the user
// manager logs its messages about the unit -- "Main process exited,
// code=exited, status=N", "Failed with result 'exit-code'" -- with USER_UNIT=,
// so a selection carrying only the first clause can never see a run's exit
// status. journalctl's own --user-unit flag is not a substitute: it binds the
// uid to the caller's, and the guard reads as snowfarm, not as the agent.
func unitMatches(uid int, unit string) []string {
	owner := "_UID=" + strconv.Itoa(uid)
	return []string{
		owner, "_SYSTEMD_USER_UNIT=" + unit,
		"+",
		owner, "USER_UNIT=" + unit,
	}
}

func parseEntries(out []byte) ([]Entry, error) {
	scanner := bufio.NewScanner(bytes.NewReader(out))
	scanner.Buffer(make([]byte, 0, bufio.MaxScanTokenSize), maxLine)
	var entries []Entry
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}
		var rec record
		if err := json.Unmarshal(line, &rec); err != nil {
			return nil, fmt.Errorf("journal record %q: %w", line, err)
		}
		micros, err := strconv.ParseInt(rec.Realtime.value, 10, 64)
		if err != nil {
			return nil, fmt.Errorf("journal record %q: __REALTIME_TIMESTAMP: %w", line, err)
		}
		priority, err := strconv.Atoi(rec.Priority.value)
		if err != nil {
			priority = 0
		}
		entries = append(entries, Entry{
			Time:     time.UnixMicro(micros).UTC(),
			Message:  rec.Message.value,
			Priority: priority,
		})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return entries, nil
}

type record struct {
	Realtime field `json:"__REALTIME_TIMESTAMP"`
	Message  field `json:"MESSAGE"`
	Priority field `json:"PRIORITY"`
}

// field is any of the three shapes journalctl gives a field: the usual string,
// a number, or the array of byte values it falls back to for a message that is
// not valid UTF-8.
type field struct{ value string }

func (f *field) UnmarshalJSON(data []byte) error {
	var raw any
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	switch v := raw.(type) {
	case nil:
		f.value = ""
	case string:
		f.value = v
	case float64:
		f.value = strconv.FormatFloat(v, 'f', -1, 64)
	case []any:
		out := make([]byte, 0, len(v))
		for _, element := range v {
			n, ok := element.(float64)
			if !ok || n < 0 || n > 255 {
				return fmt.Errorf("journal field %s is not a byte array", data)
			}
			out = append(out, byte(n))
		}
		f.value = string(out)
	default:
		return fmt.Errorf("journal field %s has no readable value", data)
	}
	return nil
}

// GatewayLog returns the complete lines written after offset and the offset to
// resume from. A line still being written is left for the next call, and a
// file shorter than the offset has been rotated, so reading restarts at its
// beginning.
func (r *execReader) GatewayLog(_ context.Context, path string, offset int64) ([]string, int64, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, offset, err
	}
	defer func() { _ = f.Close() }()

	info, err := f.Stat()
	if err != nil {
		return nil, offset, err
	}
	if offset < 0 || info.Size() < offset {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return nil, offset, err
	}

	reader := bufio.NewReader(f)
	var lines []string
	for {
		line, err := reader.ReadString('\n')
		if errors.Is(err, io.EOF) {
			return lines, offset, nil
		}
		if err != nil {
			return lines, offset, err
		}
		offset += int64(len(line))
		lines = append(lines, strings.TrimRight(line, "\r\n"))
	}
}
