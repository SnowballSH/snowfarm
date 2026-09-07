package roster

import (
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
)

const (
	envPrefix       = "FARM_"
	claudeTokenEnv  = "CLAUDE_CODE_OAUTH_TOKEN"
	discordTokenEnv = "DISCORD_BOT_TOKEN"
	claudeSkill     = "farm-claude-code"
	kanbanToolset   = "kanban"
	mintedLayout    = "2006-01-02"
)

var (
	agentName       = regexp.MustCompile(`^[a-z][a-z0-9-]{1,15}$`)
	reasoningLevels = []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	claudeToolsets  = []string{"terminal", "skills"}
	workerNoTools   = []string{"skill_manage", "kanban_create", "kanban_link"}
)

func (r *Roster) Validate() error {
	var errs []error
	errs = append(errs, r.validateFarm()...)
	errs = append(errs, r.validateGuard()...)
	errs = append(errs, r.validateAgents()...)
	errs = append(errs, r.validateSchedules()...)
	return errors.Join(errs...)
}

func (r *Roster) validateFarm() []error {
	var errs []error
	if r.Farm.Discord.OperatorUserID == "" {
		errs = append(errs, errors.New("farm.discord.operator_user_id is required: a manager rendered without a human allow-list would act on bot mentions"))
	}
	if r.Farm.Location == "" {
		errs = append(errs, errors.New("farm.location is required"))
	} else if _, err := time.LoadLocation(r.Farm.Location); err != nil {
		errs = append(errs, fmt.Errorf("farm.location %q: %w", r.Farm.Location, err))
	}
	if r.Farm.UIDBase < uidFloor {
		errs = append(errs, fmt.Errorf("farm.uid_base is %d, below the floor %d the IMDS egress drop keys on", r.Farm.UIDBase, uidFloor))
	}
	return errs
}

func (r *Roster) validateGuard() []error {
	var errs []error
	positives := []struct {
		field string
		value int
	}{
		{"max_concurrent_runs", r.Guard.MaxConcurrentRuns},
		{"max_claude_slots", r.Guard.MaxClaudeSlots},
		{"manager_turns_per_hour", r.Guard.ManagerTurnsPerHour},
		{"operator_mentions_per_hour", r.Guard.OperatorMentionsPerHour},
		{"burst_per_5m", r.Guard.BurstPer5m},
		{"restart_budget_per_6h", r.Guard.RestartBudgetPer6h},
	}
	for _, p := range positives {
		if p.value <= 0 {
			errs = append(errs, fmt.Errorf("guard.%s must be positive, got %d", p.field, p.value))
		}
	}
	intervals := []struct {
		field string
		value Duration
	}{
		{"dispatch_interval", r.Guard.DispatchInterval},
		{"default_max_runtime", r.Guard.DefaultMaxRuntime},
		{"hygiene_interval", r.Guard.HygieneInterval},
	}
	for _, i := range intervals {
		if i.value <= 0 {
			errs = append(errs, fmt.Errorf("guard.%s must be positive, got %s", i.field, time.Duration(i.value)))
		}
	}
	for field, pattern := range map[string]string{
		"turn_log_pattern":      r.Guard.TurnLogPattern,
		"turn_complete_pattern": r.Guard.TurnCompletePattern,
	} {
		if pattern == "" {
			continue
		}
		if _, err := regexp.Compile(pattern); err != nil {
			errs = append(errs, fmt.Errorf("guard.%s: %w", field, err))
		}
	}
	if r.Guard.RestartWindow != "" && r.Guard.TurnCompletePattern == "" {
		errs = append(errs, errors.New("guard.turn_complete_pattern is required while guard.restart_window arms the drained restarter: a turn-start line alone never proves the turn ended"))
	}
	return errs
}

func (r *Roster) validateAgents() []error {
	var errs []error
	teams := map[string]bool{}
	for _, t := range r.Teams {
		teams[t.Name] = true
	}
	seen := map[string]bool{}
	uids := map[int]string{}
	for _, a := range r.Agents {
		if seen[a.Name] {
			errs = append(errs, fmt.Errorf("duplicate agent name %q", a.Name))
			continue
		}
		seen[a.Name] = true
		errs = append(errs, validateAgent(a, teams)...)
		uid := r.UID(a)
		if other, collides := uids[uid]; collides {
			errs = append(errs, fmt.Errorf("agents %q and %q hash to the same uid %d: rename one", other, a.Name, uid))
		} else {
			uids[uid] = a.Name
		}
	}
	return errs
}

func validateAgent(a Agent, teams map[string]bool) []error {
	fail := func(format string, args ...any) error {
		return fmt.Errorf("agent %q: %s", a.Name, fmt.Sprintf(format, args...))
	}
	var errs []error
	switch {
	case a.Name == "default":
		errs = append(errs, errors.New(`agent name "default" is reserved: it is Hermes' fallback profile`))
	case !agentName.MatchString(a.Name):
		errs = append(errs, fmt.Errorf("agent name %q must match %s", a.Name, agentName))
	}
	if a.Tier != TierManager && a.Tier != TierWorker {
		errs = append(errs, fail("tier %q must be %q or %q", a.Tier, TierManager, TierWorker))
	}
	if a.Model == "" {
		errs = append(errs, fail("model is required"))
	}
	if !slices.Contains(reasoningLevels, a.Reasoning) {
		errs = append(errs, fail("reasoning %q must be one of %s", a.Reasoning, strings.Join(reasoningLevels, ", ")))
	}
	if a.Tier == TierManager && a.DiscordApplicationID == "" {
		errs = append(errs, fail("a manager needs a discord_application_id"))
	}
	if a.Tier == TierWorker && a.DiscordApplicationID != "" {
		errs = append(errs, fail("workers hold no discord application, so discord_application_id must be empty"))
	}
	for _, team := range a.Teams {
		if !teams[team] {
			errs = append(errs, fail("unknown team %q", team))
		}
	}
	errs = append(errs, validateClaudeReach(a, fail)...)
	errs = append(errs, validateKanbanReach(a, fail)...)
	for _, name := range a.Env {
		if name != claudeTokenEnv && name != discordTokenEnv && !strings.HasPrefix(name, envPrefix) {
			errs = append(errs, fail("env %q must start with %s or be %s or %s", name, envPrefix, claudeTokenEnv, discordTokenEnv))
		}
	}
	if a.ContextLength <= 0 {
		errs = append(errs, fail("context_length must be positive: Hermes bounds its own context to it"))
	}
	if a.Limits.SliceMiB <= 0 {
		errs = append(errs, fail("limits.slice_mib must be positive"))
	}
	if a.Limits.CPUPercent <= 0 {
		errs = append(errs, fail("limits.cpu_percent must be positive"))
	}
	if a.Tier == TierWorker && a.Limits.RunMiB <= 0 {
		errs = append(errs, fail("limits.run_mib must be positive for a worker"))
	}
	if a.ModelgateKeyMinted != "" {
		if _, err := time.Parse(mintedLayout, a.ModelgateKeyMinted); err != nil {
			errs = append(errs, fail("modelgate_key_minted %q must be YYYY-MM-DD", a.ModelgateKeyMinted))
		}
	}
	for _, server := range a.MCPServers {
		toolset := "mcp-" + server.Name
		if !slices.Contains(a.Toolsets, toolset) {
			errs = append(errs, fail("mcp_servers lists %q, so toolsets must list %q", server.Name, toolset))
		}
	}
	return errs
}

func validateClaudeReach(a Agent, fail func(string, ...any) error) []error {
	var errs []error
	for _, toolset := range claudeToolsets {
		if !slices.Contains(a.Toolsets, toolset) {
			errs = append(errs, fail("toolsets must list %q, or Claude Code is unreachable", toolset))
		}
		if slices.Contains(a.DisabledToolsets, toolset) {
			errs = append(errs, fail("disabled_toolsets must not list %q, or Claude Code is unreachable however the toolsets read", toolset))
		}
	}
	if !slices.Contains(a.Skills, claudeSkill) {
		errs = append(errs, fail("skills must list %q, or the skill body never reaches the model", claudeSkill))
	}
	if !slices.Contains(a.Env, claudeTokenEnv) {
		errs = append(errs, fail("env must list %s, or Claude Code fails on authentication", claudeTokenEnv))
	}
	return errs
}

func validateKanbanReach(a Agent, fail func(string, ...any) error) []error {
	var errs []error
	if !slices.Contains(a.Toolsets, kanbanToolset) {
		errs = append(errs, fail("toolsets must list %q: without it a worker has no kanban_complete and no card can terminate", kanbanToolset))
	}
	if a.Tier != TierWorker {
		return errs
	}
	if slices.Contains(a.DisabledToolsets, kanbanToolset) {
		errs = append(errs, fail("disabled_toolsets must not list %q on a worker: it strips kanban_complete and the escalation terminators with it", kanbanToolset))
	}
	for _, tool := range workerNoTools {
		if !slices.Contains(a.DisabledTools, tool) {
			errs = append(errs, fail("disabled_tools must list %q on a worker", tool))
		}
	}
	return errs
}

func (r *Roster) validateSchedules() []error {
	var errs []error
	workers := names(r.Workers())
	managers := names(r.Managers())
	for _, s := range r.Schedules {
		fail := func(format string, args ...any) error {
			return fmt.Errorf("schedule %q: %s", s.Name, fmt.Sprintf(format, args...))
		}
		if !slices.Contains(workers, s.Assignee) {
			errs = append(errs, fail("assignee %q is not a worker; the workers are %s", s.Assignee, strings.Join(workers, ", ")))
		}
		if !slices.Contains(managers, s.Notifier) {
			errs = append(errs, fail("notifier must be a manager, %q is not", s.Notifier))
		}
		if !strings.HasPrefix(s.Channel, "#") {
			errs = append(errs, fail("channel %q must start with #", s.Channel))
		}
		if _, err := cron.ParseStandard(s.Cron); err != nil {
			errs = append(errs, fmt.Errorf("schedule %q: cron %q: %w", s.Name, s.Cron, err))
		}
		if _, err := time.LoadLocation(s.Location); err != nil {
			errs = append(errs, fmt.Errorf("schedule %q: location %q: %w", s.Name, s.Location, err))
		}
	}
	return errs
}

func names(agents []Agent) []string {
	out := make([]string, 0, len(agents))
	for _, a := range agents {
		out = append(out, a.Name)
	}
	slices.Sort(out)
	return out
}
