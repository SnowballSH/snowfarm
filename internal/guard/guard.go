package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"filippo.io/age"
	"github.com/coreos/go-systemd/v22/daemon"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/discord"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/hygiene"
	"github.com/SnowballSH/snowfarm/internal/journal"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/schedule"
	"github.com/SnowballSH/snowfarm/internal/secrets"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	DefaultConfig       = "/etc/snowfarm/farm.yaml"
	DefaultStateDir     = "/var/lib/snowfarm"
	DefaultSecretsDir   = "/etc/snowfarm/secrets"
	DefaultIdentityPath = "/etc/snowfarm/age.key"
	DefaultPinsPath     = "/etc/snowfarm/pins.yaml"
	DefaultPIDPath      = "/run/snowfarm/guard.pid"

	ledgerFile      = "ledger.db"
	channelsFile    = "channels.json"
	profileHashFile = "profile-hashes.json"
	boardFile       = "kanban.db"
	eventLogDir     = "log"

	channelsMode = fs.FileMode(0o640)
	pidMode      = fs.FileMode(0o644)

	breakerInterval   = 30 * time.Second
	claudeInterval    = 30 * time.Second
	restartInterval   = 30 * time.Second
	modelgateInterval = time.Minute
	healthInterval    = time.Minute
	weeklyInterval    = 7 * 24 * time.Hour
	pruneInterval     = 24 * time.Hour
	eventRetention    = 90 * 24 * time.Hour

	watchdogPeriod = 30 * time.Second

	// metricsBindPoll and metricsBindWait bound the wait for the tailnet
	// address. tailscaled reports its unit started before the interface
	// carries the farm's 100.64.x.y, and the guard unit's start limit is
	// five failures in five minutes, so exiting on the first
	// EADDRNOTAVAIL would leave a cold-booted host with a stopped guard
	// until someone reset it by hand. The bind precedes the Discord
	// connection, so the wait costs no IDENTIFY.
	metricsBindPoll = 2 * time.Second
	metricsBindWait = 2 * time.Minute

	eventBuffer = 256
)

// Config is what the guard needs from outside itself. Every dependency has a
// host default, so a caller that fills only the paths gets the production
// wiring and a test can replace one seam at a time.
type Config struct {
	ConfigPath   string
	StateDir     string
	SecretsDir   string
	IdentityPath string
	SocketPath   string
	PinsPath     string
	PIDPath      string

	// MetricsAddr overrides the roster's own address, which is what lets a
	// test bind an ephemeral port.
	MetricsAddr string

	Units     unitctl.Controller
	Journal   journal.Reader
	NewClient func(token, guildID string) (discord.Client, error)
	HTTP      *http.Client

	// Listen binds the metrics address and Sleep waits between attempts,
	// which is the seam the cold-boot wait is tested through.
	Listen func(network, address string) (net.Listener, error)
	Sleep  func(d time.Duration)

	// Notify delivers a state line to systemd. A guard started outside
	// systemd has no socket and the default is a no-op.
	Notify func(state string) error

	// Signals carries SIGHUP. A nil channel means the caller does not
	// reload this guard.
	Signals <-chan os.Signal

	Now func() time.Time
	Log *slog.Logger
}

// Run is the guard: it builds every component around one roster and serves
// until ctx ends.
func Run(ctx context.Context, cfg Config) error {
	g, err := New(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = g.Close() }()
	return g.Run(ctx)
}

// Guard owns the one shared roster and uid map every component reads through,
// so a reload changes what the dispatch loop, the schedule runner and the
// hygiene sweep do without any of them holding a roster of its own.
type Guard struct {
	cfg Config
	log *slog.Logger

	roster  atomic.Pointer[roster.Roster]
	uids    atomic.Pointer[map[string]int]
	secrets atomic.Pointer[secrets.Store]

	metrics    *metrics.Registry
	listener   net.Listener
	ledger     *dispatch.Ledger
	board      *board.Reader
	client     discord.Client
	poster     *discord.Poster
	store      *discord.LogStore
	logger     *discord.Logger
	loop       *dispatch.Loop
	runner     *schedule.Runner
	sweep      *hygiene.Sweep
	breaker    *Breaker
	restart    *Restarter
	probes     *Probes
	controls   *Controls
	claudeRuns *ClaudeTail
	socket     *secrets.Server
}

// New performs every step that must be able to fail before the guard costs a
// Discord IDENTIFY: the roster, the age files, and the metrics listener. A
// crash-looping guard that connected first would spend a bot's daily session
// budget in an hour and take Discord away for the rest of the day.
func New(cfg Config) (*Guard, error) {
	cfg.withDefaults()
	g := &Guard{cfg: cfg, log: cfg.Log}

	r, err := roster.Load(cfg.ConfigPath)
	if err != nil {
		return nil, err
	}
	g.publish(r)

	store, err := secrets.Load(cfg.IdentityPath, cfg.SecretsDir, agentNames(r))
	if err != nil {
		return nil, err
	}
	g.secrets.Store(store)

	addr := cfg.MetricsAddr
	if addr == "" {
		addr = r.Farm.MetricsAddr
	}
	if addr == "" {
		return nil, errors.New("guard: farm.metrics_addr is empty, so nothing would scrape the farm")
	}
	listener, err := g.listenMetrics(addr)
	if err != nil {
		return nil, err
	}
	g.listener = listener
	g.metrics = metrics.New()

	if err := g.open(r); err != nil {
		return nil, errors.Join(err, g.Close())
	}
	g.assemble(r)
	g.log.Info("guard loaded", "agents", agentNames(r), "metrics", listener.Addr().String())
	return g, nil
}

func (c *Config) withDefaults() {
	defaultPath(&c.ConfigPath, DefaultConfig)
	defaultPath(&c.StateDir, DefaultStateDir)
	defaultPath(&c.SecretsDir, DefaultSecretsDir)
	defaultPath(&c.IdentityPath, DefaultIdentityPath)
	defaultPath(&c.SocketPath, secrets.DefaultSocket)
	defaultPath(&c.PinsPath, DefaultPinsPath)
	defaultPath(&c.PIDPath, DefaultPIDPath)
	if c.Units == nil {
		c.Units = unitctl.NewExec("", "")
	}
	if c.Journal == nil {
		c.Journal = journal.NewExec("")
	}
	if c.NewClient == nil {
		c.NewClient = func(token, guildID string) (discord.Client, error) {
			return discord.NewDisgo(token, guildID)
		}
	}
	if c.HTTP == nil {
		c.HTTP = &http.Client{Timeout: probeTimeout}
	}
	if c.Listen == nil {
		c.Listen = net.Listen
	}
	if c.Sleep == nil {
		c.Sleep = time.Sleep
	}
	if c.Notify == nil {
		c.Notify = sdNotify
	}
	if c.Now == nil {
		c.Now = func() time.Time { return time.Now().UTC() }
	}
	if c.Log == nil {
		c.Log = slog.Default()
	}
}

// listenMetrics binds the address, waiting while the host says it is not
// assigned to any interface. Every other bind failure — a port already in
// use, a malformed address — is a fault the guard exits on at once.
func (g *Guard) listenMetrics(addr string) (net.Listener, error) {
	deadline := g.cfg.Now().Add(metricsBindWait)
	waiting := false
	for {
		listener, err := g.cfg.Listen("tcp", addr)
		switch {
		case err == nil:
			return listener, nil
		case !errors.Is(err, syscall.EADDRNOTAVAIL) || !g.cfg.Now().Before(deadline):
			return nil, fmt.Errorf("guard: listen on %s: %w", addr, err)
		}
		if !waiting {
			waiting = true
			g.log.Warn("the metrics address is not assigned to any interface yet, waiting for it",
				"addr", addr, "within", metricsBindWait)
		}
		g.cfg.Sleep(metricsBindPoll)
	}
}

func defaultPath(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

func (g *Guard) open(r *roster.Roster) error {
	ledger, err := dispatch.OpenLedger(filepath.Join(g.cfg.StateDir, ledgerFile))
	if err != nil {
		return err
	}
	g.ledger = ledger

	reader, err := board.Open(filepath.Join(r.Farm.KanbanHome, boardFile))
	if err != nil {
		return err
	}
	g.board = reader

	vars, ok := g.secretsFor(secrets.SupervisorName)
	if !ok || vars["DISCORD_BOT_TOKEN"] == "" {
		return errors.New("guard: the supervisor's age file carries no DISCORD_BOT_TOKEN")
	}
	client, err := g.cfg.NewClient(vars["DISCORD_BOT_TOKEN"], r.Farm.Discord.GuildID)
	if err != nil {
		return err
	}
	g.client = client

	recipient, err := logRecipient(g.cfg.IdentityPath)
	if err != nil {
		return err
	}
	store, err := discord.NewLogStore(discord.LogStoreConfig{
		Dir:       filepath.Join(g.cfg.StateDir, eventLogDir),
		Recipient: recipient,
		Now:       g.cfg.Now,
	})
	if err != nil {
		return err
	}
	g.store = store
	return nil
}

// assemble wires the components together. Each holds &g.roster and &g.uids
// rather than a roster of its own, so one Store publishes a reload to all of
// them at once.
func (g *Guard) assemble(r *roster.Roster) {
	g.poster = &discord.Poster{Client: g.client}
	g.logger = &discord.Logger{Client: g.client, Store: g.store}

	g.loop = &dispatch.Loop{
		Roster:  &g.roster,
		UIDs:    &g.uids,
		Board:   g.board,
		Units:   g.cfg.Units,
		Journal: g.cfg.Journal,
		Ledger:  g.ledger,
		Post:    g.post,
		PostTo:  g.postTo,
		Metrics: g.metrics,
		Now:     g.cfg.Now,
	}
	g.runner = &schedule.Runner{
		Roster: &g.roster,
		Units:  g.cfg.Units,
		Post:   g.post,
		Now:    g.cfg.Now,
		Log:    g.log,
	}
	g.sweep = &hygiene.Sweep{
		Roster:   &g.roster,
		Board:    g.board,
		Units:    g.cfg.Units,
		Post:     g.post,
		Metrics:  g.metrics,
		HomeRoot: r.Farm.HomeRoot,
		Baseline: filepath.Join(g.cfg.StateDir, profileHashFile),
		Capacity: g.capacity,
	}
	turns := &Turns{Store: g.ledger}
	g.breaker = &Breaker{
		Roster:  &g.roster,
		Units:   g.cfg.Units,
		Journal: g.cfg.Journal,
		Ledger:  g.ledger,
		Client:  g.client,
		Secrets: g.secretsFor,
		Turns:   turns,
		Post:    g.post,
		Metrics: g.metrics,
		Now:     g.cfg.Now,
	}
	g.restart = &Restarter{
		Roster:  &g.roster,
		Units:   g.cfg.Units,
		Ledger:  g.ledger,
		Turns:   turns,
		Post:    g.post,
		Metrics: g.metrics,
		Now:     g.cfg.Now,
		Log:     g.log,
	}
	g.probes = &Probes{
		Roster:   &g.roster,
		Secrets:  g.secretsFor,
		HTTP:     g.cfg.HTTP,
		Board:    g.board,
		Units:    g.cfg.Units,
		Ledger:   g.ledger,
		Paused:   &g.loop.Paused,
		Post:     g.post,
		Metrics:  g.metrics,
		Now:      g.cfg.Now,
		Log:      g.log,
		PinsPath: g.cfg.PinsPath,
	}
	g.controls = &Controls{
		Roster:   &g.roster,
		Board:    g.board,
		Loop:     g.loop,
		Ledger:   g.ledger,
		Breaker:  g.breaker,
		Post:     g.post,
		Now:      g.cfg.Now,
		Log:      g.log,
		Operator: r.Farm.Discord.OperatorUserID,
	}
	g.claudeRuns = &ClaudeTail{
		Roster:  &g.roster,
		Ledger:  g.ledger,
		Post:    g.post,
		Metrics: g.metrics,
		Now:     g.cfg.Now,
		Log:     g.log,
	}
	g.socket = &secrets.Server{
		Path:       g.cfg.SocketPath,
		UIDToAgent: g.agentForUID,
		Log:        newLogWriter(g.log),
	}
	g.socket.Swap(g.secrets.Load())
}

// Run reconciles the guild, then serves every loop until ctx ends. The first
// component to stop takes the rest with it: a guard missing a half is a guard
// systemd should restart.
func (g *Guard) Run(ctx context.Context) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	if err := g.reconcile(ctx); err != nil {
		return err
	}
	events, err := g.client.Events(ctx)
	if err != nil {
		return err
	}
	logged := make(chan discord.Event, eventBuffer)
	controlled := make(chan discord.Event, eventBuffer)

	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		failures []error
	)
	// A component that returns has stopped guarding, so it takes the rest
	// down with it and systemd restarts the whole guard rather than leaving
	// one half of it serving.
	start := func(name string, fn func(context.Context) error) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer cancel()
			err := fn(ctx)
			if err == nil || errors.Is(err, context.Canceled) {
				return
			}
			mu.Lock()
			defer mu.Unlock()
			failures = append(failures, fmt.Errorf("%s: %w", name, err))
		}()
	}

	start("event fanout", func(ctx context.Context) error {
		fanout(ctx, events, logged, controlled)
		return nil
	})
	start("event log", g.store.Run)
	start("event logger", func(ctx context.Context) error { return g.logger.Run(ctx, logged) })
	start("controls", func(ctx context.Context) error { return g.controls.Run(ctx, controlled) })
	start("dispatch", func(ctx context.Context) error {
		return g.every(ctx, "dispatch tick", g.dispatchInterval, g.loop.Tick)
	})
	start("hygiene", func(ctx context.Context) error {
		return g.every(ctx, "hygiene sweep", g.hygieneInterval, g.sweep.Run)
	})
	start("breaker", func(ctx context.Context) error {
		return g.every(ctx, "breaker", fixed(breakerInterval), g.breaker.Tick)
	})
	start("drained restarts", func(ctx context.Context) error {
		return g.every(ctx, "drained restart", fixed(restartInterval), g.restart.Tick)
	})
	start("claude run log", func(ctx context.Context) error {
		return g.every(ctx, "claude run log tail", fixed(claudeInterval), g.claudeRuns.Tick)
	})
	start("modelgate probe", func(ctx context.Context) error {
		return g.every(ctx, "modelgate probe", fixed(modelgateInterval), g.probes.Modelgate)
	})
	start("health", func(ctx context.Context) error {
		return g.every(ctx, "health", fixed(healthInterval), g.probes.Health)
	})
	start("weekly probes", func(ctx context.Context) error {
		return g.every(ctx, "weekly probes", fixed(weeklyInterval), g.probes.Weekly)
	})
	start("event log prune", func(ctx context.Context) error {
		return g.every(ctx, "event log prune", fixed(pruneInterval), func(context.Context) error {
			return g.store.Prune(eventRetention)
		})
	})
	start("secrets socket", g.socket.ListenAndServe)
	start("metrics", g.serveMetrics)
	start("signals", g.signals)
	start("watchdog", g.watchdog)

	if err := g.runner.Start(ctx); err != nil {
		g.log.Error("some schedules did not register", "error", err)
	}
	defer g.runner.Stop()

	if err := g.writePID(); err != nil {
		g.log.Error("the pid file could not be written, so `snowfarm reload` will not find this guard", "error", err)
	}
	defer g.removePID()

	if err := g.cfg.Notify(daemon.SdNotifyReady); err != nil {
		g.log.Error("sd_notify READY failed", "error", err)
	}

	<-ctx.Done()
	wg.Wait()
	mu.Lock()
	defer mu.Unlock()
	return errors.Join(failures...)
}

// Close releases what New opened. It is safe on a half-built guard, which is
// what New hands it when a later step fails.
func (g *Guard) Close() error {
	var errs []error
	if g.store != nil {
		errs = append(errs, g.store.Flush())
	}
	if g.board != nil {
		errs = append(errs, g.board.Close())
	}
	if g.ledger != nil {
		errs = append(errs, g.ledger.Close())
	}
	if g.listener != nil {
		if err := g.listener.Close(); err != nil && !errors.Is(err, net.ErrClosed) {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// MetricsAddr is the address the guard bound, which is the roster's own
// unless the caller overrode it.
func (g *Guard) MetricsAddr() string { return g.listener.Addr().String() }

// reconcile makes the guild match the roster and records the channel ids: the
// poster and the controls address Discord with them, and `snowfarm apply`
// reads them out of channels.json to fill each manager unit's allow-list.
func (g *Guard) reconcile(ctx context.Context) error {
	r := g.roster.Load()
	reconciler := &discord.Reconciler{Client: g.client, Roster: r}
	state, err := reconciler.Reconcile(ctx)
	if err != nil {
		return err
	}
	if err := writeChannels(filepath.Join(g.cfg.StateDir, channelsFile), state.Channels); err != nil {
		return err
	}
	g.poster.StatusChannelID = state.Channels[discord.ChannelFarmStatus]
	g.controls.Channel = state.Channels[discord.ChannelFarmControl]
	g.runner.Channels = state.Channels
	return nil
}

// Reload re-reads the roster and the age files. A farm.yaml that no longer
// validates is refused and the guard keeps the roster it is running on: a
// reload never leaves the guard with no roster.
func (g *Guard) Reload(ctx context.Context) error {
	r, err := roster.Load(g.cfg.ConfigPath)
	if err != nil {
		g.log.Error("the new farm.yaml was refused; the guard is still running the previous roster", "error", err)
		g.post(ctx, fmt.Sprintf("reload refused: %v; the guard is still running the roster it loaded before", err))
		return err
	}
	store, err := secrets.Load(g.cfg.IdentityPath, g.cfg.SecretsDir, agentNames(r))
	if err != nil {
		g.log.Error("the age files were refused; the guard is still running the previous roster", "error", err)
		g.post(ctx, fmt.Sprintf("reload refused: %v; the guard is still running the secrets it loaded before", err))
		return err
	}
	g.publish(r)
	g.secrets.Store(store)
	g.socket.Swap(store)
	g.log.Info("guard reloaded", "agents", agentNames(r))
	return g.runner.Reload(ctx)
}

// publish is the single store of a roster: the loop, the runner, the sweep,
// the breaker and the probes all read these two pointers, so no component can
// be forgotten and none of them races on a half-updated pair.
func (g *Guard) publish(r *roster.Roster) {
	uids := make(map[string]int, len(r.Agents))
	for _, agent := range r.Agents {
		uids[agent.Name] = r.UID(agent)
	}
	g.roster.Store(r)
	g.uids.Store(&uids)
}

func (g *Guard) signals(ctx context.Context) error {
	if g.cfg.Signals == nil {
		<-ctx.Done()
		return nil
	}
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-g.cfg.Signals:
			// A refused reload reports itself and leaves the guard on
			// the roster it is already running.
			_ = g.Reload(ctx)
		}
	}
}

// watchdog answers systemd for as long as the guard's own loops are running.
// A guard started outside systemd is told no interval and pings nothing.
func (g *Guard) watchdog(ctx context.Context) error {
	interval, err := daemon.SdWatchdogEnabled(false)
	if err != nil || interval == 0 {
		<-ctx.Done()
		return nil //nolint:nilerr // a guard outside systemd has no watchdog to answer
	}
	period := min(interval/2, watchdogPeriod)
	ticker := time.NewTicker(period)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := g.cfg.Notify(daemon.SdNotifyWatchdog); err != nil {
				g.log.Error("sd_notify WATCHDOG failed", "error", err)
			}
		}
	}
}

func (g *Guard) serveMetrics(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(g.metrics, promhttp.HandlerOpts{}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdown)
	}()
	if err := server.Serve(g.listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// every runs fn now and then on its interval, re-reading the interval each
// pass so a reload changes it. A pass that fails is logged and the loop goes
// on: one bad tick must not take the guard down.
func (g *Guard) every(ctx context.Context, name string, interval func() time.Duration, fn func(context.Context) error) error {
	for {
		if err := fn(ctx); err != nil && !errors.Is(err, context.Canceled) {
			g.log.Error(name+" failed", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil
		case <-time.After(interval()):
		}
	}
}

func (g *Guard) dispatchInterval() time.Duration {
	return time.Duration(g.roster.Load().Guard.DispatchInterval)
}

func (g *Guard) hygieneInterval() time.Duration {
	return time.Duration(g.roster.Load().Guard.HygieneInterval)
}

func fixed(d time.Duration) func() time.Duration { return func() time.Duration { return d } }

func (g *Guard) capacity() (occupied, capacity int, paused bool) {
	return len(g.loop.Running()), g.roster.Load().Guard.MaxConcurrentRuns, g.loop.Paused.Load()
}

func (g *Guard) secretsFor(agent string) (map[string]string, bool) {
	store := g.secrets.Load()
	if store == nil {
		return nil, false
	}
	return store.For(agent)
}

// agentForUID is what the secrets socket decides with: the peer's uid, mapped
// through the same derivation apply created the account with.
func (g *Guard) agentForUID(uid uint32) (string, bool) {
	uids := g.uids.Load()
	if uids == nil {
		return "", false
	}
	for name, owned := range *uids {
		if uint32(owned) == uid { // #nosec G115 -- a roster uid is a small positive integer
			return name, true
		}
	}
	return "", false
}

func (g *Guard) post(ctx context.Context, text string) {
	if err := g.poster.Status(ctx, text); err != nil {
		g.log.Error("a status post did not reach Discord", "error", err, "text", text)
	}
}

func (g *Guard) postTo(ctx context.Context, channelID, text string) {
	if err := g.poster.To(ctx, channelID, text); err != nil {
		g.log.Error("a post did not reach its thread", "error", err, "channel", channelID)
	}
}

func (g *Guard) writePID() error {
	return os.WriteFile(g.cfg.PIDPath, []byte(strconv.Itoa(os.Getpid())+"\n"), pidMode)
}

func (g *Guard) removePID() {
	if err := os.Remove(g.cfg.PIDPath); err != nil && !errors.Is(err, fs.ErrNotExist) {
		g.log.Error("the pid file could not be removed", "error", err)
	}
}

// fanout gives every consumer the whole stream. One subscription serves them
// all, because a second Events call on a live client doubles every delivery.
func fanout(ctx context.Context, source <-chan discord.Event, targets ...chan discord.Event) {
	defer func() {
		for _, target := range targets {
			close(target)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		case event, open := <-source:
			if !open {
				return
			}
			for _, target := range targets {
				select {
				case target <- event:
				case <-ctx.Done():
					return
				}
			}
		}
	}
}

func agentNames(r *roster.Roster) []string {
	agents := r.EnabledAgents()
	names := make([]string, 0, len(agents))
	for _, agent := range agents {
		names = append(names, agent.Name)
	}
	return names
}

func writeChannels(path string, channels map[string]string) error {
	content, err := json.MarshalIndent(channels, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(path, append(content, '\n'), channelsMode)
}

// logRecipient encrypts the event log to the guard's own age identity, so the
// operator reads a day back with the same key that decrypts the farm's
// secrets and the guard needs no second one.
func logRecipient(identityPath string) (string, error) {
	file, err := os.Open(identityPath) // #nosec G304 -- the path is the guard's own configured identity
	if err != nil {
		return "", err
	}
	defer func() { _ = file.Close() }()
	identities, err := age.ParseIdentities(file)
	if err != nil {
		return "", fmt.Errorf("parse age identity: %w", err)
	}
	for _, identity := range identities {
		if x25519, ok := identity.(*age.X25519Identity); ok {
			return x25519.Recipient().String(), nil
		}
	}
	return "", fmt.Errorf("%s holds no X25519 identity to encrypt the event log to", identityPath)
}

func sdNotify(state string) error {
	_, err := daemon.SdNotify(false, state)
	return err
}

// logWriter carries the secrets socket's per-connection lines, which name
// variables and never values, into the guard's own logger.
type logWriter struct{ log *slog.Logger }

func newLogWriter(log *slog.Logger) *logWriter { return &logWriter{log: log} }

func (w *logWriter) Write(p []byte) (int, error) {
	w.log.Info(strings.TrimSpace(string(p)))
	return len(p), nil
}
