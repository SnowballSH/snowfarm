package guard

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	yaml "go.yaml.in/yaml/v3"

	"github.com/SnowballSH/snowfarm/internal/board"
	"github.com/SnowballSH/snowfarm/internal/dispatch"
	"github.com/SnowballSH/snowfarm/internal/metrics"
	"github.com/SnowballSH/snowfarm/internal/roster"
	"github.com/SnowballSH/snowfarm/internal/secrets"
	"github.com/SnowballSH/snowfarm/internal/unitctl"
)

const (
	// GoogleTokenEndpoint is where a refresh grant proves a stored refresh
	// token still works. An access token from the same file expires within
	// the hour, so checking one would report every healthy token as broken.
	GoogleTokenEndpoint = "https://oauth2.googleapis.com/token"

	probeTimeout = 20 * time.Second

	// pendingChannels is what a manager unit carries until the reconciler
	// has resolved its channel ids; apply installs such a unit and does not
	// start it, so the farm is not managing that manager yet.
	pendingChannels = "pending-reconcile"

	boardFileDB  = "db"
	boardFileWAL = "wal"

	noticePinDrift = "pin_drift"
)

// Probes are the guard's outward checks: the model gateway every minute, the
// manager units and the board's size on the health pass, and the Google
// refresh token and the Hermes pin once a week.
type Probes struct {
	Roster  *atomic.Pointer[roster.Roster]
	Secrets func(agent string) (map[string]string, bool)
	HTTP    *http.Client
	Board   *board.Reader
	Units   unitctl.Controller
	Ledger  *dispatch.Ledger

	// Paused is the dispatch loop's own flag: no run starts while the model
	// gateway is unreachable, because a run without inference only spends a
	// card's failure count.
	Paused *atomic.Bool

	Post    func(ctx context.Context, text string)
	Metrics *metrics.Registry
	Now     func() time.Time
	Log     *slog.Logger

	PinsPath         string
	GoogleClientPath string
	GoogleTokenPath  string
	GoogleTokenURL   string

	mu          sync.Mutex
	unreachable bool
}

func (p *Probes) Modelgate(ctx context.Context) error {
	r := p.Roster.Load()
	if r == nil {
		return errors.New("probes: the guard holds no roster")
	}
	if r.Farm.Modelgate.API == "" {
		return nil
	}
	p.reportModelgate(ctx, p.askModelgate(ctx, r))
	return nil
}

func (p *Probes) askModelgate(ctx context.Context, r *roster.Roster) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	endpoint := strings.TrimSuffix(r.Farm.Modelgate.API, "/") + "/models"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return err
	}
	if vars, ok := p.secrets(secrets.SupervisorName); ok {
		if key := vars["FARM_MODELGATE_KEY"]; key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
	}
	resp, err := p.client().Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	if resp.StatusCode >= http.StatusBadRequest {
		return fmt.Errorf("%s answered %s", endpoint, resp.Status)
	}
	return nil
}

// reportModelgate posts on each edge only: the probe runs every minute, and an
// outage that posted every pass would bury everything else in #farm-status.
func (p *Probes) reportModelgate(ctx context.Context, err error) {
	p.mu.Lock()
	was := p.unreachable
	p.unreachable = err != nil
	p.mu.Unlock()

	if err != nil {
		p.Metrics.ModelgateReachable.Set(0)
		p.Paused.Store(true)
		if !was {
			p.post(ctx, fmt.Sprintf("the model gateway is unreachable, so dispatch is paused until it answers again: %v", err))
		}
		return
	}
	p.Metrics.ModelgateReachable.Set(1)
	p.Paused.Store(false)
	if was {
		p.post(ctx, "the model gateway answered again; dispatch has resumed")
	}
}

// Health publishes what the guard can see of the farm without leaving the
// host: each manager's unit, the board's size, and how old each agent's
// modelgate key is.
func (p *Probes) Health(ctx context.Context) error {
	r := p.Roster.Load()
	if r == nil {
		return errors.New("probes: the guard holds no roster")
	}
	// A manager a phase has since disabled must not keep reporting: the
	// alert on a manager that is down reads exactly these two series.
	p.Metrics.ManagerUp.Reset()
	p.Metrics.ManagerManaged.Reset()
	var errs []error
	for _, manager := range r.EnabledManagers() {
		managed, err := p.managed(ctx, manager.Name)
		if err != nil {
			errs = append(errs, err)
		}
		p.Metrics.ManagerUp.WithLabelValues(manager.Name).Set(boolean(p.active(ctx, manager.Name)))
		p.Metrics.ManagerManaged.WithLabelValues(manager.Name).Set(boolean(managed))
	}
	errs = append(errs, p.boardSize(), p.keyAges(r))
	return errors.Join(errs...)
}

// active reads the unit's own state. systemctl is-active exits non-zero for
// an inactive unit, so the state it printed decides and the exit status is
// not an error to report.
func (p *Probes) active(ctx context.Context, agent string) bool {
	out, err := p.Units.Gateway(ctx, agent, "is-active")
	state := strings.TrimSpace(string(out))
	if state == "" && err != nil {
		p.log().Warn("a manager's gateway state could not be read", "agent", agent, "error", err)
		return false
	}
	return state == "active"
}

// managed is what the farm is supposed to be running: an installed unit whose
// channel ids apply has resolved, and which the guard has not paused. It is
// what separates "the operator has not started this manager yet" from "it is
// meant to be up and is not".
func (p *Probes) managed(ctx context.Context, agent string) (bool, error) {
	_, paused, err := p.Ledger.Paused(agent)
	if err != nil {
		return false, err
	}
	if paused {
		return false, nil
	}
	out, err := p.Units.Gateway(ctx, agent, "show")
	if err != nil {
		return false, fmt.Errorf("read %s's gateway unit: %w", agent, err)
	}
	unit := string(out)
	return strings.Contains(unit, "LoadState=loaded") && !strings.Contains(unit, pendingChannels), nil
}

func (p *Probes) boardSize() error {
	db, wal, err := p.Board.Size()
	if err != nil {
		return err
	}
	p.Metrics.BoardBytes.WithLabelValues(boardFileDB).Set(float64(db))
	p.Metrics.BoardBytes.WithLabelValues(boardFileWAL).Set(float64(wal))
	return nil
}

// keyAges reads the mint dates the roster records. An agent whose key the
// operator has not minted yet reports no age rather than one counted from the
// zero date, which would alert on expiry the moment the agent is declared.
func (p *Probes) keyAges(r *roster.Roster) error {
	var errs []error
	for _, agent := range r.EnabledAgents() {
		if agent.ModelgateKeyMinted == "" {
			p.Metrics.ModelgateKeyAgeSeconds.WithLabelValues(agent.Name).Set(0)
			continue
		}
		minted, err := time.ParseInLocation(time.DateOnly, agent.ModelgateKeyMinted, time.UTC)
		if err != nil {
			errs = append(errs, fmt.Errorf("agent %s: modelgate_key_minted: %w", agent.Name, err))
			continue
		}
		p.Metrics.ModelgateKeyAgeSeconds.WithLabelValues(agent.Name).Set(p.now().Sub(minted).Seconds())
	}
	return errors.Join(errs...)
}

// Weekly is the slow pass: the credentials and pins that change on a human
// timescale.
func (p *Probes) Weekly(ctx context.Context) error {
	return errors.Join(p.googleToken(ctx), p.pinDrift(ctx))
}

func (p *Probes) googleToken(ctx context.Context) error {
	client, token := p.googlePaths()
	if token == "" || client == "" {
		return nil
	}
	refresh, err := readRefreshToken(token)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && refresh == "") {
		return nil
	}
	if err != nil {
		return err
	}
	id, secret, err := readGoogleClient(client)
	if err != nil {
		return err
	}
	healthy, err := p.refreshGrant(ctx, id, secret, refresh)
	if err != nil {
		return err
	}
	p.Metrics.GoogleTokenHealthy.Set(boolean(healthy))
	if !healthy {
		p.post(ctx, "the Google refresh token was refused: hestia's calendar tools will fail until the operator repeats the consent ceremony")
	}
	return nil
}

// googlePaths takes the two files from the roster itself: the calendar MCP
// server declares both in its own environment, so the probe checks whatever
// that server is configured to read rather than a path repeated here. The
// configured fields override them, which is what a test sets.
func (p *Probes) googlePaths() (client, token string) {
	client, token = p.GoogleClientPath, p.GoogleTokenPath
	if client != "" && token != "" {
		return client, token
	}
	r := p.Roster.Load()
	if r == nil {
		return client, token
	}
	for _, agent := range r.EnabledAgents() {
		for _, server := range agent.MCPServers {
			if client == "" {
				client = server.Env["GOOGLE_OAUTH_CREDENTIALS"]
			}
			if token == "" {
				token = server.Env["GOOGLE_CALENDAR_MCP_TOKEN_PATH"]
			}
		}
	}
	return client, token
}

// refreshGrant reports whether Google still honours the refresh token. A
// transport failure is not an answer about the token, so it is reported to
// the caller and leaves the gauge as it was.
func (p *Probes) refreshGrant(ctx context.Context, id, secret, refresh string) (bool, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	form := url.Values{
		"client_id":     {id},
		"client_secret": {secret},
		"refresh_token": {refresh},
		"grant_type":    {"refresh_token"},
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.tokenURL(), strings.NewReader(form.Encode()))
	if err != nil {
		return false, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := p.client().Do(req)
	if err != nil {
		return false, err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode < http.StatusBadRequest, nil
}

func (p *Probes) pinDrift(ctx context.Context) error {
	if p.PinsPath == "" {
		return nil
	}
	r := p.Roster.Load()
	if r == nil {
		return errors.New("probes: the guard holds no roster")
	}
	commit, err := pinnedCommit(p.PinsPath)
	if errors.Is(err, fs.ErrNotExist) || (err == nil && commit == "") {
		return nil
	}
	if err != nil {
		return err
	}
	installed, err := hermesVersion(ctx, r.Farm.HermesBin)
	if err != nil {
		return err
	}
	if carriesCommit(installed, commit) {
		p.Metrics.PinDrift.Set(0)
		return nil
	}
	p.Metrics.PinDrift.Set(1)
	first, err := p.Ledger.NoticeOnce(noticePinDrift, commit+"/"+installed, p.now())
	if err != nil || !first {
		return err
	}
	p.post(ctx, fmt.Sprintf(
		"the installed Hermes reports %q, which does not carry the pinned commit %s; the farm is running something the pin does not describe",
		installed, commit))
	return nil
}

func hermesVersion(ctx context.Context, bin string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "version").Output() // #nosec G204 -- the path is the roster's own hermes_bin
	if err != nil {
		return "", fmt.Errorf("%s version: %w", bin, err)
	}
	return strings.TrimSpace(string(out)), nil
}

// carriesCommit accepts the abbreviations a version string may print instead
// of the whole hash.
func carriesCommit(version, commit string) bool {
	if strings.Contains(version, commit) {
		return true
	}
	return len(commit) >= 12 && strings.Contains(version, commit[:12])
}

func pinnedCommit(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the guard's own configured pins file
	if err != nil {
		return "", err
	}
	var pins struct {
		Hermes struct {
			Commit string `yaml:"commit"`
		} `yaml:"hermes"`
	}
	if err := yaml.Unmarshal(data, &pins); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return pins.Hermes.Commit, nil
}

// readRefreshToken finds the refresh token wherever the MCP server's own
// token file nests it. The file's shape is the server's, not this farm's, so
// the search is by key rather than by a path this code would have to guess.
func readRefreshToken(path string) (string, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the guard's own configured token file
	if err != nil {
		return "", err
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return "", fmt.Errorf("%s: %w", path, err)
	}
	return findString(document, "refresh_token"), nil
}

func findString(document any, key string) string {
	switch value := document.(type) {
	case map[string]any:
		if found, ok := value[key].(string); ok && found != "" {
			return found
		}
		for _, nested := range value {
			if found := findString(nested, key); found != "" {
				return found
			}
		}
	case []any:
		for _, nested := range value {
			if found := findString(nested, key); found != "" {
				return found
			}
		}
	}
	return ""
}

func readGoogleClient(path string) (id, secret string, err error) {
	data, err := os.ReadFile(path) // #nosec G304 -- the path is the guard's own configured client file
	if err != nil {
		return "", "", err
	}
	var document any
	if err := json.Unmarshal(data, &document); err != nil {
		return "", "", fmt.Errorf("%s: %w", path, err)
	}
	id, secret = findString(document, "client_id"), findString(document, "client_secret")
	if id == "" || secret == "" {
		return "", "", fmt.Errorf("%s names no client_id and client_secret", path)
	}
	return id, secret, nil
}

func (p *Probes) secrets(agent string) (map[string]string, bool) {
	if p.Secrets == nil {
		return nil, false
	}
	return p.Secrets(agent)
}

func (p *Probes) client() *http.Client {
	if p.HTTP != nil {
		return p.HTTP
	}
	return http.DefaultClient
}

func (p *Probes) tokenURL() string {
	if p.GoogleTokenURL != "" {
		return p.GoogleTokenURL
	}
	return GoogleTokenEndpoint
}

func (p *Probes) post(ctx context.Context, text string) {
	if p.Post != nil {
		p.Post(ctx, text)
	}
}

func (p *Probes) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now().UTC()
}

func (p *Probes) log() *slog.Logger {
	if p.Log != nil {
		return p.Log
	}
	return slog.Default()
}

func boolean(yes bool) float64 {
	if yes {
		return 1
	}
	return 0
}
