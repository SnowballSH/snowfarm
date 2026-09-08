package roster

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func load(t *testing.T, path string) *Roster {
	t.Helper()
	r, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestLoadRealRoster(t *testing.T) {
	r, err := Load("testdata/farm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(r.Managers()); got != 2 {
		t.Fatalf("managers = %d", got)
	}
	if got := len(r.Workers()); got != 5 {
		t.Fatalf("workers = %d", got)
	}
	a, _ := r.Agent("hestia")
	if a.HermesHome(r.Farm) != "/var/lib/farm/hestia/.hermes/profiles/hestia" {
		t.Fatal(a.HermesHome(r.Farm))
	}
	if a.User() != "farm-hestia" || a.Home(r.Farm) != "/var/lib/farm/hestia" {
		t.Fatalf("user %s home %s", a.User(), a.Home(r.Farm))
	}
	for _, ag := range r.Agents {
		if !slices.Contains(ag.Toolsets, "terminal") || !slices.Contains(ag.Toolsets, "skills") ||
			!slices.Contains(ag.Skills, "farm-claude-code") ||
			!slices.Contains(ag.Env, "CLAUDE_CODE_OAUTH_TOKEN") {
			t.Fatalf("%s cannot reach Claude Code: toolsets=%v skills=%v env=%v", ag.Name, ag.Toolsets, ag.Skills, ag.Env)
		}
	}
	if time.Duration(r.Guard.DispatchInterval) != 30*time.Second {
		t.Fatal("default dispatch interval")
	}
	if time.Duration(r.Guard.DefaultMaxRuntime) != 2*time.Hour {
		t.Fatal("default max runtime")
	}
}

func TestDerivedBoardRootsNestUnderKanbanHome(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	if r.Farm.WorkspacesRoot != "/srv/snowfarm/kanban/kanban/workspaces" {
		t.Fatalf("workspaces root %q", r.Farm.WorkspacesRoot)
	}
	if r.Farm.AttachmentsRoot != "/srv/snowfarm/kanban/kanban/attachments" {
		t.Fatalf("attachments root %q", r.Farm.AttachmentsRoot)
	}
}

func TestDefaultsAreConservative(t *testing.T) {
	r, err := Load("testdata/farm-no-guard-block.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if r.Guard.MaxConcurrentRuns != 1 || r.Guard.MaxClaudeSlots != 2 {
		t.Fatalf("defaults: runs=%d slots=%d", r.Guard.MaxConcurrentRuns, r.Guard.MaxClaudeSlots)
	}
	if r.Guard.ManagerTurnsPerHour != 30 || r.Guard.OperatorMentionsPerHour != 6 ||
		r.Guard.BurstPer5m != 20 || r.Guard.RestartBudgetPer6h != 6 {
		t.Fatalf("guard defaults: %+v", r.Guard)
	}
	if r.Guard.HousekeepingPasses == nil || !*r.Guard.HousekeepingPasses {
		t.Fatal("housekeeping passes must default on")
	}
	if time.Duration(r.Guard.DispatchInterval) != 30*time.Second ||
		time.Duration(r.Guard.HygieneInterval) != 5*time.Minute ||
		time.Duration(r.Guard.DefaultMaxRuntime) != 2*time.Hour {
		t.Fatalf("interval defaults: %+v", r.Guard)
	}
	if r.Guard.RestartWindow != "04:00-05:00" {
		t.Fatalf("restart window %q", r.Guard.RestartWindow)
	}
	if r.Guard.TurnLogPattern == "" || r.Guard.TurnCompletePattern == "" {
		t.Fatalf("turn patterns: %q %q", r.Guard.TurnLogPattern, r.Guard.TurnCompletePattern)
	}
	if r.Farm.HomeRoot != "/var/lib/farm" || r.Farm.KanbanHome != "/srv/snowfarm/kanban" ||
		r.Farm.ClaudeDir != "/srv/snowfarm/claude" || r.Farm.HermesBin != "/usr/local/bin/hermes" ||
		r.Farm.SoulDir != "/etc/snowfarm/soul" {
		t.Fatalf("farm defaults: %+v", r.Farm)
	}
	if r.Farm.UIDBase != 6000 {
		t.Fatalf("uid base %d", r.Farm.UIDBase)
	}
	atlas, _ := r.Agent("atlas")
	hestia, _ := r.Agent("hestia")
	if atlas.MaxIterations != 120 || hestia.MaxIterations != 80 {
		t.Fatalf("iteration defaults: manager=%d worker=%d", atlas.MaxIterations, hestia.MaxIterations)
	}
	if r.Schedules[0].Location != r.Farm.Location {
		t.Fatalf("schedule location %q", r.Schedules[0].Location)
	}
}

func TestValidateRejects(t *testing.T) {
	cases := map[string]string{
		"duplicate-name.yaml":             "duplicate agent name",
		"default-name.yaml":               `agent name "default"`,
		"manager-without-app.yaml":        "discord_application_id",
		"worker-with-app.yaml":            "workers hold no discord",
		"schedule-unknown-worker.yaml":    "schedule",
		"schedule-notifier-worker.yaml":   "notifier must be a manager",
		"bad-reasoning.yaml":              "reasoning",
		"no-operator.yaml":                "operator_user_id",
		"zero-runs.yaml":                  "max_concurrent_runs",
		"env-prefix.yaml":                 "FARM_",
		"agent-without-terminal.yaml":     "terminal",
		"agent-without-skills.yaml":       "skills",
		"agent-without-claude-skill.yaml": "farm-claude-code",
		"agent-without-claude-token.yaml": "CLAUDE_CODE_OAUTH_TOKEN",
		"worker-no-slice.yaml":            "slice_mib",
		"bad-key-minted.yaml":             "modelgate_key_minted",
		"zero-context.yaml":               "context_length",
		"disabled-skills.yaml":            "disabled_toolsets",
		"uid-base-below-floor.yaml":       "uid_base",
		"relative-soul-dir.yaml":          "farm.soul_dir",
	}
	for file, want := range cases {
		t.Run(file, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", "invalid", file))
			if err == nil || !strings.Contains(err.Error(), want) {
				t.Fatalf("want error containing %q, got %v", want, err)
			}
		})
	}
}

func TestRejectsDisabledToolsetsStrippingClaude(t *testing.T) {
	for _, file := range []string{"disabled-skills.yaml", "worker-disabled-kanban.yaml"} {
		t.Run(file, func(t *testing.T) {
			_, err := Load(filepath.Join("testdata", "invalid", file))
			if err == nil || !strings.Contains(err.Error(), "disabled_toolsets") {
				t.Fatalf("want a disabled_toolsets rejection, got %v", err)
			}
		})
	}
}

func TestUIDStableUnderReorder(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	before := map[string]int{}
	for _, a := range r.Agents {
		before[a.Name] = r.UID(a)
	}
	slices.Reverse(r.Agents)
	r.Agents = append(r.Agents[3:], r.Agents[:3]...)
	for _, a := range r.Agents {
		if got := r.UID(a); got != before[a.Name] {
			t.Fatalf("%s uid %d, want %d", a.Name, got, before[a.Name])
		}
	}
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUIDStableUnderInsertion(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	before := map[string]int{}
	for _, a := range r.Agents {
		before[a.Name] = r.UID(a)
	}
	inserted := r.Agents[len(r.Agents)-1]
	inserted.Name = "aardvark"
	r.Agents = append([]Agent{inserted}, r.Agents...)
	if err := r.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, a := range r.Agents[1:] {
		if got := r.UID(a); got != before[a.Name] {
			t.Fatalf("%s uid %d, want %d", a.Name, got, before[a.Name])
		}
	}
}

func TestUIDsAreDistinctAndAboveTheFloor(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	seen := map[int]string{}
	for _, a := range r.Agents {
		uid := r.UID(a)
		if uid < 6000 {
			t.Fatalf("%s uid %d is below the IMDS floor", a.Name, uid)
		}
		if other, dup := seen[uid]; dup {
			t.Fatalf("%s and %s share uid %d", other, a.Name, uid)
		}
		seen[uid] = a.Name
	}
}

func TestValidateRejectsUIDCollision(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	r.Agents[1].Name = "aadal"
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "uid") {
		t.Fatalf("want a uid collision rejection, got %v", err)
	}
}

func TestEnabledSetScopesToEnabledAgents(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	if len(r.EnabledAgents()) != len(r.Agents) {
		t.Fatalf("the fixture roster is fully enabled: %d", len(r.EnabledAgents()))
	}
	for i := range r.Agents {
		if r.Agents[i].Name == "argus" {
			r.Agents[i].Enabled = false
		}
	}
	if r.IsEnabled("argus") {
		t.Fatal("argus must be disabled")
	}
	for _, a := range r.EnabledAgents() {
		if a.Name == "argus" {
			t.Fatal("EnabledAgents returned a disabled agent")
		}
	}
	if got := len(r.EnabledWorkers()); got != 4 {
		t.Fatalf("enabled workers = %d", got)
	}
	if got := len(r.EnabledManagers()); got != 2 {
		t.Fatalf("enabled managers = %d", got)
	}
	names := []string{}
	for _, a := range r.EnabledAgents() {
		names = append(names, a.Name)
	}
	if !slices.Equal(names, []string{"atlas", "iris", "hestia", "euclid", "hypatia", "daedalus"}) {
		t.Fatalf("enabled agents out of roster order: %v", names)
	}
	if r.IsEnabled("nobody") {
		t.Fatal("an unknown name is not enabled")
	}
}

func TestCheckOnlyNamesTheEnabledSet(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	for i := range r.Agents {
		if r.Agents[i].Name == "argus" {
			r.Agents[i].Enabled = false
		}
	}
	if err := CheckOnly(r, []string{"atlas", "hestia"}); err != nil {
		t.Fatal(err)
	}
	if err := CheckOnly(r, nil); err != nil {
		t.Fatal(err)
	}
	err := CheckOnly(r, []string{"atlas", "argus"})
	if err == nil || !strings.Contains(err.Error(), "argus") {
		t.Fatalf("want a disabled-agent rejection, got %v", err)
	}
	err = CheckOnly(r, []string{"nobody"})
	if err == nil || !strings.Contains(err.Error(), "nobody") {
		t.Fatalf("want an unknown-agent rejection, got %v", err)
	}
}

func TestLoadRejectsUnparseableDuration(t *testing.T) {
	src, err := os.ReadFile("testdata/farm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(src), "dispatch_interval: 30s", "dispatch_interval: 30", 1)
	path := filepath.Join(t.TempDir(), "farm.yaml")
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("want a duration rejection, got %v", err)
	}
}

func TestLoadRejectsUnknownField(t *testing.T) {
	src, err := os.ReadFile("testdata/farm.yaml")
	if err != nil {
		t.Fatal(err)
	}
	broken := strings.Replace(string(src), "  max_claude_slots: 2", "  max_claude_slot: 2", 1)
	path := filepath.Join(t.TempDir(), "farm.yaml")
	if err := os.WriteFile(path, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil || !strings.Contains(err.Error(), "max_claude_slot") {
		t.Fatalf("want an unknown-field rejection, got %v", err)
	}
}

func TestValidateRejectsMissingMCPToolset(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	for i := range r.Agents {
		if r.Agents[i].Name == "hestia" {
			r.Agents[i].Toolsets = slices.DeleteFunc(slices.Clone(r.Agents[i].Toolsets), func(s string) bool {
				return s == "mcp-google-calendar"
			})
		}
	}
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "mcp-google-calendar") {
		t.Fatalf("want a missing mcp toolset rejection, got %v", err)
	}
}

func TestValidateRejectsEmptyTurnCompletePattern(t *testing.T) {
	r := load(t, "testdata/farm.yaml")
	r.Guard.TurnCompletePattern = ""
	err := r.Validate()
	if err == nil || !strings.Contains(err.Error(), "turn_complete_pattern") {
		t.Fatalf("want a turn_complete_pattern rejection, got %v", err)
	}
	r.Guard.RestartWindow = ""
	if err := r.Validate(); err != nil {
		t.Fatalf("with the drained restarter disarmed the pattern is optional: %v", err)
	}
}
