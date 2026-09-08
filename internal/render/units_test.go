package render

import (
	"bytes"
	"errors"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

const systemGolden = "testdata/golden/system"

func systemFiles(t *testing.T, r *roster.Roster) []File {
	t.Helper()
	files, err := System(r)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func agentFiles(t *testing.T, r *roster.Roster, name string, ch map[string]string) []File {
	t.Helper()
	a, ok := r.Agent(name)
	if !ok {
		t.Fatalf("%s is missing from the fixture", name)
	}
	files, err := Units(r, a, r.UID(a), ch)
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func body(t *testing.T, files []File, suffix string) string {
	t.Helper()
	f := fileNamed(files, suffix)
	if f == nil {
		t.Fatalf("no rendered file ending in %s", suffix)
	}
	return string(f.Content)
}

func altRoster(t *testing.T) *roster.Roster {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, swap := range [][2]string{
		{"  home_root: /var/lib/farm", "  home_root: /opt/farm/homes"},
		{"  kanban_home: /srv/snowfarm/kanban", "  kanban_home: /srv/board"},
	} {
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
	return r
}

func environment(unit, key string) string {
	for line := range strings.Lines(unit) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "Environment="+key+"="); ok {
			return value
		}
	}
	return ""
}

func directive(unit, key string) string {
	for line := range strings.Lines(unit) {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), key+"="); ok {
			return value
		}
	}
	return ""
}

var shellAssignment = regexp.MustCompile(`(?m)^[ \t]*(?:export )?([A-Za-z_]+)="([^"]*)"`)

func shellVar(script, name string) string {
	for _, m := range shellAssignment.FindAllStringSubmatch(script, -1) {
		if m[1] == name {
			return m[2]
		}
	}
	return ""
}

func TestUnitsGolden(t *testing.T) {
	r := loadRoster(t)
	var files []File
	for _, a := range r.Agents {
		units, err := Units(r, a, r.UID(a), nil)
		if err != nil {
			t.Fatalf("%s: %v", a.Name, err)
		}
		files = append(files, units...)
	}
	files = append(files, systemFiles(t, r)...)

	rendered := map[string]bool{}
	for _, f := range files {
		rel := strings.TrimPrefix(f.Path, "/")
		if rendered[rel] {
			t.Fatalf("%s is rendered twice", f.Path)
		}
		rendered[rel] = true
		golden := filepath.Join(systemGolden, filepath.FromSlash(rel))
		if *update {
			if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(golden, f.Content, 0o644); err != nil {
				t.Fatal(err)
			}
		}
		want, err := os.ReadFile(golden)
		if err != nil {
			t.Fatalf("missing golden %s", golden)
		}
		if !bytes.Equal(want, f.Content) {
			t.Errorf("%s differs from golden", f.Path)
		}
	}
	for _, stale := range staleGoldens(t, systemGolden, rendered) {
		t.Errorf("golden %s is no longer rendered", stale)
	}
}

func TestUnitOwnership(t *testing.T) {
	r := loadRoster(t)
	atlas := agentFiles(t, r, "atlas", nil)
	hestia := agentFiles(t, r, "hestia", nil)

	unit := fileNamed(atlas, ".config/systemd/user/hermes-gateway.service")
	if unit == nil {
		t.Fatal("atlas has no gateway unit")
	}
	if unit.Owner != "farm-atlas" || unit.Group != "farm-atlas" || unit.Mode.Perm() != 0o644 {
		t.Errorf("gateway unit %s:%s %v", unit.Owner, unit.Group, unit.Mode.Perm())
	}
	link := fileNamed(atlas, "default.target.wants/hermes-gateway.service")
	if link == nil {
		t.Fatal("atlas has no default.target.wants symlink")
	}
	if !link.IsSymlink() {
		t.Errorf("the wants entry must be a symlink, got mode %v", link.Mode)
	}
	if string(link.Content) != unit.Path {
		t.Errorf("symlink target %s, want %s", link.Content, unit.Path)
	}
	if link.Owner != "farm-atlas" || link.Group != "farm-atlas" {
		t.Errorf("symlink %s:%s", link.Owner, link.Group)
	}

	rootOwned := slices.Concat(hestia, systemFiles(t, r))
	for _, f := range rootOwned {
		if f.Owner != rootOwner || f.Group != rootOwner {
			t.Errorf("%s is %s:%s, want root:root", f.Path, f.Owner, f.Group)
		}
	}
	modes := map[string]fs.FileMode{
		"50-snowfarm.conf":         0o644,
		"limits/hestia":            0o644,
		"sbin/farm-unitctl":        0o755,
		"sudoers.d/snowfarm":       0o440,
		"tmpfiles.d/snowfarm.conf": 0o644,
		"snowfarm-guard.service":   0o644,
	}
	for suffix, want := range modes {
		f := fileNamed(rootOwned, suffix)
		if f == nil {
			t.Fatalf("nothing rendered ending in %s", suffix)
		}
		if f.Mode.Perm() != want {
			t.Errorf("%s mode %v, want %v", f.Path, f.Mode.Perm(), want)
		}
	}
}

func TestManagerUnitEnvironment(t *testing.T) {
	r := loadRoster(t)
	atlas, ok := r.Agent("atlas")
	if !ok {
		t.Fatal("atlas is missing from the fixture")
	}
	unit := body(t, agentFiles(t, r, "atlas", nil), "user/hermes-gateway.service")

	for key, want := range map[string]string{
		"DISCORD_ALLOWED_USERS":             r.Farm.Discord.OperatorUserID,
		"DISCORD_ALLOWED_CHANNELS":          "pending-reconcile",
		"DISCORD_REQUIRE_MENTION":           "true",
		"DISCORD_THREAD_REQUIRE_MENTION":    "true",
		"DISCORD_ALLOW_BOTS":                "none",
		"DISCORD_REPLY_TO_MODE":             "off",
		"DISCORD_HISTORY_BACKFILL":          "false",
		"DISCORD_ALLOW_MENTION_EVERYONE":    "false",
		"DISCORD_ALLOW_MENTION_ROLES":       "false",
		"DISCORD_AUTO_THREAD":               "true",
		"DISCORD_COMMAND_SYNC_POLICY":       "off",
		"DISCORD_HOME_CHANNEL":              "pending-reconcile",
		"HERMES_KANBAN_DISPATCH_IN_GATEWAY": "false",
		"HERMES_HOME":                       atlas.HermesHome(r.Farm),
		"HERMES_KANBAN_HOME":                r.Farm.KanbanHome,
		"HERMES_KANBAN_WORKSPACES_ROOT":     r.Farm.WorkspacesRoot,
		"HERMES_KANBAN_ATTACHMENTS_ROOT":    r.Farm.AttachmentsRoot,
		"HERMES_WRITE_SAFE_ROOT":            atlas.Home(r.Farm),
		"HERMES_REDACT_SECRETS":             "true",
		"HOME":                              atlas.Home(r.Farm),
	} {
		if got := environment(unit, key); got != want {
			t.Errorf("%s is %q, want %q", key, got, want)
		}
	}
	if got := environment(unit, "HERMES_MAX_ITERATIONS"); got != "120" {
		t.Errorf("HERMES_MAX_ITERATIONS is %q, want 120", got)
	}
	for _, want := range []string{"UMask=0007", "NoNewPrivileges=true", "WantedBy=default.target"} {
		if !strings.Contains(unit, "\n"+want+"\n") {
			t.Errorf("the gateway unit is missing %s", want)
		}
	}
	for _, forbidden := range []string{"docker", "DOCKER", "TOKEN", "podman"} {
		if strings.Contains(unit, forbidden) {
			t.Errorf("the gateway unit carries %q", forbidden)
		}
	}
}

func TestManagerUnitTakesResolvedChannelIDs(t *testing.T) {
	r := loadRoster(t)
	ch := map[string]string{
		"#command":           "900000000000000001",
		"#managers":          "900000000000000002",
		"#assistant-general": "900000000000000003",
		"#assistant-log":     "900000000000000004",
		"#research-general":  "900000000000000005",
		"#research-log":      "900000000000000006",
		"#swe-general":       "900000000000000007",
		"#swe-log":           "900000000000000008",
		"#farm-status":       "900000000000000009",
	}
	unit := body(t, agentFiles(t, r, "iris", ch), "user/hermes-gateway.service")
	want := "900000000000000001,900000000000000002,900000000000000005,900000000000000006,900000000000000007,900000000000000008"
	if got := environment(unit, "DISCORD_ALLOWED_CHANNELS"); got != want {
		t.Errorf("DISCORD_ALLOWED_CHANNELS is %q, want %q", got, want)
	}
	if got := environment(unit, "DISCORD_HOME_CHANNEL"); got != "900000000000000002" {
		t.Errorf("DISCORD_HOME_CHANNEL is %q", got)
	}
	if strings.Contains(unit, "pending-reconcile") {
		t.Error("a resolved map must leave no placeholder behind")
	}
}

func TestManagerUnitHoldsPlaceholderWhenAChannelIsMissing(t *testing.T) {
	r := loadRoster(t)
	unit := body(t, agentFiles(t, r, "iris", map[string]string{"#command": "1"}), "user/hermes-gateway.service")
	if got := environment(unit, "DISCORD_ALLOWED_CHANNELS"); got != "pending-reconcile" {
		t.Errorf("a partial map must not render half a channel list, got %q", got)
	}
}

func TestSliceDropInAndRunLimits(t *testing.T) {
	r := loadRoster(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	files := agentFiles(t, r, "hestia", nil)
	drop := fileNamed(files, "50-snowfarm.conf")
	if drop == nil {
		t.Fatal("hestia has no slice drop-in")
	}
	if want := "/etc/systemd/system/user-" + strconv.Itoa(r.UID(hestia)) + ".slice.d/50-snowfarm.conf"; drop.Path != want {
		t.Errorf("slice drop-in at %s, want %s", drop.Path, want)
	}
	want := "[Slice]\nMemoryMax=2048M\nMemoryHigh=1740M\nMemorySwapMax=0\nCPUQuota=150%\n"
	if string(drop.Content) != want {
		t.Errorf("slice drop-in is\n%s\nwant\n%s", drop.Content, want)
	}
	limits := body(t, files, "limits/hestia")
	if limits != "RUN_MEMORY_MAX=1792M\nRUN_TASKS_MAX=512\nRUN_MAX_ITERATIONS=80\n" {
		t.Errorf("run limits are\n%s", limits)
	}
}

func TestGuardUnitKeepsSudoUsable(t *testing.T) {
	unit := body(t, systemFiles(t, loadRoster(t)), "snowfarm-guard.service")

	if strings.Contains(unit, "NoNewPrivileges=") {
		t.Error("NoNewPrivileges= makes the kernel ignore sudo's setuid bit, and every guard action goes through sudo")
	}
	implyNoNewPrivileges := []string{
		"PrivateDevices", "ProtectClock", "ProtectHostname",
		"ProtectKernelTunables", "ProtectKernelModules", "ProtectKernelLogs",
		"ProtectControlGroups", "RestrictAddressFamilies", "RestrictNamespaces",
		"RestrictRealtime", "RestrictSUIDSGID", "LockPersonality",
		"MemoryDenyWriteExecute", "SystemCallFilter", "SystemCallLog",
		"SystemCallArchitectures", "DynamicUser", "CapabilityBoundingSet",
	}
	for _, name := range implyNoNewPrivileges {
		if strings.Contains(unit, name+"=") {
			t.Errorf("%s= installs a seccomp filter, which implies NoNewPrivileges for an unprivileged unit", name)
		}
	}
	if directive(unit, "Group") != "farm-agents" {
		t.Errorf("Group is %q, want farm-agents", directive(unit, "Group"))
	}
	if strings.Contains(unit, "RuntimeDirectory=") {
		t.Error("RuntimeDirectory= would re-chown /run/snowfarm away from farm-agents on every start")
	}
	want := []string{"/var/lib/snowfarm", "/run/snowfarm", "/srv/snowfarm", "/var/lib/farm"}
	if got := strings.Fields(directive(unit, "ReadWritePaths")); !slices.Equal(got, want) {
		t.Errorf("ReadWritePaths is %v, want %v", got, want)
	}
	service := strings.Index(unit, "[Service]")
	for _, key := range []string{"StartLimitIntervalSec=300", "StartLimitBurst=5"} {
		at := strings.Index(unit, key)
		if at < 0 {
			t.Fatalf("the guard unit is missing %s", key)
		}
		if at > service {
			t.Errorf("%s is a [Unit] directive; systemd ignores it under [Service]", key)
		}
	}
	for _, key := range []string{"User=snowfarm", "SupplementaryGroups=systemd-journal", "Type=notify",
		"Restart=always", "RestartSec=5", "WatchdogSec=90", "UMask=0027", "ProtectSystem=strict",
		"ProtectHome=read-only", "PrivateTmp=true", "ExecReload=/bin/kill -HUP $MAINPID"} {
		if !strings.Contains(unit, "\n"+key+"\n") {
			t.Errorf("the guard unit is missing %s", key)
		}
	}
	after := directive(unit, "After")
	for _, ordered := range []string{"systemd-tmpfiles-setup.service", "tailscaled.service"} {
		if !strings.Contains(after, ordered) {
			t.Errorf("After is %q, which does not order the guard after %s", after, ordered)
		}
	}
}

func TestGuardWritesAreCoveredByReadWritePaths(t *testing.T) {
	unit := body(t, systemFiles(t, loadRoster(t)), "snowfarm-guard.service")
	writable := strings.Fields(directive(unit, "ReadWritePaths"))
	writes := []string{
		"/var/lib/snowfarm/ledger.db",
		"/var/lib/snowfarm/log",
		"/var/lib/snowfarm/channels.json",
		"/var/lib/snowfarm/profile-hashes.json",
		"/run/snowfarm/guard.sock",
	}
	for _, write := range writes {
		if !slices.ContainsFunc(writable, func(root string) bool {
			return write == root || strings.HasPrefix(write, root+"/")
		}) {
			t.Errorf("%s is not under any ReadWritePaths root %v; ProtectSystem=strict would refuse the write", write, writable)
		}
	}
}

func TestRunDispatchEnvironment(t *testing.T) {
	r := loadRoster(t)
	script := body(t, systemFiles(t, r), "farm-unitctl")

	want := []string{
		"HOME", "HERMES_HOME", "HERMES_KANBAN_HOME", "HERMES_KANBAN_WORKSPACES_ROOT",
		"HERMES_KANBAN_ATTACHMENTS_ROOT", "HERMES_KANBAN_DISPATCH_IN_GATEWAY",
		"HERMES_MAX_ITERATIONS", "HERMES_REDACT_SECRETS", "HERMES_WRITE_SAFE_ROOT",
	}
	var got []string
	for _, m := range regexp.MustCompile(`--setenv=([A-Z_]+)`).FindAllStringSubmatch(script, -1) {
		got = append(got, m[1])
	}
	if !slices.Equal(got, want) {
		t.Errorf("the dispatched run's --setenv list is\n%v\nwant\n%v", got, want)
	}
	if strings.Contains(script, "HERMES_DOCKER_BINARY") || strings.Contains(script, "docker") {
		t.Error("nothing on this host installs a container runtime")
	}

	kanbanHome := shellVar(script, "HERMES_KANBAN_HOME")
	roots := map[string]string{
		"HERMES_KANBAN_WORKSPACES_ROOT":  r.Farm.WorkspacesRoot,
		"HERMES_KANBAN_ATTACHMENTS_ROOT": r.Farm.AttachmentsRoot,
	}
	for name, expected := range roots {
		got := strings.ReplaceAll(shellVar(script, name), "${HERMES_KANBAN_HOME}", kanbanHome)
		if got != expected {
			t.Errorf("%s renders %q, want %q", name, got, expected)
		}
	}

	if !strings.Contains(script, `. "/usr/local/lib/snowfarm/limits/${name}"`) {
		t.Error("the run limits must come from /usr/local/lib/snowfarm/limits")
	}
	if strings.Contains(script, "/etc/snowfarm") {
		t.Error("/etc/snowfarm is 0700 snowfarm; a farm user cannot traverse it")
	}
	unit := shellVar(script, "unit")
	if !strings.HasSuffix(unit, ".service") {
		t.Errorf("the transient unit name is %q; run-stop and journalctl both expect the .service suffix", unit)
	}
	printed := strings.Index(script, `printf '%s\n' "${unit}"`)
	execed := strings.Index(script, "exec systemd-run")
	if printed < 0 || execed < 0 || printed > execed {
		t.Error("the wrapper must print the unit name before it execs the run")
	}
	if !strings.Contains(script, "run-dispatch) max=1 ;; run-maintenance) max=0 ;;") {
		t.Error("run-dispatch is the spawn pass (--max 1) and run-maintenance spawns nothing (--max 0)")
	}
}

func TestKanbanRootsAgreeAcrossSurfaces(t *testing.T) {
	r := altRoster(t)
	unit := body(t, agentFiles(t, r, "atlas", nil), "user/hermes-gateway.service")
	script := body(t, systemFiles(t, r), "farm-unitctl")

	kanbanHome := shellVar(script, "HERMES_KANBAN_HOME")
	if kanbanHome != r.Farm.KanbanHome {
		t.Fatalf("the wrapper's HERMES_KANBAN_HOME is %q, want %q", kanbanHome, r.Farm.KanbanHome)
	}
	if environment(unit, "HERMES_KANBAN_HOME") != kanbanHome {
		t.Fatalf("the gateway and the wrapper disagree about the board root")
	}
	for _, pair := range [][2]string{
		{"HERMES_KANBAN_WORKSPACES_ROOT", "/srv/board/kanban/workspaces"},
		{"HERMES_KANBAN_ATTACHMENTS_ROOT", "/srv/board/kanban/attachments"},
	} {
		fromScript := strings.ReplaceAll(shellVar(script, pair[0]), "${HERMES_KANBAN_HOME}", kanbanHome)
		fromUnit := environment(unit, pair[0])
		if fromUnit != pair[1] || fromScript != pair[1] {
			t.Errorf("%s: gateway %q, wrapper %q, want %q", pair[0], fromUnit, fromScript, pair[1])
		}
	}
	home := strings.ReplaceAll(shellVar(script, "home"), "${name}", "atlas")
	if want := "/opt/farm/homes/atlas"; home != want || environment(unit, "HOME") != want {
		t.Errorf("HOME: gateway %q, wrapper %q, want %q", environment(unit, "HOME"), home, want)
	}
	if strings.Contains(unit, "HERMES_DOCKER_BINARY") || strings.Contains(script, "HERMES_DOCKER_BINARY") {
		t.Error("no container runtime exists on this host")
	}
}

func TestWorkerHasNoGatewayUnit(t *testing.T) {
	r := loadRoster(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	files, err := Units(r, hestia, r.UID(hestia), nil)
	if err != nil {
		t.Fatal(err)
	}
	var paths []string
	for _, f := range files {
		paths = append(paths, f.Path)
	}
	want := []string{
		"/etc/systemd/system/user-" + strconv.Itoa(r.UID(hestia)) + ".slice.d/50-snowfarm.conf",
		"/usr/local/lib/snowfarm/limits/hestia",
	}
	if !slices.Equal(paths, want) {
		t.Errorf("a worker renders %v, want %v", paths, want)
	}
}

func TestUnitctlAllowList(t *testing.T) {
	script := body(t, systemFiles(t, loadRoster(t)), "farm-unitctl")

	var verbs []string
	for _, m := range regexp.MustCompile(`(?m)^ {2}([a-z|-]+)\)`).FindAllStringSubmatch(script, -1) {
		verbs = append(verbs, strings.Split(m[1], "|")...)
	}
	want := []string{"gateway", "run-dispatch", "run-maintenance", "run-stop", "run-list", "kanban"}
	if !slices.Equal(verbs, want) {
		t.Errorf("the wrapper answers %v, want exactly %v", verbs, want)
	}

	sub := regexp.MustCompile(`case "\$\{1:-\}" in ([a-z|-]+)\) exec \S+ kanban`).FindStringSubmatch(script)
	if sub == nil {
		t.Fatal("the kanban arm has no subverb allow-list")
	}
	wantSub := []string{"create", "notify-subscribe", "notify-unsubscribe", "block", "reclaim", "diagnostics", "list", "show", "stats"}
	if got := strings.Split(sub[1], "|"); !slices.Equal(got, wantSub) {
		t.Errorf("kanban subverbs are %v, want %v", got, wantSub)
	}
	if !strings.Contains(script, `snowfarm-run-"${name}"-*.service)`) {
		t.Error("run-stop must refuse a unit that is not this agent's own run")
	}
}

func TestUnitctlBehaviour(t *testing.T) {
	r := loadRoster(t)
	bin := t.TempDir()
	writeFake(t, bin, "id", "#!/bin/sh\ncase \"$1\" in\n-un) echo farm-hestia ;;\n-u) echo 6003 ;;\n*) exit 1 ;;\nesac\n")
	writeFake(t, bin, "systemctl", "#!/bin/sh\necho \"systemctl $*\"\n")
	writeFake(t, bin, "hermes", "#!/bin/sh\necho \"hermes $*\"\n")
	r.Farm.HermesBin = filepath.Join(bin, "hermes")

	script := filepath.Join(t.TempDir(), "farm-unitctl")
	if err := os.WriteFile(script, []byte(body(t, systemFiles(t, r), "farm-unitctl")), 0o600); err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name     string
		user     string
		args     []string
		exit     int
		contains string
	}{
		{name: "no verb", exit: 2},
		{name: "unknown verb", args: []string{"apply"}, exit: 2},
		{name: "gateway subverb outside the list", args: []string{"gateway", "kill"}, exit: 2},
		{name: "gateway start", args: []string{"gateway", "start"}, contains: "systemctl --user start hermes-gateway.service"},
		{name: "gateway show", args: []string{"gateway", "show"}, contains: "systemctl --user show hermes-gateway.service"},
		{name: "another agent's run", args: []string{"run-stop", "snowfarm-run-atlas-1.service"}, exit: 2},
		{name: "a unit that is not a run", args: []string{"run-stop", "hermes-gateway.service"}, exit: 2},
		{name: "its own run", args: []string{"run-stop", "snowfarm-run-hestia-17.service"}, contains: "systemctl --user stop snowfarm-run-hestia-17.service"},
		{name: "run-list", args: []string{"run-list"}, contains: "snowfarm-run-hestia-*.service"},
		{name: "kanban subverb outside the list", args: []string{"kanban", "delete"}, exit: 2},
		{name: "kanban list", args: []string{"kanban", "list", "--assignee", "hestia"}, contains: "hermes kanban list --assignee hestia"},
		{name: "not a farm user", user: "alice", args: []string{"gateway", "start"}, exit: 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cmd := exec.Command("/bin/sh", append([]string{script}, tc.args...)...)
			cmd.Env = []string{"PATH=" + bin + ":/usr/bin:/bin"}
			if tc.user != "" {
				cmd.Env = append(cmd.Env, "USER="+tc.user)
			}
			out, err := cmd.CombinedOutput()
			exit := 0
			var exitErr *exec.ExitError
			if errors.As(err, &exitErr) {
				exit = exitErr.ExitCode()
			} else if err != nil {
				t.Fatalf("%v: %s", err, out)
			}
			if exit != tc.exit {
				t.Errorf("exit %d, want %d: %s", exit, tc.exit, out)
			}
			if tc.contains != "" && !strings.Contains(string(out), tc.contains) {
				t.Errorf("output %q does not contain %q", out, tc.contains)
			}
		})
	}
}

func writeFake(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestSudoersRule(t *testing.T) {
	rule := body(t, systemFiles(t, loadRoster(t)), "sudoers.d/snowfarm")
	if strings.TrimSpace(rule) != "snowfarm ALL=(%farm-agents) NOPASSWD: /usr/local/sbin/farm-unitctl" {
		t.Errorf("the sudoers rule is %q", rule)
	}
}

func TestTmpfilesEntry(t *testing.T) {
	entry := body(t, systemFiles(t, loadRoster(t)), "tmpfiles.d/snowfarm.conf")
	if strings.TrimSpace(entry) != "d /run/snowfarm 2750 snowfarm farm-agents -" {
		t.Errorf("the tmpfiles entry is %q", entry)
	}
}

func TestCronCommandArgv(t *testing.T) {
	r := loadRoster(t)
	atlas, ok := r.Agent("atlas")
	if !ok {
		t.Fatal("atlas is missing from the fixture")
	}
	const prompt = "Read the board with kanban_list for every team you own. Report, in under 300 words: cards opened, completed, blocked and stale since your last report, and what you intend next. If nothing changed, reply exactly [SILENT]."
	for _, tc := range []struct {
		season string
		now    time.Time
		expr   string
	}{
		{season: "July", now: time.Date(2026, 7, 15, 9, 0, 0, 0, time.UTC), expr: "0 12,16,20,0 * * *"},
		{season: "January", now: time.Date(2027, 1, 15, 9, 0, 0, 0, time.UTC), expr: "0 13,17,21,1 * * *"},
	} {
		t.Run(tc.season, func(t *testing.T) {
			got, err := CronCommand(r, atlas, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"runuser", "-u", "farm-atlas", "--", "env",
				"HOME=/var/lib/farm/atlas",
				"HERMES_HOME=/var/lib/farm/atlas/.hermes/profiles/atlas",
				"/usr/local/bin/hermes", "cron", "create", tc.expr, prompt,
				"--name", "farm-report",
				"--deliver", "discord:#managers",
				"--model", "gpt-5.6-sol",
				"--provider", "custom:modelgate",
				"--reasoning-effort", "low",
			}
			if !slices.Equal(got, want) {
				t.Errorf("argv is\n%q\nwant\n%q", got, want)
			}
			expr, err := CronExpression(r, tc.now)
			if err != nil {
				t.Fatal(err)
			}
			if expr != tc.expr {
				t.Errorf("expression is %q, want %q", expr, tc.expr)
			}
		})
	}
}

func TestCronCommandRefusesAWorker(t *testing.T) {
	r := loadRoster(t)
	hestia, ok := r.Agent("hestia")
	if !ok {
		t.Fatal("hestia is missing from the fixture")
	}
	if _, err := CronCommand(r, hestia, time.Now()); err == nil {
		t.Error("only a manager reports on a schedule")
	}
}
