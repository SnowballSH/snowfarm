package claude

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

const token = "the-subscription-token"

type harness struct {
	t        *testing.T
	dir      string
	bin      string
	env      []string
	now      time.Time
	stdout   bytes.Buffer
	stderr   bytes.Buffer
	argvPath string
	envPath  string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	dir := t.TempDir()
	record := filepath.Join(dir, "record")
	for _, sub := range []string{"shared/slots", "runs", "home/.claude", "record"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o750); err != nil {
			t.Fatal(err)
		}
	}
	for _, slot := range []string{"1", "2"} {
		writeFile(t, filepath.Join(dir, "shared", "slots", slot), "")
	}
	writeFile(t, filepath.Join(dir, "runs", "hestia.jsonl"), "")

	bin, err := filepath.Abs(filepath.Join("testdata", "fakeclaude", "claude"))
	if err != nil {
		t.Fatal(err)
	}
	h := &harness{
		t:        t,
		dir:      dir,
		bin:      bin,
		now:      time.Date(2026, 9, 7, 9, 30, 0, 0, time.UTC),
		argvPath: filepath.Join(record, "argv"),
		envPath:  filepath.Join(record, "env"),
	}
	h.env = []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(dir, "home"),
		"CLAUDE_CODE_OAUTH_TOKEN=" + token,
		"FAKE_CLAUDE_MODE=ok",
		"FAKE_CLAUDE_ARGV=" + h.argvPath,
		"FAKE_CLAUDE_ENV=" + h.envPath,
	}
	return h
}

func (h *harness) config() Config {
	return Config{
		Dir:         h.dir,
		Bin:         h.bin,
		ConfigDir:   filepath.Join(h.dir, "home", ".claude"),
		Agent:       "hestia",
		SlotTimeout: 200 * time.Millisecond,
		SlotPoll:    20 * time.Millisecond,
	}
}

func (h *harness) run(args ...string) int {
	h.t.Helper()
	w := Wrapper{
		Config: h.config(),
		Env:    slices.Clone(h.env),
		Stdout: &h.stdout,
		Stderr: &h.stderr,
		Now:    func() time.Time { return h.now },
	}
	return w.Run(args)
}

func (h *harness) setEnv(key, value string) {
	h.env = slices.DeleteFunc(h.env, func(entry string) bool {
		name, _, _ := strings.Cut(entry, "=")
		return name == key
	})
	h.env = append(h.env, key+"="+value)
}

func (h *harness) reset() {
	h.stdout.Reset()
	h.stderr.Reset()
	if err := os.Remove(h.argvPath); err != nil && !os.IsNotExist(err) {
		h.t.Fatal(err)
	}
}

func (h *harness) ranClaude() bool {
	_, err := os.Stat(h.argvPath)
	return err == nil
}

func (h *harness) argv() []string {
	h.t.Helper()
	return strings.Split(strings.TrimSuffix(readFile(h.t, h.argvPath), "\n"), "\n")
}

func (h *harness) childEnv() []string {
	h.t.Helper()
	return strings.Split(strings.TrimSuffix(readFile(h.t, h.envPath), "\n"), "\n")
}

func (h *harness) runsFile(t *testing.T) string {
	t.Helper()
	return readFile(t, filepath.Join(h.dir, "runs", "hestia.jsonl"))
}

func (h *harness) records() []map[string]any {
	h.t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(h.runsFile(h.t)), "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			h.t.Fatalf("run record %q: %v", line, err)
		}
		out = append(out, record)
	}
	return out
}

func (h *harness) record(t *testing.T) map[string]any {
	t.Helper()
	records := h.records()
	if len(records) != 1 {
		t.Fatalf("%d run records, want one: %v", len(records), records)
	}
	return records[0]
}

func (h *harness) writeMarker(at time.Time) {
	h.t.Helper()
	writeFile(h.t, filepath.Join(h.dir, "shared", "limit-until"), at.Format(time.RFC3339)+"\n")
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o640); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestRunRefusesBeforeExec(t *testing.T) {
	for _, tc := range []struct {
		name  string
		args  []string
		strip string
	}{
		{name: "a containment override", args: []string{"-p", "x", "--permission-mode", "bypassPermissions"}},
		{name: "no prompt", args: []string{"--model", "claude-opus-5"}},
		{name: "no token", args: []string{"-p", "x"}, strip: "CLAUDE_CODE_OAUTH_TOKEN"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if tc.strip != "" {
				h.env = slices.DeleteFunc(h.env, func(entry string) bool {
					name, _, _ := strings.Cut(entry, "=")
					return name == tc.strip
				})
			}
			if code := h.run(tc.args...); code != ExitError {
				t.Fatalf("exit %d, want %d", code, ExitError)
			}
			if h.ranClaude() {
				t.Error("the wrapper execed claude anyway")
			}
			if h.stderr.Len() == 0 {
				t.Error("nothing was reported on stderr")
			}
		})
	}
}

func TestRunPassesTheCallerArgv(t *testing.T) {
	h := newHarness(t)
	if code := h.run("-p", "print the git HEAD", "--add-dir", "/srv/snowfarm/kanban/kanban/workspaces/card-17"); code != ExitOK {
		t.Fatalf("exit %d, want 0 (stderr: %s)", code, h.stderr.String())
	}
	opts, err := ParseArgs([]string{"-p", "print the git HEAD", "--add-dir", "/srv/snowfarm/kanban/kanban/workspaces/card-17"})
	if err != nil {
		t.Fatal(err)
	}
	want, err := Argv(h.bin, opts)
	if err != nil {
		t.Fatal(err)
	}
	if got := h.argv(); !slices.Equal(got, want[1:]) {
		t.Fatalf("argv\n got %q\nwant %q", got, want[1:])
	}
	if _, ok := envLookup(h.childEnv(), "ANTHROPIC_API_KEY"); ok {
		t.Error("ANTHROPIC_API_KEY reached the child")
	}
	if got, ok := envLookup(h.childEnv(), "CLAUDE_CONFIG_DIR"); !ok || got != h.config().ConfigDir {
		t.Errorf("CLAUDE_CONFIG_DIR = %q (present %v), want %q", got, ok, h.config().ConfigDir)
	}
}

func TestRunWithoutAResultObject(t *testing.T) {
	silent, err := exec.LookPath("true")
	if err != nil {
		t.Skipf("no true(1) on this machine: %v", err)
	}
	h := newHarness(t)
	w := Wrapper{
		Config: Config{
			Dir: h.dir, Bin: silent, ConfigDir: h.config().ConfigDir, Agent: "hestia",
			SlotTimeout: 200 * time.Millisecond, SlotPoll: 20 * time.Millisecond,
		},
		Env: h.env, Stdout: &h.stdout, Stderr: &h.stderr, Now: func() time.Time { return h.now },
	}
	if code := w.Run([]string{"-p", "print the git HEAD"}); code != ExitError {
		t.Fatalf("exit %d for a run that said nothing, want %d", code, ExitError)
	}
	if record := h.record(t); record["is_error"] != true {
		t.Errorf("record %v, want is_error true", record)
	}
}
