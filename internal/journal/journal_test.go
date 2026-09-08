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
	}
	if !slices.Equal(argv, wantArgv) {
		t.Errorf("journalctl argv %v, want %v", argv, wantArgv)
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
	read := func(offset int64) ([]string, int64) {
		t.Helper()
		lines, next, err := NewExec("journalctl").GatewayLog(context.Background(), path, offset)
		if err != nil {
			t.Fatal(err)
		}
		return lines, next
	}

	write("first\nsecond\n")
	lines, offset := read(0)
	if want := []string{"first", "second"}; !slices.Equal(lines, want) {
		t.Errorf("read %v, want %v", lines, want)
	}
	if offset != 13 {
		t.Errorf("offset %d, want 13", offset)
	}

	if lines, next := read(offset); len(lines) != 0 || next != offset {
		t.Errorf("re-reading returned %v at %d, want nothing at %d", lines, next, offset)
	}

	write("part")
	lines, next := read(offset)
	if len(lines) != 0 || next != offset {
		t.Errorf("a partial line returned %v at %d, want nothing at %d", lines, next, offset)
	}

	write("ial\n")
	lines, offset = read(offset)
	if want := []string{"partial"}; !slices.Equal(lines, want) {
		t.Errorf("read %v, want %v", lines, want)
	}
	if offset != 21 {
		t.Errorf("offset %d, want 21", offset)
	}

	if err := os.WriteFile(path, []byte("rotated\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	lines, offset = read(offset)
	if want := []string{"rotated"}; !slices.Equal(lines, want) {
		t.Errorf("after rotation read %v, want %v", lines, want)
	}
	if offset != 8 {
		t.Errorf("offset %d, want 8", offset)
	}
}

func TestGatewayLogBeforeTheGatewayHasRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gateway.log")
	_, _, err := NewExec("journalctl").GatewayLog(context.Background(), path, 0)
	if !errors.Is(err, os.ErrNotExist) {
		t.Errorf("GatewayLog returned %v, want a not-exist error", err)
	}
}
