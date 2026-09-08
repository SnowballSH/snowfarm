package journal

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

const (
	fakebinStateEnv = "SNOWFARM_FAKEBIN_STATE"
	fakebinModeEnv  = "SNOWFARM_FAKEBIN_MODE"
	unit            = "snowfarm-run-hestia-1757000000123456789.service"
)

func fakeJournalctl(t *testing.T) string {
	t.Helper()
	fakebin, err := filepath.Abs(filepath.Join("testdata", "fakebin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := t.TempDir()
	t.Setenv(fakebinStateEnv, state)
	return state
}

func recordedArgv(t *testing.T, state string) []string {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(state, "argv"))
	if err != nil {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")[1:]
}

func TestJournalParsesJSON(t *testing.T) {
	state := fakeJournalctl(t)
	since := time.Unix(1757000000, 500_000_000).UTC()

	entries, err := NewExec("journalctl").UserUnit(context.Background(), 6003, unit, since)
	if err != nil {
		t.Fatal(err)
	}

	want := []Entry{
		{
			Time:     time.UnixMicro(1757000000123456).UTC(),
			Message:  `{"spawned": [{"task_id": "t_1"}], "promoted": []}`,
			Priority: 6,
		},
		{
			Time:     time.UnixMicro(1757000001654321).UTC(),
			Message:  "Main process exited, code=exited, status=75/n/a",
			Priority: 3,
		},
	}
	if !slices.Equal(entries, want) {
		t.Errorf("UserUnit returned %+v, want %+v", entries, want)
	}

	argv := recordedArgv(t, state)
	wantArgv := []string{
		"--no-pager", "-o", "json", "--output-fields=MESSAGE,PRIORITY",
		"--since", "@1757000000",
		"_UID=6003", "_SYSTEMD_USER_UNIT=" + unit,
		"+",
		"_UID=6003", "USER_UNIT=" + unit,
	}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("journalctl argv %v, want %v", argv, wantArgv)
	}
}

func TestUserUnitSeesTheManagersExitLine(t *testing.T) {
	state := fakeJournalctl(t)

	entries, err := NewExec("journalctl").UserUnit(context.Background(), 6003, unit, time.Time{})
	if err != nil {
		t.Fatal(err)
	}

	const exited = "Main process exited, code=exited, status=75/n/a"
	if !slices.ContainsFunc(entries, func(e Entry) bool { return e.Message == exited }) {
		t.Errorf("UserUnit returned %+v, none of it the run's exit status", entries)
	}

	argv := recordedArgv(t, state)
	group := slices.Index(argv, "+")
	if group < 0 {
		t.Fatalf("journalctl argv %v selects one group, so the manager's messages about the unit are out of reach", argv)
	}
	if manager := argv[group+1:]; !slices.Equal(manager, []string{"_UID=6003", "USER_UNIT=" + unit}) {
		t.Errorf("the second match group is %v, want the user manager's own tagging of the unit", manager)
	}
}

func TestUserUnitWithoutASince(t *testing.T) {
	state := fakeJournalctl(t)
	if _, err := NewExec("journalctl").UserUnit(context.Background(), 6003, unit, time.Time{}); err != nil {
		t.Fatal(err)
	}
	if argv := recordedArgv(t, state); slices.Contains(argv, "--since") {
		t.Errorf("journalctl argv %v carries a --since for the zero time", argv)
	}
}

func TestUserUnitRefusesAnUnreadableSelection(t *testing.T) {
	cases := []struct {
		name string
		uid  int
		unit string
	}{
		{name: "no uid", uid: 0, unit: unit},
		{name: "no unit", uid: 6003},
		{name: "a unit that is not one name", uid: 6003, unit: "a.service b.service"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state := fakeJournalctl(t)
			if _, err := NewExec("journalctl").UserUnit(context.Background(), tc.uid, tc.unit, time.Time{}); err == nil {
				t.Fatal("UserUnit succeeded, want a refusal")
			}
			if _, err := os.Stat(filepath.Join(state, "argv")); !errors.Is(err, os.ErrNotExist) {
				t.Error("journalctl ran for a selection it cannot answer")
			}
		})
	}
}

func TestUserUnitReportsFailure(t *testing.T) {
	fakeJournalctl(t)
	t.Setenv(fakebinModeEnv, "fail")

	_, err := NewExec("journalctl").UserUnit(context.Background(), 6003, unit, time.Time{})
	if err == nil {
		t.Fatal("UserUnit succeeded, want the fake's failure")
	}
	if !strings.Contains(err.Error(), "Failed to add match") {
		t.Errorf("error %q carries no stderr", err)
	}
}

func TestUserUnitOnAnEmptyJournal(t *testing.T) {
	fakeJournalctl(t)
	t.Setenv(fakebinModeEnv, "empty")

	entries, err := NewExec("journalctl").UserUnit(context.Background(), 6003, unit, time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Errorf("UserUnit returned %+v, want nothing", entries)
	}
}

func TestParseEntries(t *testing.T) {
	cases := []struct {
		name    string
		out     string
		want    []Entry
		wantErr bool
	}{
		{
			name: "a message journald could not read as text",
			out:  `{"__REALTIME_TIMESTAMP":"1757000000000001","PRIORITY":6,"MESSAGE":[104,105,10]}`,
			want: []Entry{{Time: time.UnixMicro(1757000000000001).UTC(), Message: "hi\n", Priority: 6}},
		},
		{
			name: "a line that is not a record",
			out:  "-- No entries --\n",
		},
		{
			name: "a blank line between records",
			out:  "\n" + `{"__REALTIME_TIMESTAMP":"1757000000000002","PRIORITY":"7","MESSAGE":"tick"}` + "\n\n",
			want: []Entry{{Time: time.UnixMicro(1757000000000002).UTC(), Message: "tick", Priority: 7}},
		},
		{
			name:    "a record that is not JSON",
			out:     `{"__REALTIME_TIMESTAMP":`,
			wantErr: true,
		},
		{
			name:    "a record without a timestamp",
			out:     `{"PRIORITY":"6","MESSAGE":"tick"}`,
			wantErr: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries, err := parseEntries([]byte(tc.out))
			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseEntries returned %+v, want an error", entries)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(entries, tc.want) {
				t.Errorf("parseEntries returned %+v, want %+v", entries, tc.want)
			}
		})
	}
}

func TestGatewayLogTails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	write := func(text string) {
		t.Helper()
		f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteString(text); err != nil {
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
	}
	read := func(offset int64) Tail {
		t.Helper()
		tail, err := NewExec("journalctl").GatewayLog(context.Background(), path, offset)
		if err != nil {
			t.Fatal(err)
		}
		return tail
	}

	write("first\nsecond\n")
	tail := read(0)
	if want := []string{"first", "second"}; !slices.Equal(tail.Lines, want) {
		t.Errorf("read %v, want %v", tail.Lines, want)
	}
	if tail.Offset != 13 || tail.Rotated {
		t.Errorf("offset %d rotated=%v, want 13 and false", tail.Offset, tail.Rotated)
	}

	if again := read(tail.Offset); len(again.Lines) != 0 || again.Offset != tail.Offset {
		t.Errorf("re-reading returned %v at %d, want nothing at %d", again.Lines, again.Offset, tail.Offset)
	}

	write("part")
	partial := read(tail.Offset)
	if len(partial.Lines) != 0 || partial.Offset != tail.Offset {
		t.Errorf("a partial line returned %v at %d, want nothing at %d", partial.Lines, partial.Offset, tail.Offset)
	}

	write("ial\n")
	tail = read(tail.Offset)
	if want := []string{"partial"}; !slices.Equal(tail.Lines, want) {
		t.Errorf("read %v, want %v", tail.Lines, want)
	}
	if tail.Offset != 21 {
		t.Errorf("offset %d, want 21", tail.Offset)
	}

	// A file shorter than the offset was rotated in place. What it holds now
	// was written while the reader was looking elsewhere, so the tail says so
	// and a caller counting a rate can decline to count it.
	if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	tail = read(tail.Offset)
	if want := []string{"rotated"}; !slices.Equal(tail.Lines, want) {
		t.Errorf("after rotation read %v, want %v", tail.Lines, want)
	}
	if tail.Offset != 8 || !tail.Rotated {
		t.Errorf("after rotation offset %d rotated=%v, want 8 and true", tail.Offset, tail.Rotated)
	}
	if again := read(tail.Offset); again.Rotated {
		t.Error("a continuing read of a rotated file still reports a rotation")
	}
}

func TestGatewayLogBeforeTheGatewayHasRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	_, err := NewExec("journalctl").GatewayLog(context.Background(), path, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("GatewayLog returned %v, want a not-exist error", err)
	}
}
