package sysops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/SnowballSH/snowfarm/internal/render"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	fixture         = "../roster/testdata/farm.yaml"
	fakebinStateEnv = "SNOWFARM_FAKEBIN_STATE"
)

type calls struct {
	t       *testing.T
	fakebin string
	log     [][]string
	users   map[string]int
}

func recordCalls(t *testing.T) *calls {
	t.Helper()
	fakebin, err := filepath.Abs("testdata/fakebin")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv(fakebinStateEnv, t.TempDir())
	return &calls{t: t, fakebin: fakebin, users: map[string]int{}}
}

func (c *calls) Run(name string, args ...string) ([]byte, error) {
	argv := append([]string{name}, args...)
	c.log = append(c.log, argv)
	if name == "useradd" {
		c.recordUser(args)
	}
	resolved := c.resolve(argv)
	out, err := exec.Command(resolved[0], resolved[1:]...).CombinedOutput()
	if err != nil {
		return out, fmt.Errorf("%v: %w: %s", argv, err, out)
	}
	return out, nil
}

// resolve rewrites an absolute host path to the fakebin of the same name, so
// an argv that names /usr/local/bin/hermes reaches the fake rather than a
// binary this machine does not have.
func (c *calls) resolve(argv []string) []string {
	out := slices.Clone(argv)
	for i, arg := range out {
		if !filepath.IsAbs(arg) {
			continue
		}
		fake := filepath.Join(c.fakebin, filepath.Base(arg))
		if info, err := os.Stat(fake); err == nil && !info.IsDir() {
			out[i] = fake
		}
	}
	return out
}

func (c *calls) recordUser(args []string) {
	uid := 0
	for i, arg := range args {
		if arg != "--uid" || i+1 >= len(args) {
			continue
		}
		parsed, err := strconv.Atoi(args[i+1])
		if err != nil {
			c.t.Fatalf("useradd --uid %q: %v", args[i+1], err)
		}
		uid = parsed
	}
	c.users[args[len(args)-1]] = uid
}

// Lookup reports the test process's own uid for every user the fake useradd
// created: the fake chown records its arguments and changes nothing, so a
// path Apply chowned to farm-<agent> is still owned by whoever runs the test.
func (c *calls) Lookup(user string) (int, bool) {
	if _, ok := c.users[user]; !ok {
		return 0, false
	}
	return os.Getuid(), true
}

func (c *calls) Count(name string) int { return len(c.invocations(name)) }

func (c *calls) invocations(name string) [][]string {
	var out [][]string
	for _, argv := range c.log {
		if argv[0] == name {
			out = append(out, argv)
		}
	}
	return out
}

func (c *calls) hasChown(spec, path string) bool {
	for _, argv := range c.invocations("chown") {
		if argv[len(argv)-2] == spec && argv[len(argv)-1] == path {
			return true
		}
	}
	return false
}

func (c *calls) index(t *testing.T, want ...string) int {
	t.Helper()
	for i, argv := range c.log {
		if slices.Equal(argv, want) {
			return i
		}
	}
	t.Fatalf("never ran %v", want)
	return -1
}

func (c *calls) hasArgv(want ...string) bool {
	for _, argv := range c.log {
		if slices.Equal(argv, want) {
			return true
		}
	}
	return false
}

func loadRoster(t *testing.T) *roster.Roster {
	t.Helper()
	r, err := roster.Load(fixture)
	if err != nil {
		t.Fatal(err)
	}
	r.Farm.SoulDir = t.TempDir()
	return r
}

func rosterWith(t *testing.T, swaps ...[2]string) *roster.Roster {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, swap := range swaps {
		if !strings.Contains(text, swap[0]) {
			t.Fatalf("fixture no longer carries %q", swap[0])
		}
		text = strings.Replace(text, swap[0], swap[1], 1)
	}
	path := filepath.Join(t.TempDir(), "farm.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	r, err := roster.Load(path)
	if err != nil {
		t.Fatal(err)
	}
	r.Farm.SoulDir = t.TempDir()
	return r
}

func targets(p Plan) []string {
	var out []string
	for _, changes := range [][]Change{p.Groups, p.Users, p.Dirs, p.Files, p.Units, p.Reloads} {
		for _, c := range changes {
			out = append(out, c.Target)
		}
	}
	return out
}

func detailOf(t *testing.T, p Plan, target string) string {
	t.Helper()
	for _, changes := range [][]Change{p.Groups, p.Users, p.Dirs, p.Files, p.Units, p.Reloads} {
		for _, c := range changes {
			if c.Target == target {
				return c.Detail
			}
		}
	}
	t.Fatalf("no change for %s in\n%s", target, p)
	return ""
}

func assertMode(t *testing.T, path string, want fs.FileMode) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode() & (fs.ModePerm | fs.ModeSetgid); got != want {
		t.Errorf("%s: mode %04o, want %04o", path, got, want)
	}
}

func assertChown(t *testing.T, c *calls, spec, path string) {
	t.Helper()
	if !c.hasChown(spec, path) {
		t.Errorf("no chown %s %s", spec, path)
	}
}

func applied(t *testing.T) (*Applier, *roster.Roster, *calls, string) {
	t.Helper()
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup}
	r := loadRoster(t)
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	return a, r, c, root
}

func TestApplyIsIdempotent(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup}
	r := loadRoster(t)

	p1, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if p1.Empty() {
		t.Fatal("the first plan against an empty root must not be empty")
	}
	if err := a.Apply(r, p1); err != nil {
		t.Fatal(err)
	}

	for _, agent := range r.Agents {
		home := filepath.Join(root, agent.Home(r.Farm))
		profile := filepath.Join(root, agent.HermesHome(r.Farm))
		assertMode(t, home, 0o750)
		assertMode(t, filepath.Join(home, ".hermes"), 0o755)
		assertMode(t, filepath.Join(home, ".hermes", "profiles"), 0o755)
		assertMode(t, profile, 0o750)
		assertMode(t, filepath.Join(profile, "logs"), 0o750)
		assertMode(t, filepath.Join(profile, "config.yaml"), 0o640)
		assertMode(t, filepath.Join(profile, "SOUL.md"), 0o640)
		assertChown(t, c, agent.User()+":"+agent.User(), filepath.Join(home, ".hermes"))
		assertChown(t, c, agent.User()+":"+agent.User(), filepath.Join(home, ".hermes", "profiles"))
		assertChown(t, c, "root:"+agent.User(), filepath.Join(profile, "config.yaml"))
		assertChown(t, c, "root:"+agent.User(), filepath.Join(profile, "SOUL.md"))
		assertChown(t, c, agent.User()+":farm-agents",
			filepath.Join(root, r.Farm.ClaudeDir, "runs", agent.Name+".jsonl"))
		unit := filepath.Join(home, ".config", "systemd", "user", "hermes-gateway.service")
		wants := filepath.Join(home, ".config", "systemd", "user", "default.target.wants", "hermes-gateway.service")
		if agent.Tier != roster.TierManager {
			if _, err := os.Lstat(unit); err == nil {
				t.Errorf("%s is a worker and has a gateway unit", agent.Name)
			}
			continue
		}
		assertMode(t, unit, 0o644)
		link, err := os.Readlink(wants)
		if err != nil {
			t.Fatal(err)
		}
		if link != unit {
			t.Errorf("%s wants link points at %s, want %s", agent.Name, link, unit)
		}
	}

	assertMode(t, filepath.Join(root, "var/lib/snowfarm"), 0o750)
	assertChown(t, c, "snowfarm:snowfarm", filepath.Join(root, "var/lib/snowfarm"))
	assertMode(t, filepath.Join(root, r.Farm.KanbanHome), 0o770|fs.ModeSetgid)
	assertChown(t, c, "root:farm-agents", filepath.Join(root, r.Farm.KanbanHome))
	assertMode(t, filepath.Join(root, r.Farm.ClaudeDir, "shared"), 0o750)
	assertChown(t, c, "snowfarm:farm-agents", filepath.Join(root, r.Farm.ClaudeDir, "shared"))
	assertMode(t, filepath.Join(root, r.Farm.ClaudeDir, "shared", "slots"), 0o750)
	assertMode(t, filepath.Join(root, r.Farm.ClaudeDir, "runs"), 0o750)
	assertChown(t, c, "snowfarm:farm-agents", filepath.Join(root, r.Farm.ClaudeDir, "runs"))
	for slot := 1; slot <= r.Guard.MaxClaudeSlots; slot++ {
		path := filepath.Join(root, r.Farm.ClaudeDir, "shared", "slots", fmt.Sprint(slot))
		assertMode(t, path, 0o640)
		assertChown(t, c, "snowfarm:farm-agents", path)
	}
	for _, path := range []string{
		"etc/sudoers.d/snowfarm",
		"etc/tmpfiles.d/snowfarm.conf",
		"usr/local/sbin/farm-unitctl",
		"etc/systemd/system/snowfarm-guard.service",
		"var/lib/snowfarm/profile-hashes.json",
	} {
		if _, err := os.Stat(filepath.Join(root, path)); err != nil {
			t.Errorf("%s: %v", path, err)
		}
	}

	p2, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p2.Empty() {
		t.Fatalf("second plan not empty:\n%s", p2)
	}
	if c.Count("useradd") != 7 || c.Count("loginctl") != 7 {
		t.Fatalf("useradd %d loginctl %d", c.Count("useradd"), c.Count("loginctl"))
	}
}

func TestApplyInstallsWhatTheRendererProduced(t *testing.T) {
	_, r, _, root := applied(t)
	atlas, ok := r.Agent("atlas")
	if !ok {
		t.Fatal("atlas is missing from the fixture")
	}
	files, err := render.Hermes(r, atlas)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		got, err := os.ReadFile(filepath.Join(root, f.Path))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != string(f.Content) {
			t.Errorf("%s: installed content differs from the render", f.Path)
		}
	}
}

func TestApplyFreezesTheProfileAndItsAncestors(t *testing.T) {
	_, r, c, root := applied(t)
	for _, agent := range r.Agents {
		home := filepath.Join(root, agent.Home(r.Farm))
		profile := filepath.Join(root, agent.HermesHome(r.Farm))
		for _, path := range []string{
			filepath.Join(home, ".hermes"),
			filepath.Join(home, ".hermes", "profiles"),
			filepath.Join(profile, "config.yaml"),
			filepath.Join(profile, "SOUL.md"),
		} {
			if !c.hasArgv("chattr", "+i", path) {
				t.Errorf("no chattr +i %s", path)
			}
		}
	}
}

func TestApplySetsTheGuardsAccessPath(t *testing.T) {
	_, r, c, root := applied(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	home := filepath.Join(root, hestia.Home(r.Farm))
	profile := filepath.Join(root, hestia.HermesHome(r.Farm))
	for _, path := range []string{
		home,
		filepath.Join(home, ".hermes"),
		filepath.Join(home, ".hermes", "profiles"),
		profile,
		filepath.Join(profile, "logs"),
	} {
		if !c.hasArgv("setfacl", "-m", "u:snowfarm:rx", path) {
			t.Errorf("no search ACL for the guard on %s", path)
		}
	}
	if !c.hasArgv("setfacl", "-d", "-m", "u:snowfarm:rx", filepath.Join(profile, "logs")) {
		t.Error("no default ACL on the log directory")
	}
	for _, name := range []string{"config.yaml", "SOUL.md"} {
		if !c.hasArgv("setfacl", "-m", "u:snowfarm:r", filepath.Join(profile, name)) {
			t.Errorf("no read ACL for the guard on %s", name)
		}
	}
}

func TestApplyKeepsWhatTheAgentWroteToItsRunLedger(t *testing.T) {
	a, r, _, root := applied(t)
	ledger := filepath.Join(root, r.Farm.ClaudeDir, "runs", "hestia.jsonl")
	line := `{"agent":"hestia","is_error":false}` + "\n"
	write(t, ledger, line, 0o640)
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("a written ledger is not drift:\n%s", p)
	}
	chmod(t, ledger, 0o600)
	p, err = a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	assertMode(t, ledger, 0o640)
	got, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != line {
		t.Fatalf("apply rewrote the run ledger: %q", got)
	}
}

func TestGuardReadACLLandsBetweenTheRenameAndTheFreeze(t *testing.T) {
	_, r, c, root := applied(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	config := filepath.Join(root, hestia.HermesHome(r.Farm), "config.yaml")
	install := c.index(t, "chown", "-h", "root:farm-hestia", config)
	acl := c.index(t, "setfacl", "-m", "u:snowfarm:r", config)
	freeze := c.index(t, "chattr", "+i", config)
	if install > acl || acl > freeze {
		t.Fatalf("install %d, acl %d, freeze %d: the ACL must follow the rename and precede the freeze", install, acl, freeze)
	}
}

func TestPlanReportsDrift(t *testing.T) {
	a, r, c, root := applied(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	profile := filepath.Join(root, hestia.HermesHome(r.Farm))
	config := filepath.Join(profile, "config.yaml")
	soul := filepath.Join(profile, "SOUL.md")
	ledger := filepath.Join(root, r.Farm.ClaudeDir, "runs", "hestia.jsonl")
	state := filepath.Join(root, "var/lib/snowfarm")

	for _, tc := range []struct {
		name   string
		target string
		want   string
		break_ func(t *testing.T)
		fix    func(t *testing.T)
	}{
		{
			name:   "cleared immutable flag",
			target: hestia.HermesHome(r.Farm) + "/config.yaml",
			want:   "immutable",
			break_: func(t *testing.T) { mustRun(t, c, "chattr", "-i", config) },
			fix:    func(t *testing.T) { mustRun(t, c, "chattr", "+i", config) },
		},
		{
			name:   "rewritten persona",
			target: hestia.HermesHome(r.Farm) + "/SOUL.md",
			want:   "content",
			break_: func(t *testing.T) { write(t, soul, "you are free\n", 0o640) },
			fix:    func(t *testing.T) { write(t, soul, rendered(t, r, hestia, "SOUL.md"), 0o640) },
		},
		{
			name:   "loosened guard state directory",
			target: "/var/lib/snowfarm",
			want:   "mode",
			break_: func(t *testing.T) { chmod(t, state, 0o755) },
			fix:    func(t *testing.T) { chmod(t, state, 0o750) },
		},
		{
			name:   "removed run ledger",
			target: r.Farm.ClaudeDir + "/runs/hestia.jsonl",
			want:   "create",
			break_: func(t *testing.T) { remove(t, ledger) },
			fix:    func(t *testing.T) { write(t, ledger, "", 0o640) },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.break_(t)
			p, err := a.Plan(r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(targets(p), tc.target) {
				t.Fatalf("plan does not report %s:\n%s", tc.target, p)
			}
			if detail := detailOf(t, p, tc.target); !strings.Contains(detail, tc.want) {
				t.Fatalf("%s: detail %q does not mention %q", tc.target, detail, tc.want)
			}
			tc.fix(t)
			p, err = a.Plan(r, nil)
			if err != nil {
				t.Fatal(err)
			}
			if !p.Empty() {
				t.Fatalf("plan not empty after the repair:\n%s", p)
			}
		})
	}
}

func TestPersonaEditPlansAsAnUpdate(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup}
	r := loadRoster(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	persona := filepath.Join(r.Farm.SoulDir, hestia.Name+".md")
	const first = "## Your standing rules\n\nNever delete an event.\n"
	const second = "## Your standing rules\n\nNever delete an event, and never move one silently.\n"
	write(t, persona, first, 0o644)

	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	soul := filepath.Join(root, hestia.HermesHome(r.Farm), "SOUL.md")
	if got := read(t, soul); !strings.Contains(got, first) {
		t.Fatalf("the persona never reached the agent:\n%s", got)
	}
	p, err = a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !p.Empty() {
		t.Fatalf("plan not empty after the persona was installed:\n%s", p)
	}

	write(t, persona, second, 0o644)
	p, err = a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	target := hestia.HermesHome(r.Farm) + "/SOUL.md"
	if !slices.Contains(targets(p), target) {
		t.Fatalf("an edited persona must plan as an update to %s:\n%s", target, p)
	}
	if detail := detailOf(t, p, target); !strings.Contains(detail, "content") {
		t.Fatalf("%s: detail %q does not mention the content", target, detail)
	}
	if !slices.Contains(targets(p), profileHashPath) {
		t.Fatalf("the hygiene baseline must move with the persona:\n%s", p)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	if got := read(t, soul); !strings.Contains(got, second) {
		t.Fatalf("the edited persona never reached the agent:\n%s", got)
	}
}

func TestPlanReportsAProfileAncestorTheAgentDoesNotOwn(t *testing.T) {
	a, r, c, _ := applied(t)
	a.Lookup = func(user string) (int, bool) {
		uid, ok := c.Lookup(user)
		if user == "farm-hestia" {
			return uid + 1, ok
		}
		return uid, ok
	}
	hestia, _ := r.Agent("hestia")
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range []string{
		hestia.Home(r.Farm) + "/.hermes",
		hestia.Home(r.Farm) + "/.hermes/profiles",
	} {
		if !slices.Contains(targets(p), target) {
			t.Fatalf("plan does not report the owner of %s:\n%s", target, p)
		}
		if detail := detailOf(t, p, target); !strings.Contains(detail, "owner") {
			t.Fatalf("%s: detail %q does not mention the owner", target, detail)
		}
	}
}

func TestApplyRestoresImmutabilityWhenAStepFails(t *testing.T) {
	a, r, c, root := applied(t)
	hestia, _ := r.Agent("hestia")
	config := filepath.Join(root, hestia.HermesHome(r.Farm), "config.yaml")
	write(t, config, "rewritten by hand\n", 0o640)

	a.Run = func(name string, args ...string) ([]byte, error) {
		if name == "setfacl" {
			return nil, errors.New("setfacl: refused")
		}
		return c.Run(name, args...)
	}
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err == nil {
		t.Fatal("apply must fail when a step fails")
	}
	a.Run = c.Run
	if !c.hasArgv("chattr", "-i", config) {
		t.Fatal("the apply never cleared the flag whose restoration this test is about")
	}
	frozen, err := a.immutable(config)
	if err != nil {
		t.Fatal(err)
	}
	if !frozen {
		t.Fatal("the failed apply left the profile writable")
	}
}

func TestApplyRefusesGatewayWithPendingChannels(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup, Start: true}
	r := loadRoster(t)

	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	unit := "/var/lib/farm/atlas/.config/systemd/user/hermes-gateway.service"
	if detail := detailOf(t, p, unit); !strings.Contains(detail, "not started") {
		t.Fatalf("%s: detail %q does not say the gateway stays stopped", unit, detail)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	installed, err := os.ReadFile(filepath.Join(root, unit))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(installed), pendingChannels) {
		t.Fatalf("the unit rendered from a nil channel map must hold %q", pendingChannels)
	}
	for _, name := range []string{"atlas", "iris"} {
		if c.hasArgv("runuser", "-u", "farm-"+name, "--", unitctlPath, "gateway", "start") {
			t.Errorf("%s: apply started a gateway whose channels are unresolved", name)
		}
	}
	pending, err := a.PendingGateways(r)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(pending, []string{"atlas", "iris"}) {
		t.Fatalf("pending gateways %v", pending)
	}
}

func TestApplyStartsGatewayWhenChannelsResolve(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup, Start: true}
	r := loadRoster(t)
	ch := map[string]string{
		"#command": "1", "#managers": "2",
		"#assistant-general": "3", "#assistant-log": "4",
		"#snowsys-general": "5", "#snowsys-log": "6",
		"#research-general": "7", "#research-log": "8",
		"#swe-general": "9", "#swe-log": "10",
		"#avalanche-general": "11", "#avalanche-log": "12",
	}
	p, err := a.Plan(r, ch)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"atlas", "iris"} {
		if !c.hasArgv("runuser", "-u", "farm-"+name, "--", unitctlPath, "gateway", "start") {
			t.Errorf("%s: gateway not started", name)
		}
	}
	pending, err := a.PendingGateways(r)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 0 {
		t.Fatalf("pending gateways %v", pending)
	}
}

func TestApplyCreatesTheManagerReportJobOnce(t *testing.T) {
	a, r, c, _ := applied(t)
	if got := c.Count("runuser"); got != 4 {
		t.Fatalf("runuser called %d times, want two managers listed and two created", got)
	}
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	if got := cronJobsCreated(c); got != 2 {
		t.Fatalf("the report job was created %d times, want once per manager", got)
	}
}

func cronJobsCreated(c *calls) int {
	created := 0
	for _, argv := range c.invocations("runuser") {
		if slices.Contains(argv, "create") {
			created++
		}
	}
	return created
}

func TestApplyReportsAFailedCronListing(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	var warnings strings.Builder
	a := &Applier{Root: root, Lookup: c.Lookup, Warn: &warnings}
	r := loadRoster(t)
	a.Run = func(name string, args ...string) ([]byte, error) {
		if name == "runuser" && slices.Contains(args, "list") {
			return nil, errors.New("hermes: the cron store is locked")
		}
		return c.Run(name, args...)
	}

	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"atlas", "iris", render.CronJobName,
		r.Farm.HermesBin + " cron list", "the cron store is locked",
	} {
		if !strings.Contains(warnings.String(), want) {
			t.Errorf("the failed listing is not reported as %q:\n%s", want, warnings.String())
		}
	}
	if got := cronJobsCreated(c); got != 2 {
		t.Fatalf("a listing that failed must still create the job: created %d, want one per manager", got)
	}

	warnings.Reset()
	a.Run = c.Run
	p, err = a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Apply(r, p); err != nil {
		t.Fatal(err)
	}
	if warnings.Len() != 0 {
		t.Errorf("a listing that succeeds reports nothing, got:\n%s", warnings.String())
	}
	if got := cronJobsCreated(c); got != 2 {
		t.Fatalf("the report job was created %d times, want once per manager", got)
	}
}

func TestOnlyRestrictsChangesToTheNamedAgents(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup, Only: []string{"hestia"}}
	r := loadRoster(t)
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Users) != 1 || p.Users[0].Target != "farm-hestia" {
		t.Fatalf("users %v", targets(Plan{Users: p.Users}))
	}
	for _, target := range targets(p) {
		for _, agent := range r.Agents {
			if agent.Name == "hestia" {
				continue
			}
			if strings.Contains(target, "/"+agent.Name) || strings.Contains(target, "farm-"+agent.Name) {
				t.Errorf("--only hestia plans %s", target)
			}
		}
	}
	if !slices.Contains(targets(p), r.Farm.KanbanHome) {
		t.Error("--only must still create the directories the agents share")
	}
}

func TestPlanSkipsADisabledAgent(t *testing.T) {
	root := t.TempDir()
	c := recordCalls(t)
	a := &Applier{Root: root, Run: c.Run, Lookup: c.Lookup}
	r := rosterWith(t, [2]string{`    enabled: true
    modelgate_key_minted: ""
  - name: hypatia`, `    enabled: false
    modelgate_key_minted: ""
  - name: hypatia`})
	p, err := a.Plan(r, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, target := range targets(p) {
		if strings.Contains(target, "euclid") {
			t.Errorf("a disabled agent is planned: %s", target)
		}
	}
}

func mustRun(t *testing.T, c *calls, name string, args ...string) {
	t.Helper()
	if _, err := c.Run(name, args...); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func write(t *testing.T, path, content string, mode fs.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
	chmod(t, path, mode)
}

func chmod(t *testing.T, path string, mode fs.FileMode) {
	t.Helper()
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
}

func remove(t *testing.T, path string) {
	t.Helper()
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
}

func rendered(t *testing.T, r *roster.Roster, agent roster.Agent, name string) string {
	t.Helper()
	files, err := render.Hermes(r, agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if filepath.Base(f.Path) == name {
			return string(f.Content)
		}
	}
	t.Fatalf("the renderer produces no %s for %s", name, agent.Name)
	return ""
}
