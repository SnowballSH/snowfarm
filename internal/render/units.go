package render

import (
	"fmt"
	"io/fs"
	"path"
	"slices"
	"strings"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	gatewayUnitName = "hermes-gateway.service"
	userUnitDir     = ".config/systemd/user"
	wantsDir        = "default.target.wants"

	pendingChannels = "pending-reconcile"
	commandChannel  = "#command"
	managerChannel  = "#managers"

	guardStateDir   = "/var/lib/snowfarm"
	guardRuntimeDir = "/run/snowfarm"
	farmSharedRoot  = "/srv/snowfarm"
	farmHomeRoot    = "/var/lib/farm"

	guardUnitPath   = "/etc/systemd/system/snowfarm-guard.service"
	guardConfigPath = "/etc/snowfarm/farm.yaml"
	snowfarmBin     = "/usr/local/bin/snowfarm"
	unitctlPath     = "/usr/local/sbin/farm-unitctl"
	sudoersPath     = "/etc/sudoers.d/snowfarm"
	tmpfilesPath    = "/etc/tmpfiles.d/snowfarm.conf"
	runLimitsDir    = "/usr/local/lib/snowfarm/limits"

	agentGroup = "farm-agents"

	unitMode    fs.FileMode = 0o644
	scriptMode  fs.FileMode = 0o755
	sudoersMode fs.FileMode = 0o440

	memoryHighPercent = 85
	runTasksMax       = 512
)

type gatewayData struct {
	Agent           roster.Agent
	Home            string
	HermesHome      string
	KanbanHome      string
	WorkspacesRoot  string
	AttachmentsRoot string
	HermesBin       string
	OperatorUserID  string
	AllowedChannels string
	HomeChannel     string
}

// Units renders what systemd needs for one agent: a manager's gateway unit
// and the wants symlink that enables it, the slice drop-in that caps
// everything the agent runs, and the run limits farm-unitctl sources. ch maps
// channel name to id; a name the guard's reconciler has not resolved yet
// renders as pending-reconcile, which apply --start refuses to start.
func Units(r *roster.Roster, a roster.Agent, uid int, ch map[string]string) ([]File, error) {
	var files []File
	if a.Tier == roster.TierManager {
		unit, err := gatewayUnit(r, a, ch)
		if err != nil {
			return nil, err
		}
		files = append(files, unit, wantsLink(a.Home(r.Farm), unit.Path, a.User()))
	}
	files = append(files, sliceDropIn(a, uid))
	if a.Limits.RunMiB > 0 {
		files = append(files, runLimits(a))
	}
	return files, nil
}

// System renders the host-wide files: the wrapper the guard drives agents
// through, the one sudoers rule that reaches it, the runtime directory the
// secrets socket lives in, and the guard's own unit.
func System(r *roster.Roster) ([]File, error) {
	script, err := unitctlScript(r)
	if err != nil {
		return nil, err
	}
	return []File{
		rootFile(unitctlPath, scriptMode, script),
		rootFile(sudoersPath, sudoersMode, []byte(sudoersRule+"\n")),
		rootFile(tmpfilesPath, unitMode, []byte(tmpfilesEntry+"\n")),
		rootFile(guardUnitPath, unitMode, guardUnit(r.Farm)),
	}, nil
}

func gatewayUnit(r *roster.Roster, a roster.Agent, ch map[string]string) (File, error) {
	content, err := execute("hermes-gateway.service.tmpl", gatewayData{
		Agent:           a,
		Home:            a.Home(r.Farm),
		HermesHome:      a.HermesHome(r.Farm),
		KanbanHome:      r.Farm.KanbanHome,
		WorkspacesRoot:  r.Farm.WorkspacesRoot,
		AttachmentsRoot: r.Farm.AttachmentsRoot,
		HermesBin:       r.Farm.HermesBin,
		OperatorUserID:  r.Farm.Discord.OperatorUserID,
		AllowedChannels: channelIDs(ch, allowedChannels(r, a)),
		HomeChannel:     channelIDs(ch, []string{managerChannel}),
	})
	if err != nil {
		return File{}, fmt.Errorf("agent %q: %w", a.Name, err)
	}
	return File{
		Path:    path.Join(a.Home(r.Farm), userUnitDir, gatewayUnitName),
		Mode:    unitMode,
		Owner:   a.User(),
		Group:   a.User(),
		Content: content,
	}, nil
}

func wantsLink(home, target, user string) File {
	return File{
		Path:    path.Join(home, userUnitDir, wantsDir, gatewayUnitName),
		Mode:    fs.ModeSymlink | 0o777,
		Owner:   user,
		Group:   user,
		Content: []byte(target),
	}
}

func sliceDropIn(a roster.Agent, uid int) File {
	content := fmt.Sprintf("[Slice]\nMemoryMax=%dM\nMemoryHigh=%dM\nMemorySwapMax=0\nCPUQuota=%d%%\n",
		a.Limits.SliceMiB, a.Limits.SliceMiB*memoryHighPercent/100, a.Limits.CPUPercent)
	return rootFile(fmt.Sprintf("/etc/systemd/system/user-%d.slice.d/50-snowfarm.conf", uid), unitMode, []byte(content))
}

func runLimits(a roster.Agent) File {
	content := fmt.Sprintf("RUN_MEMORY_MAX=%dM\nRUN_TASKS_MAX=%d\nRUN_MAX_ITERATIONS=%d\n",
		a.Limits.RunMiB, runTasksMax, a.MaxIterations)
	return rootFile(path.Join(runLimitsDir, a.Name), unitMode, []byte(content))
}

func rootFile(path string, mode fs.FileMode, content []byte) File {
	return File{Path: path, Mode: mode, Owner: rootOwner, Group: rootOwner, Content: content}
}

func allowedChannels(r *roster.Roster, a roster.Agent) []string {
	names := []string{commandChannel, managerChannel}
	for _, t := range teamsOf(r, a) {
		names = append(names, "#"+t.Name+"-general", "#"+t.Name+"-log")
	}
	return names
}

func channelIDs(ch map[string]string, names []string) string {
	ids := make([]string, 0, len(names))
	for _, name := range names {
		id, ok := ch[name]
		if !ok {
			return pendingChannels
		}
		ids = append(ids, id)
	}
	return strings.Join(ids, ",")
}

const sudoersRule = "snowfarm ALL=(%" + agentGroup + ") NOPASSWD: " + unitctlPath

const tmpfilesEntry = "d " + guardRuntimeDir + " 2750 snowfarm " + agentGroup + " -"

const guardUnitTemplate = `[Unit]
Description=snowfarm supervisor
After=network-online.target systemd-tmpfiles-setup.service tailscaled.service
Wants=network-online.target
StartLimitIntervalSec=300
StartLimitBurst=5

[Service]
# sudo needs setuid: a hardening directive that installs a seccomp filter makes
# the kernel drop the setuid bit, and every guard action goes through sudo.
Type=notify
User=snowfarm
Group=` + agentGroup + `
SupplementaryGroups=systemd-journal
ExecStart=` + snowfarmBin + ` guard --config ` + guardConfigPath + `
ExecReload=/bin/kill -HUP $MAINPID
Restart=always
RestartSec=5
WatchdogSec=90
UMask=0027
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=true
ReadWritePaths=%s

[Install]
WantedBy=multi-user.target
`

func guardUnit(f roster.Farm) []byte {
	return fmt.Appendf(nil, guardUnitTemplate, strings.Join(readWritePaths(f), " "))
}

// readWritePaths is the guard's writable set under ProtectSystem=strict. The
// four roots cover the default layout; a roster that moves the board, the
// Claude tree or the homes elsewhere adds the tree it moved to, so a
// customised path can never leave the guard writing into a read-only mount.
func readWritePaths(f roster.Farm) []string {
	paths := []string{guardStateDir, guardRuntimeDir, farmSharedRoot, farmHomeRoot}
	for _, p := range []string{f.KanbanHome, f.ClaudeDir, f.HomeRoot} {
		if !slices.ContainsFunc(paths, func(root string) bool { return p == root || strings.HasPrefix(p, root+"/") }) {
			paths = append(paths, p)
		}
	}
	return paths
}
