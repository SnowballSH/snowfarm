package roster

import (
	"bytes"
	"fmt"
	"hash/fnv"
	"os"
	"path"
	"time"

	yaml "go.yaml.in/yaml/v3"
)

type Tier string

const (
	TierManager Tier = "manager"
	TierWorker  Tier = "worker"
)

type Roster struct {
	Farm      Farm        `yaml:"farm"`
	Agents    []Agent     `yaml:"agents"`
	Teams     []Team      `yaml:"teams"`
	Schedules []Schedule  `yaml:"schedules"`
	Guard     GuardConfig `yaml:"guard"`
}

type Farm struct {
	HomeRoot    string `yaml:"home_root"`
	KanbanHome  string `yaml:"kanban_home"`
	MetricsAddr string `yaml:"metrics_addr"`

	WorkspacesRoot  string `yaml:"-"`
	AttachmentsRoot string `yaml:"-"`

	ClaudeDir string          `yaml:"claude_dir"`
	HermesBin string          `yaml:"hermes_bin"`
	SoulDir   string          `yaml:"soul_dir"`
	Modelgate ModelgateConfig `yaml:"modelgate"`
	Discord   DiscordConfig   `yaml:"discord"`
	Location  string          `yaml:"location"`
	UIDBase   int             `yaml:"uid_base"`
}

type ModelgateConfig struct {
	API string `yaml:"api"`
}

type DiscordConfig struct {
	GuildID                 string `yaml:"guild_id"`
	OperatorUserID          string `yaml:"operator_user_id"`
	SupervisorApplicationID string `yaml:"supervisor_application_id"`
}

type Agent struct {
	Name                 string      `yaml:"name"`
	Tier                 Tier        `yaml:"tier"`
	Teams                []string    `yaml:"teams"`
	Model                string      `yaml:"model"`
	Reasoning            string      `yaml:"reasoning"`
	DiscordApplicationID string      `yaml:"discord_application_id,omitempty"`
	Toolsets             []string    `yaml:"toolsets"`
	DisabledToolsets     []string    `yaml:"disabled_toolsets,omitempty"`
	DisabledTools        []string    `yaml:"disabled_tools,omitempty"`
	Skills               []string    `yaml:"skills"`
	Limits               Limits      `yaml:"limits"`
	Env                  []string    `yaml:"env"`
	AllowPrivateURLs     bool        `yaml:"allow_private_urls"`
	MCPServers           []MCPServer `yaml:"mcp_servers,omitempty"`
	MaxIterations        int         `yaml:"max_iterations"`
	ContextLength        int         `yaml:"context_length"`
	Enabled              bool        `yaml:"enabled"`
	ModelgateKeyMinted   string      `yaml:"modelgate_key_minted"`
}

type Limits struct {
	SliceMiB   int `yaml:"slice_mib"`
	RunMiB     int `yaml:"run_mib"`
	CPUPercent int `yaml:"cpu_percent"`
}

type MCPServer struct {
	Name    string            `yaml:"name"`
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args"`
	Env     map[string]string `yaml:"env"`
	Version string            `yaml:"version"`
	SHA256  string            `yaml:"sha256"`
}

type Team struct {
	Name  string `yaml:"name"`
	Label string `yaml:"label"`
}

type Schedule struct {
	Name       string `yaml:"name"`
	Cron       string `yaml:"cron"`
	Location   string `yaml:"location"`
	Title      string `yaml:"title"`
	Body       string `yaml:"body"`
	Assignee   string `yaml:"assignee"`
	Notifier   string `yaml:"notifier"`
	Channel    string `yaml:"channel"`
	MaxRuntime string `yaml:"max_runtime"`
	Priority   int    `yaml:"priority"`
}

// Duration is a time.Duration that unmarshals from a Go duration string.
// go.yaml.in/yaml/v3 rejects the string form for a bare time.Duration and
// reads a bare number as nanoseconds, so neither shape can express "30s".
type Duration time.Duration

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	parsed, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("duration %q: %w", node.Value, err)
	}
	*d = Duration(parsed)
	return nil
}

type GuardConfig struct {
	MaxConcurrentRuns       int      `yaml:"max_concurrent_runs"`
	HousekeepingPasses      *bool    `yaml:"housekeeping_passes"`
	MaxClaudeSlots          int      `yaml:"max_claude_slots"`
	ManagerTurnsPerHour     int      `yaml:"manager_turns_per_hour"`
	OperatorMentionsPerHour int      `yaml:"operator_mentions_per_hour"`
	BurstPer5m              int      `yaml:"burst_per_5m"`
	RestartBudgetPer6h      int      `yaml:"restart_budget_per_6h"`
	DispatchInterval        Duration `yaml:"dispatch_interval"`
	DefaultMaxRuntime       Duration `yaml:"default_max_runtime"`
	HygieneInterval         Duration `yaml:"hygiene_interval"`
	RestartWindow           string   `yaml:"restart_window"`
	TurnLogPattern          string   `yaml:"turn_log_pattern"`
	TurnCompletePattern     string   `yaml:"turn_complete_pattern"`

	// DrainedRestarts arms the nightly restart, and defaults off. Arming it
	// is a claim that the two turn patterns match a real gateway.log: a
	// restarter that cannot see a turn start and end reads every manager as
	// quiet and restarts gateways mid-turn.
	DrainedRestarts *bool `yaml:"drained_restarts"`
}

func (g GuardConfig) DrainedRestartsArmed() bool {
	return g.DrainedRestarts != nil && *g.DrainedRestarts
}

const (
	defaultHomeRoot   = "/var/lib/farm"
	defaultKanbanHome = "/srv/snowfarm/kanban"
	defaultClaudeDir  = "/srv/snowfarm/claude"
	defaultHermesBin  = "/usr/local/bin/hermes"
	defaultSoulDir    = "/etc/snowfarm/soul"

	defaultRestartWindow = "04:00-05:00"

	managerIterations = 120
	workerIterations  = 80

	// The floor is also the default: an agent minted below it would keep the
	// instance-role reach the IMDS egress drop keys on the uid to remove.
	uidFloor = 6000
	uidSpan  = 1000
)

func Load(path string) (*Roster, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Roster
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if err := decoder.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	r.applyDefaults()
	if err := r.Validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

func (r *Roster) applyDefaults() {
	defaultString(&r.Farm.HomeRoot, defaultHomeRoot)
	defaultString(&r.Farm.KanbanHome, defaultKanbanHome)
	defaultString(&r.Farm.ClaudeDir, defaultClaudeDir)
	defaultString(&r.Farm.HermesBin, defaultHermesBin)
	defaultString(&r.Farm.SoulDir, defaultSoulDir)
	defaultInt(&r.Farm.UIDBase, uidFloor)

	r.Farm.WorkspacesRoot = path.Join(r.Farm.KanbanHome, "kanban", "workspaces")
	r.Farm.AttachmentsRoot = path.Join(r.Farm.KanbanHome, "kanban", "attachments")

	defaultInt(&r.Guard.MaxConcurrentRuns, 1)
	defaultInt(&r.Guard.MaxClaudeSlots, 2)
	defaultInt(&r.Guard.ManagerTurnsPerHour, 30)
	defaultInt(&r.Guard.OperatorMentionsPerHour, 6)
	defaultInt(&r.Guard.BurstPer5m, 20)
	defaultInt(&r.Guard.RestartBudgetPer6h, 6)
	defaultDuration(&r.Guard.DispatchInterval, 30*time.Second)
	defaultDuration(&r.Guard.DefaultMaxRuntime, 2*time.Hour)
	defaultDuration(&r.Guard.HygieneInterval, 5*time.Minute)
	defaultString(&r.Guard.RestartWindow, defaultRestartWindow)
	if r.Guard.HousekeepingPasses == nil {
		on := true
		r.Guard.HousekeepingPasses = &on
	}
	if r.Guard.DrainedRestarts == nil {
		off := false
		r.Guard.DrainedRestarts = &off
	}

	for i := range r.Agents {
		iterations := workerIterations
		if r.Agents[i].Tier == TierManager {
			iterations = managerIterations
		}
		defaultInt(&r.Agents[i].MaxIterations, iterations)
	}
	for i := range r.Schedules {
		defaultString(&r.Schedules[i].Location, r.Farm.Location)
	}
}

func defaultString(field *string, value string) {
	if *field == "" {
		*field = value
	}
}

func defaultInt(field *int, value int) {
	if *field == 0 {
		*field = value
	}
}

func defaultDuration(field *Duration, value time.Duration) {
	if *field == 0 {
		*field = Duration(value)
	}
}

func (r *Roster) Agent(name string) (Agent, bool) {
	for _, a := range r.Agents {
		if a.Name == name {
			return a, true
		}
	}
	return Agent{}, false
}

func (r *Roster) Managers() []Agent { return r.tier(TierManager) }

func (r *Roster) Workers() []Agent { return r.tier(TierWorker) }

func (r *Roster) tier(t Tier) []Agent {
	var out []Agent
	for _, a := range r.Agents {
		if a.Tier == t {
			out = append(out, a)
		}
	}
	return out
}

func (r *Roster) EnabledAgents() []Agent { return enabled(r.Agents) }

func (r *Roster) EnabledManagers() []Agent { return enabled(r.Managers()) }

func (r *Roster) EnabledWorkers() []Agent { return enabled(r.Workers()) }

func enabled(agents []Agent) []Agent {
	var out []Agent
	for _, a := range agents {
		if a.Enabled {
			out = append(out, a)
		}
	}
	return out
}

func (r *Roster) IsEnabled(name string) bool {
	a, ok := r.Agent(name)
	return ok && a.Enabled
}

func (a Agent) User() string { return "farm-" + a.Name }

func (a Agent) Home(f Farm) string { return path.Join(f.HomeRoot, a.Name) }

func (a Agent) HermesHome(f Farm) string {
	return path.Join(a.Home(f), ".hermes", "profiles", a.Name)
}

// UID is the one derivation every consumer shares. Keying it off the name
// rather than the roster position keeps a uid fixed when an agent is
// inserted, reordered or removed, which is what useradd --uid needs.
func (r *Roster) UID(a Agent) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(a.Name))
	return r.Farm.UIDBase + int(h.Sum32()%uidSpan)
}

// CheckOnly reports whether an --only selection names a subset of the
// enabled set: apply provisions only enabled agents, so naming a disabled
// one asks for work apply will not do.
func CheckOnly(r *Roster, names []string) error {
	var unknown, disabled []string
	for _, name := range names {
		switch a, ok := r.Agent(name); {
		case !ok:
			unknown = append(unknown, name)
		case !a.Enabled:
			disabled = append(disabled, name)
		}
	}
	if len(unknown) > 0 {
		return fmt.Errorf("--only names agents that are not in the roster: %v", unknown)
	}
	if len(disabled) > 0 {
		return fmt.Errorf("--only must name a subset of the enabled set; these are disabled: %v", disabled)
	}
	return nil
}
