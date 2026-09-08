package guard

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

type probeEnv struct {
	roster *atomic.Pointer[roster.Roster]
	units  *fakeUnits
	posts  *recorder
	paused *atomic.Bool
	reg    *metrics.Registry
	clock  *clock
}

func newProbes(t *testing.T, cards ...board.RunningCard) (*Probes, *probeEnv) {
	t.Helper()
	env := &probeEnv{
		roster: held(testRoster(t, t.TempDir())),
		units:  newFakeUnits(),
		posts:  &recorder{},
		paused: &atomic.Bool{},
		reg:    metrics.New(),
		clock:  newClock(time.Date(2026, 9, 20, 12, 0, 0, 0, time.UTC)),
	}
	probes := &Probes{
		Roster: env.roster,
		Secrets: func(string) (map[string]string, bool) {
			return map[string]string{"FARM_MODELGATE_KEY": "probe-key"}, true
		},
		HTTP:    &http.Client{Timeout: 5 * time.Second},
		Board:   openBoard(t, cards...),
		Units:   env.units,
		Ledger:  openLedger(t),
		Paused:  env.paused,
		Post:    env.posts.post,
		Metrics: env.reg,
		Now:     env.clock.now,
	}
	return probes, env
}

// A model gateway that cannot answer means a run started now would burn a
// failure without inference, so dispatch pauses until it recovers — and says
// so once in each direction, not on every sixty-second probe.
func TestModelgateProbePausesDispatch(t *testing.T) {
	var healthy atomic.Bool
	healthy.Store(true)
	var asked atomic.Int64
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked.Add(1)
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe asked for %s, want /v1/models", r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer probe-key" {
			t.Errorf("probe carried %q, want the supervisor's own key", got)
		}
		if !healthy.Load() {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		if _, err := w.Write([]byte(`{"data":[]}`)); err != nil {
			t.Errorf("write: %v", err)
		}
	}))
	defer server.Close()

	probes, env := newProbes(t)
	r := env.roster.Load()
	r.Farm.Modelgate.API = server.URL + "/v1"
	env.roster.Store(r)

	if err := probes.Modelgate(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if env.paused.Load() {
		t.Fatal("a reachable gateway paused dispatch")
	}
	if got := testutil.ToFloat64(env.reg.ModelgateReachable); got != 1 {
		t.Fatalf("modelgate_reachable is %v while the gateway answered", got)
	}
	if posts := env.posts.all(); len(posts) != 0 {
		t.Fatalf("a healthy first probe posted %v", posts)
	}

	healthy.Store(false)
	for range 3 {
		if err := probes.Modelgate(context.Background()); err != nil {
			t.Fatalf("probe: %v", err)
		}
	}
	if !env.paused.Load() {
		t.Fatal("an unreachable gateway left dispatch running")
	}
	if got := testutil.ToFloat64(env.reg.ModelgateReachable); got != 0 {
		t.Fatalf("modelgate_reachable is %v while the gateway refused", got)
	}
	if posts := env.posts.all(); len(posts) != 1 {
		t.Fatalf("three failed probes posted %v", posts)
	}

	healthy.Store(true)
	for range 2 {
		if err := probes.Modelgate(context.Background()); err != nil {
			t.Fatalf("probe: %v", err)
		}
	}
	if env.paused.Load() {
		t.Fatal("dispatch stayed paused after the gateway recovered")
	}
	if posts := env.posts.all(); len(posts) != 2 {
		t.Fatalf("recovery posted %v", posts)
	}
	if asked.Load() != 6 {
		t.Fatalf("the gateway was asked %d times, want one request per probe", asked.Load())
	}
}

// A gateway the guard cannot reach at all is the same signal as one that
// refuses: the farm must not start runs into it.
func TestModelgateProbeHandlesATransportFailure(t *testing.T) {
	probes, env := newProbes(t)
	r := env.roster.Load()
	r.Farm.Modelgate.API = "http://127.0.0.1:1/v1"
	env.roster.Store(r)

	if err := probes.Modelgate(context.Background()); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if !env.paused.Load() {
		t.Fatal("an unreachable address left dispatch running")
	}
}

func TestManagerHealthSeparatesDownFromNotManaged(t *testing.T) {
	probes, env := newProbes(t)
	env.units.inactive["iris"] = true
	env.units.show["iris"] = "LoadState=loaded\nUnitFileState=enabled\nEnvironment=DISCORD_ALLOWED_CHANNELS=pending-reconcile\n"

	if err := probes.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	for _, want := range []struct {
		agent       string
		up, managed float64
	}{
		{agent: "atlas", up: 1, managed: 1},
		{agent: "iris", up: 0, managed: 0},
	} {
		up := testutil.ToFloat64(env.reg.ManagerUp.WithLabelValues(want.agent))
		managed := testutil.ToFloat64(env.reg.ManagerManaged.WithLabelValues(want.agent))
		if up != want.up || managed != want.managed {
			t.Fatalf("%s: up=%v managed=%v, want up=%v managed=%v", want.agent, up, managed, want.up, want.managed)
		}
	}
}

// A paused manager is down on purpose, and the alert that fires on a manager
// that is down reads exactly this gauge to tell the two apart.
func TestPausedManagerIsNotManaged(t *testing.T) {
	probes, env := newProbes(t)
	if err := probes.Ledger.RecordPause("atlas", pauseOperator, env.clock.now(), time.Time{}); err != nil {
		t.Fatalf("record pause: %v", err)
	}
	env.units.inactive["atlas"] = true

	if err := probes.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if got := testutil.ToFloat64(env.reg.ManagerManaged.WithLabelValues("atlas")); got != 0 {
		t.Fatalf("a paused manager reports managed=%v", got)
	}
}

func TestHealthExportsBoardSizeAndKeyAge(t *testing.T) {
	probes, env := newProbes(t, board.RunningCard{
		ID: "t_1", Assignee: "hestia", StartedAt: time.Date(2026, 9, 20, 11, 0, 0, 0, time.UTC),
	})
	if err := probes.Health(context.Background()); err != nil {
		t.Fatalf("health: %v", err)
	}
	if got := testutil.ToFloat64(env.reg.BoardBytes.WithLabelValues("db")); got <= 0 {
		t.Fatalf("board_bytes{db} is %v", got)
	}
	// The fixture mints atlas' key on 2026-09-10, ten days and twelve hours
	// before the clock.
	if got := testutil.ToFloat64(env.reg.ModelgateKeyAgeSeconds.WithLabelValues("atlas")); got != (10*24+12)*3600 {
		t.Fatalf("modelgate_key_age_seconds{atlas} is %v", got)
	}
	if got := testutil.ToFloat64(env.reg.ModelgateKeyAgeSeconds.WithLabelValues("iris")); got != 0 {
		t.Fatalf("an unminted key reports an age of %v", got)
	}
}

func writeHermes(t *testing.T, output string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hermes")
	script := "#!/bin/sh\nprintf '%s\\n' " + "'" + output + "'\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatalf("write fake hermes: %v", err)
	}
	return path
}

func writePins(t *testing.T, commit string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pins.yaml")
	if err := os.WriteFile(path, []byte("hermes:\n  commit: "+commit+"\n"), 0o600); err != nil {
		t.Fatalf("write pins: %v", err)
	}
	return path
}

func TestPinDriftComparesTheInstalledHermes(t *testing.T) {
	const commit = "f6234d0a1b2c3d4e5f60718293a4b5c6d7e8f900"
	for _, tc := range []struct {
		name   string
		output string
		drift  float64
	}{
		{name: "the pinned commit", output: "hermes 2026.8.31 (" + commit + ")", drift: 0},
		{name: "another commit", output: "hermes 2026.9.02 (0123456789abcdef0123456789abcdef01234567)", drift: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			probes, env := newProbes(t)
			probes.PinsPath = writePins(t, commit)
			r := env.roster.Load()
			r.Farm.HermesBin = writeHermes(t, tc.output)
			env.roster.Store(r)

			if err := probes.Weekly(context.Background()); err != nil {
				t.Fatalf("weekly: %v", err)
			}
			if got := testutil.ToFloat64(env.reg.PinDrift); got != tc.drift {
				t.Fatalf("pin_drift is %v, want %v", got, tc.drift)
			}
			if tc.drift == 1 && len(env.posts.matching("pin")) != 1 {
				t.Fatalf("drift posted %v", env.posts.all())
			}
		})
	}
}

// The refresh grant is the only check that says a stored refresh token still
// works; an access token in the same file expires hourly and would report a
// healthy token as broken every week.
func TestGoogleTokenProbe(t *testing.T) {
	for _, tc := range []struct {
		name    string
		status  int
		healthy float64
	}{
		{name: "a token Google still honours", status: http.StatusOK, healthy: 1},
		{name: "a revoked token", status: http.StatusBadRequest, healthy: 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
				}
				if got := r.PostForm.Get("grant_type"); got != "refresh_token" {
					t.Errorf("grant_type %q", got)
				}
				if got := r.PostForm.Get("refresh_token"); got != "the-refresh-token" {
					t.Errorf("refresh_token %q", got)
				}
				w.WriteHeader(tc.status)
			}))
			defer server.Close()

			probes, env := newProbes(t)
			dir := t.TempDir()
			probes.GoogleClientPath = filepath.Join(dir, "client.json")
			probes.GoogleTokenPath = filepath.Join(dir, "tokens.json")
			probes.GoogleTokenURL = server.URL
			write(t, probes.GoogleClientPath, `{"installed":{"client_id":"an-id","client_secret":"a-secret"}}`)
			write(t, probes.GoogleTokenPath, `{"accounts":{"primary":{"access_token":"expired","refresh_token":"the-refresh-token"}}}`)

			if err := probes.Weekly(context.Background()); err != nil {
				t.Fatalf("weekly: %v", err)
			}
			if got := testutil.ToFloat64(env.reg.GoogleTokenHealthy); got != tc.healthy {
				t.Fatalf("google_token_healthy is %v, want %v", got, tc.healthy)
			}
			for _, post := range env.posts.all() {
				if strings.Contains(post, "the-refresh-token") {
					t.Fatalf("a post carried the token: %q", post)
				}
			}
		})
	}
}

// Before F3 places the token there is nothing to check, and a farm without
// Google must not report a broken one. Zero is the value A5 alerts on, so
// "not checked" may not be reported as zero — from a fresh registry, whose
// gauge would otherwise sit at zero forever, or from a weekly pass that found
// no file to read.
func TestGoogleTokenProbeIsQuietWithoutAToken(t *testing.T) {
	probes, env := newProbes(t)
	dir := t.TempDir()
	probes.GoogleClientPath = filepath.Join(dir, "client.json")
	probes.GoogleTokenPath = filepath.Join(dir, "tokens.json")

	if got := testutil.ToFloat64(metrics.New().GoogleTokenHealthy); got == googleRefused {
		t.Fatalf("a registry reports google_token_healthy %v before any probe has run", got)
	}
	if err := probes.Weekly(context.Background()); err != nil {
		t.Fatalf("weekly: %v", err)
	}
	if posts := env.posts.all(); len(posts) != 0 {
		t.Fatalf("a farm with no Google token posted %v", posts)
	}
	if got := testutil.ToFloat64(env.reg.GoogleTokenHealthy); got == googleRefused {
		t.Fatalf("google_token_healthy is %v with no token to check, which is the value a refusal reports", got)
	}

	write(t, probes.GoogleTokenPath, `{"accounts":{"primary":{"access_token":"expired"}}}`)
	if err := probes.Weekly(context.Background()); err != nil {
		t.Fatalf("weekly over a token file with no refresh token: %v", err)
	}
	if got := testutil.ToFloat64(env.reg.GoogleTokenHealthy); got == googleRefused {
		t.Fatalf("a token file with no refresh token reported %v, the value a refusal reports", got)
	}
}

func write(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
