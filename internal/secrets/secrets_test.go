package secrets

import (
	"bytes"
	"context"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"filippo.io/age"
)

const (
	fetchBudget   = time.Second
	readyDeadline = 5 * time.Second
)

func TestLoadDecryptsPerAgent(t *testing.T) {
	identity, dir := encrypted(t, map[string]string{
		"atlas":      "atlas.env",
		"hestia":     "hestia.env",
		"supervisor": "supervisor.env",
	})
	store, err := Load(identity, dir, []string{"atlas", "hestia"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, tc := range []struct {
		agent string
		want  map[string]string
	}{
		{"hestia", map[string]string{
			"FARM_MODELGATE_KEY":        "modelgate-key-for-hestia",
			"CLAUDE_CODE_OAUTH_TOKEN":   "claude-token-for-the-farm",
			"FARM_SNOWBLOG_ADMIN_TOKEN": "snowblog=token=for=hestia",
		}},
		{"atlas", map[string]string{
			"DISCORD_BOT_TOKEN":       "discord-token-for-atlas",
			"CLAUDE_CODE_OAUTH_TOKEN": "claude-token-for-the-farm",
			"FARM_GITHUB_TEAMS_TOKEN": "github-token-for-the-teams",
		}},
		{"supervisor", map[string]string{
			"DISCORD_BOT_TOKEN":  "discord-token-for-the-supervisor",
			"FARM_MODELGATE_KEY": "modelgate-probe-key",
		}},
	} {
		t.Run(tc.agent, func(t *testing.T) {
			got, ok := store.For(tc.agent)
			if !ok {
				t.Fatalf("%s: no secrets loaded", tc.agent)
			}
			if !maps.Equal(got, tc.want) {
				t.Fatalf("%s: got %v keys, want %v keys", tc.agent, sortedKeys(got), sortedKeys(tc.want))
			}
		})
	}
	if _, ok := store.For("daedalus"); ok {
		t.Fatal("a disabled agent was loaded")
	}
}

func TestLoadRequiresEveryNamedFile(t *testing.T) {
	identity, dir := encrypted(t, map[string]string{
		"atlas":      "atlas.env",
		"supervisor": "supervisor.env",
	})
	if _, err := Load(identity, dir, []string{"atlas", "daedalus"}); err == nil {
		t.Fatal("a missing file for an enabled agent must fail the load")
	} else if !strings.Contains(err.Error(), "daedalus") {
		t.Fatalf("the error does not name the agent: %v", err)
	}

	identity, dir = encrypted(t, map[string]string{"atlas": "atlas.env"})
	if _, err := Load(identity, dir, []string{"atlas"}); err == nil {
		t.Fatal("a missing supervisor file must fail the load")
	} else if !strings.Contains(err.Error(), "supervisor") {
		t.Fatalf("the error does not name the supervisor: %v", err)
	}
}

func TestLoadRejectsALineThatIsNotAssignment(t *testing.T) {
	identity, dir := encrypted(t, map[string]string{
		"hestia":     "malformed.env",
		"supervisor": "supervisor.env",
	})
	_, err := Load(identity, dir, []string{"hestia"})
	if err == nil {
		t.Fatal("a line that is not KEY=VALUE must fail the load")
	}
	if !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("the error does not place the fault: %v", err)
	}
	if strings.Contains(err.Error(), "bare-line-carrying-a-secret") {
		t.Fatalf("the error quotes the file's content: %v", err)
	}
}

func TestServerServesOnlyCallersVariables(t *testing.T) {
	store := loadedStore(t)
	want, _ := store.For("hestia")

	t.Run("a mapped caller receives its own variables", func(t *testing.T) {
		srv := serving(t, store, mapsSelfTo("hestia"))
		got := fetchWhenReady(t, srv.Path)
		if !maps.Equal(got, want) {
			t.Fatalf("got %v keys, want %v keys", sortedKeys(got), sortedKeys(want))
		}
	})

	t.Run("an unmapped caller receives nothing", func(t *testing.T) {
		srv := serving(t, store, func(uint32) (string, bool) { return "", false })
		got := fetchWhenReady(t, srv.Path)
		if len(got) != 0 {
			t.Fatalf("an unmapped uid was served %v", sortedKeys(got))
		}
	})

	t.Run("an agent with no secrets loaded receives nothing", func(t *testing.T) {
		srv := serving(t, store, mapsSelfTo("daedalus"))
		got := fetchWhenReady(t, srv.Path)
		if len(got) != 0 {
			t.Fatalf("an unloaded agent was served %v", sortedKeys(got))
		}
	})
}

func TestListenReplacesAStaleSocketAndKeepsItGroupReadable(t *testing.T) {
	path := socketPath(t)
	if err := os.WriteFile(path, []byte("a socket a crash left behind"), 0o600); err != nil {
		t.Fatal(err)
	}
	srv := servingAt(t, path, loadedStore(t), mapsSelfTo("hestia"))
	if len(fetchWhenReady(t, srv.Path)) == 0 {
		t.Fatal("the server did not replace the stale socket")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode()&fs.ModeSocket == 0 {
		t.Fatalf("%s is not a socket: %v", path, info.Mode())
	}
	if perm := info.Mode().Perm(); perm != socketMode {
		t.Fatalf("socket mode %04o, want %04o", perm, socketMode)
	}
}

func TestSwapPublishesTheNewStore(t *testing.T) {
	srv := serving(t, loadedStore(t), mapsSelfTo("atlas"))
	before := fetchWhenReady(t, srv.Path)
	if before["DISCORD_BOT_TOKEN"] != "discord-token-for-atlas" {
		t.Fatalf("before the swap: %v", sortedKeys(before))
	}
	identity, dir := encrypted(t, map[string]string{
		"atlas":      "hestia.env",
		"supervisor": "supervisor.env",
	})
	replacement, err := Load(identity, dir, []string{"atlas"})
	if err != nil {
		t.Fatal(err)
	}
	srv.Swap(replacement)
	after := fetchWhenReady(t, srv.Path)
	if after["FARM_MODELGATE_KEY"] != "modelgate-key-for-hestia" {
		t.Fatalf("after the swap: %v", sortedKeys(after))
	}
}

func TestSecretEnvOutputFormat(t *testing.T) {
	srv := serving(t, loadedStore(t), mapsSelfTo("hestia"))
	fetchWhenReady(t, srv.Path)

	var out bytes.Buffer
	started := time.Now()
	if err := PrintEnv(&out, srv.Path, fetchBudget); err != nil {
		t.Fatalf("print: %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("the helper took %v, Hermes waits three seconds for the whole command", elapsed)
	}
	const want = "CLAUDE_CODE_OAUTH_TOKEN=claude-token-for-the-farm\n" +
		"FARM_MODELGATE_KEY=modelgate-key-for-hestia\n" +
		"FARM_SNOWBLOG_ADMIN_TOKEN=snowblog=token=for=hestia\n"
	if out.String() != want {
		t.Fatalf("secret-env printed:\n%q\nwant:\n%q", out.String(), want)
	}
}

func TestNoValueInLogs(t *testing.T) {
	log := &syncBuffer{}
	srv := &Server{Path: socketPath(t), UIDToAgent: mapsSelfTo("hestia"), Log: log}
	srv.Swap(loadedStore(t))
	start(t, srv)
	got := fetchWhenReady(t, srv.Path)
	if len(got) == 0 {
		t.Fatal("the fetch returned nothing")
	}
	logged := log.String()
	if !strings.Contains(logged, "hestia") {
		t.Fatalf("the log does not name the caller: %q", logged)
	}
	for name := range got {
		if !strings.Contains(logged, name) {
			t.Fatalf("the log does not name %s: %q", name, logged)
		}
	}
	for name, value := range got {
		if strings.Contains(logged, value) {
			t.Fatalf("the log carries the value of %s", name)
		}
	}
}

func encrypted(t *testing.T, files map[string]string) (identityPath, dir string) {
	t.Helper()
	identity, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatalf("generate identity: %v", err)
	}
	identityPath = filepath.Join(t.TempDir(), "age.key")
	if err := os.WriteFile(identityPath, []byte(identity.String()+"\n"), 0o600); err != nil {
		t.Fatalf("write identity: %v", err)
	}
	dir = t.TempDir()
	for name, fixture := range files {
		plain, err := os.ReadFile(filepath.Join("testdata", fixture))
		if err != nil {
			t.Fatalf("read %s: %v", fixture, err)
		}
		file, err := os.OpenFile(filepath.Join(dir, name+fileSuffix), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		writer, err := age.Encrypt(file, identity.Recipient())
		if err != nil {
			t.Fatalf("encrypt %s: %v", name, err)
		}
		if _, err := writer.Write(plain); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		if err := writer.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
		if err := file.Close(); err != nil {
			t.Fatalf("close %s: %v", name, err)
		}
	}
	return identityPath, dir
}

func loadedStore(t *testing.T) *Store {
	t.Helper()
	identity, dir := encrypted(t, map[string]string{
		"atlas":      "atlas.env",
		"hestia":     "hestia.env",
		"supervisor": "supervisor.env",
	})
	store, err := Load(identity, dir, []string{"atlas", "hestia"})
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	return store
}

func serving(t *testing.T, store *Store, uidToAgent func(uint32) (string, bool)) *Server {
	t.Helper()
	return servingAt(t, socketPath(t), store, uidToAgent)
}

func servingAt(t *testing.T, path string, store *Store, uidToAgent func(uint32) (string, bool)) *Server {
	t.Helper()
	srv := &Server{Path: path, UIDToAgent: uidToAgent}
	srv.Swap(store)
	start(t, srv)
	return srv
}

func start(t *testing.T, srv *Server) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	served := make(chan error, 1)
	go func() { served <- srv.ListenAndServe(ctx) }()
	t.Cleanup(func() {
		cancel()
		if err := <-served; err != nil {
			t.Errorf("serve %s: %v", srv.Path, err)
		}
	})
}

// socketPath keeps a test socket inside sun_path, which a t.TempDir named
// after the test does not on macOS.
func socketPath(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "snowfarm")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return filepath.Join(dir, "guard.sock")
}

func mapsSelfTo(agent string) func(uint32) (string, bool) {
	self := uint32(os.Getuid()) //nolint:gosec // a uid from the running process is never negative
	return func(uid uint32) (string, bool) {
		if uid != self {
			return "", false
		}
		return agent, true
	}
}

func fetchWhenReady(t *testing.T, path string) map[string]string {
	t.Helper()
	deadline := time.Now().Add(readyDeadline)
	for {
		vars, err := Fetch(path, fetchBudget)
		if err == nil {
			return vars
		}
		if time.Now().After(deadline) {
			t.Fatalf("fetch %s: %v", path, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func sortedKeys(vars map[string]string) []string {
	return slices.Sorted(maps.Keys(vars))
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}
