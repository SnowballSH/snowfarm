package guard

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"filippo.io/age"

	"github.com/SnowballSH/snowfarm/internal/discord"
)

// testWriter routes the guard's own log into the test's output, where a
// failing wiring test can show what the guard said.
type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

// fixture is a whole guard on temporary state: its own roster, age files,
// board and ledger, a fake guild and a fake user manager.
type fixture struct {
	dir       string
	config    string
	modelgate string
	units     *fakeUnits
	client    *fakeClient
	hup       chan os.Signal
	ready     chan struct{}
	guard     *Guard
	done      chan error
}

func newGuardFixture(t *testing.T, argusEnabled bool) *fixture {
	t.Helper()
	dir := t.TempDir()
	state := filepath.Join(dir, "state")
	board := filepath.Join(dir, "kanban")
	for _, path := range []string{state, board, filepath.Join(dir, "secrets"), filepath.Join(dir, "farm")} {
		if err := os.MkdirAll(path, 0o750); err != nil {
			t.Fatalf("make %s: %v", path, err)
		}
	}
	writeBoardAt(t, filepath.Join(board, boardFile))
	write(t, filepath.Join(state, profileHashFile), "{}")

	gateway := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	t.Cleanup(gateway.Close)

	f := &fixture{
		dir:       dir,
		config:    filepath.Join(dir, "farm.yaml"),
		modelgate: gateway.URL + "/v1",
		units:     newFakeUnits(),
		client:    newFakeClient(),
		hup:       make(chan os.Signal, 1),
		ready:     make(chan struct{}),
		done:      make(chan error, 1),
	}
	f.writeConfig(t, argusEnabled)

	guard, err := New(Config{
		ConfigPath:   f.config,
		StateDir:     state,
		SecretsDir:   filepath.Join(dir, "secrets"),
		IdentityPath: writeAgeFiles(t, filepath.Join(dir, "secrets")),
		SocketPath:   shortSocket(t),
		PinsPath:     filepath.Join(dir, "pins.yaml"),
		PIDPath:      filepath.Join(dir, "guard.pid"),
		MetricsAddr:  "127.0.0.1:0",
		Units:        f.units,
		Journal:      newFakeJournal(dispatchResult()),
		NewClient:    func(string, string) (discord.Client, error) { return f.client, nil },
		HTTP:         &http.Client{Timeout: time.Second},
		Notify:       f.notify,
		Signals:      f.hup,
		Log:          slog.New(slog.NewTextHandler(testWriter{t}, nil)),
	})
	if err != nil {
		t.Fatalf("new guard: %v", err)
	}
	f.guard = guard
	t.Cleanup(func() {
		if err := guard.Close(); err != nil {
			t.Errorf("close guard: %v", err)
		}
	})
	return f
}

// shortSocket keeps the socket inside the 104-byte sun_path a Unix socket
// address allows, which a test directory named after its test can exceed.
func shortSocket(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "sf")
	if err != nil {
		t.Fatalf("make socket dir: %v", err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(dir); err != nil {
			t.Errorf("remove socket dir: %v", err)
		}
	})
	return filepath.Join(dir, "g.sock")
}

func (f *fixture) notify(state string) error {
	if state == "READY=1" {
		select {
		case <-f.ready:
		default:
			close(f.ready)
		}
	}
	return nil
}

// run starts the guard and waits until it has told systemd it is ready.
func (f *fixture) run(t *testing.T) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	go func() { f.done <- f.guard.Run(ctx) }()
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-f.done:
			if err != nil {
				t.Errorf("guard: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("the guard did not stop when its context ended")
		}
	})
	select {
	case <-f.ready:
	case err := <-f.done:
		t.Fatalf("the guard stopped before it was ready: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("the guard never reported ready")
	}
}

// writeConfig lays down the committed fixture with its paths moved under the
// test's directory and the phase gate set: a farm that dispatches fast enough
// for a test to watch a reload take effect.
func (f *fixture) writeConfig(t *testing.T, argusEnabled bool) {
	t.Helper()
	source, err := os.ReadFile(rosterFixture)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	text := string(source)
	for from, to := range map[string]string{
		"home_root: /var/lib/farm":                        "home_root: " + filepath.Join(f.dir, "farm"),
		"kanban_home: /srv/snowfarm/kanban":               "kanban_home: " + filepath.Join(f.dir, "kanban"),
		"claude_dir: /srv/snowfarm/claude":                "claude_dir: " + filepath.Join(f.dir, "claude"),
		"dispatch_interval: 30s":                          "dispatch_interval: 100ms",
		"max_concurrent_runs: 1":                          "max_concurrent_runs: 7",
		"modelgate: {api: https://llm.snowballsh.com/v1}": "modelgate: {api: " + f.modelgate + "}",
	} {
		if !strings.Contains(text, from) {
			t.Fatalf("the roster fixture no longer carries %q", from)
		}
		text = strings.Replace(text, from, to, 1)
	}
	write(t, f.config, setEnabled(t, text, "argus", argusEnabled))
}

// setEnabled flips one agent's phase gate inside its own block of the roster.
func setEnabled(t *testing.T, text, agent string, enabled bool) string {
	t.Helper()
	head := "\n  - name: " + agent + "\n"
	start := strings.Index(text, head)
	if start < 0 {
		t.Fatalf("the roster fixture has no agent %s", agent)
	}
	// A nested list — an agent's mcp_servers — indents its own "- name:"
	// entries, so the next agent is found by the line anchor, not by the
	// bare prefix.
	end := strings.Index(text[start+len(head):], "\n  - name: ")
	if end < 0 {
		end = len(text)
	} else {
		end += start + len(head)
	}
	block := text[start:end]
	if !strings.Contains(block, "enabled: ") {
		t.Fatalf("agent %s carries no enabled flag to set", agent)
	}
	flipped := strings.Replace(block, "enabled: true", "enabled: "+boolText(enabled), 1)
	return text[:start] + strings.Replace(flipped, "enabled: false", "enabled: "+boolText(enabled), 1) + text[end:]
}

func boolText(yes bool) string {
	if yes {
		return "true"
	}
	return "false"
}

// writeAgeFiles encrypts one file per agent to a throwaway identity and
// returns the identity's path, which is also what the event log is encrypted
// to.
func writeAgeFiles(t *testing.T, dir string) string {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	for _, name := range []string{"supervisor", "atlas", "iris", "hestia", "argus", "euclid", "hypatia", "daedalus"} {
		path := filepath.Join(dir, name+".age")
		file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
		if err != nil {
			t.Fatalf("create %s: %v", path, err)
		}
		writer, err := age.Encrypt(file, identity.Recipient())
		if err != nil {
			t.Fatalf("encrypt %s: %v", path, err)
		}
		if _, err := io.WriteString(writer, "DISCORD_BOT_TOKEN=token-for-"+name+"\nFARM_MODELGATE_KEY=key-for-"+name+"\n"); err != nil {
			t.Fatalf("write %s: %v", path, err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close age writer: %v", err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", path, err)
		}
	}
	identityPath := filepath.Join(dir, "age.key")
	write(t, identityPath, identity.String()+"\n")
	return identityPath
}

// Without a served /metrics every snowfarm_* family is registered and never
// scraped: F1's curl and the Prometheus job both get connection-refused.
func TestGuardServesMetrics(t *testing.T) {
	f := newGuardFixture(t, true)
	f.run(t)

	if body := scrape(t, f.guard.MetricsAddr()); !strings.Contains(body, "snowfarm_runs_in_flight") {
		t.Fatalf("the exposition carries no snowfarm_runs_in_flight:\n%s", body)
	}
	// The manager families appear with the health pass, which runs beside
	// the scrape rather than before it.
	waitFor(t, "the manager families", func() bool {
		return strings.Contains(scrape(t, f.guard.MetricsAddr()), "snowfarm_manager_up")
	})
}

func scrape(t *testing.T, addr string) string {
	t.Helper()
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer func() {
		if err := resp.Body.Close(); err != nil {
			t.Errorf("close body: %v", err)
		}
	}()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/metrics answered %s", resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(body)
}

// The reconciled channel ids are what `snowfarm apply` fills each manager
// unit's allow-list from, so the guard must leave them on disk.
func TestGuardWritesTheChannelMap(t *testing.T) {
	f := newGuardFixture(t, true)
	f.run(t)

	data, err := os.ReadFile(filepath.Join(f.dir, "state", channelsFile))
	if err != nil {
		t.Fatalf("read channels: %v", err)
	}
	waitFor(t, "the first status post", func() bool { return len(f.client.sent()) > 0 })
	status := f.guard.poster.StatusChannelID
	for _, post := range f.client.sent() {
		if post.channelID != status {
			t.Fatalf("a status post reached %s, not #farm-status (%s)", post.channelID, status)
		}
	}
	for _, channel := range []string{discord.ChannelFarmStatus, discord.ChannelFarmControl, discord.ChannelManagers} {
		if !strings.Contains(string(data), channel) {
			t.Fatalf("channels.json names no %s: %s", channel, data)
		}
	}
}
