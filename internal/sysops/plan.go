package sysops

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"github.com/SnowballSH/snowfarm/internal/render"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	guardUser     = "snowfarm"
	agentGroup    = "farm-agents"
	rootOwner     = "root"
	guardStateDir = "/var/lib/snowfarm"
	sharedRoot    = "/srv/snowfarm"
	systemUnitDir = "/etc/systemd/system"
	libDir        = "/usr/local/lib/snowfarm"

	profileHashPath = guardStateDir + "/profile-hashes.json"
	channelsPath    = guardStateDir + "/channels.json"
	tmpfilesPath    = "/etc/tmpfiles.d/snowfarm.conf"
	unitctlPath     = "/usr/local/sbin/farm-unitctl"

	userUnitDir     = ".config/systemd/user"
	gatewayUnitName = "hermes-gateway.service"

	// pendingChannels is what the renderer writes into a manager unit whose
	// channel ids the guard has not resolved yet; a unit carrying it is
	// installed but never started.
	pendingChannels = "pending-reconcile"
)

const (
	dirMode        fs.FileMode = 0o750
	traversedMode  fs.FileMode = 0o755
	boardMode      fs.FileMode = 0o770 | fs.ModeSetgid
	ledgerMode     fs.FileMode = 0o640
	slotMode       fs.FileMode = 0o640
	hashesMode     fs.FileMode = 0o640
	parentDirMode  fs.FileMode = 0o755
	profileDirMode fs.FileMode = 0o755
)

// Change is one difference between the roster and the host, carrying what
// Apply needs to close it.
type Change struct {
	Target string
	Detail string

	dir    *dirSpec
	file   *fileSpec
	user   *userSpec
	reload *reloadSpec
}

// Plan is the whole difference, in the order Apply performs it.
type Plan struct {
	Users, Groups, Dirs, Files, Units, Reloads []Change
}

func (p Plan) Empty() bool {
	return len(p.Users)+len(p.Groups)+len(p.Dirs)+len(p.Files)+len(p.Units)+len(p.Reloads) == 0
}

func (p Plan) String() string {
	var b strings.Builder
	for _, section := range []struct {
		name    string
		changes []Change
	}{
		{"group", p.Groups}, {"user", p.Users}, {"dir", p.Dirs},
		{"file", p.Files}, {"unit", p.Units}, {"reload", p.Reloads},
	} {
		for _, c := range section.changes {
			fmt.Fprintf(&b, "%-6s %s: %s\n", section.name, c.Target, c.Detail)
		}
	}
	return b.String()
}

type dirSpec struct {
	Path   string
	Mode   fs.FileMode
	Owner  string
	Group  string
	Agent  string
	Frozen bool
}

type fileSpec struct {
	File   render.File
	Agent  string
	Frozen bool
	// Keep marks a file whose existence, mode and owner are managed but
	// whose content belongs to whatever writes it: the run ledgers and the
	// Claude Code slot locks.
	Keep bool
}

type userSpec struct {
	Name  string
	Agent string
	UID   int
	Home  string
}

type reloadSpec struct {
	UID  int
	Argv []string
}

type state struct {
	agents []roster.Agent
	groups []string
	users  []userSpec
	dirs   []dirSpec
	files  []fileSpec
	units  []fileSpec
}

// Plan diffs the roster against the host. ch maps channel name to id for the
// manager units; a nil map renders placeholders that Apply refuses to start.
func (a *Applier) Plan(r *roster.Roster, ch map[string]string) (Plan, error) {
	s, err := a.desired(r, ch)
	if err != nil {
		return Plan{}, err
	}
	var p Plan
	touched := map[string]bool{}
	reloadSystem, reloadTmpfiles := false, false

	for _, group := range s.groups {
		if _, err := a.run("getent", "group", group); err != nil {
			p.Groups = append(p.Groups, Change{Target: group, Detail: "create system group"})
		}
	}
	for _, u := range s.users {
		if _, ok := a.lookup(u.Name); ok {
			continue
		}
		touched[u.Agent] = true
		p.Users = append(p.Users, Change{
			Target: u.Name,
			Detail: fmt.Sprintf("create system user uid %d home %s", u.UID, u.Home),
			user:   &u,
		})
	}
	for _, d := range s.dirs {
		change, changed, err := a.dirChange(d)
		if err != nil {
			return Plan{}, err
		}
		if changed {
			touched[d.Agent] = true
			p.Dirs = append(p.Dirs, change)
		}
	}
	for _, group := range []struct {
		files []fileSpec
		into  *[]Change
	}{{s.files, &p.Files}, {s.units, &p.Units}} {
		for _, f := range group.files {
			change, changed, err := a.fileChange(f)
			if err != nil {
				return Plan{}, err
			}
			if !changed {
				continue
			}
			touched[f.Agent] = true
			reloadSystem = reloadSystem || strings.HasPrefix(f.File.Path, systemUnitDir+"/")
			reloadTmpfiles = reloadTmpfiles || f.File.Path == tmpfilesPath
			*group.into = append(*group.into, change)
		}
	}

	if reloadSystem {
		p.Reloads = append(p.Reloads, Change{
			Target: systemUnitDir,
			Detail: "systemctl daemon-reload",
			reload: &reloadSpec{Argv: []string{"systemctl", "daemon-reload"}},
		})
	}
	if reloadTmpfiles {
		p.Reloads = append(p.Reloads, Change{
			Target: tmpfilesPath,
			Detail: "systemd-tmpfiles --create",
			reload: &reloadSpec{Argv: []string{"systemd-tmpfiles", "--create", a.path(tmpfilesPath)}},
		})
	}
	for _, agent := range s.agents {
		if !touched[agent.Name] {
			continue
		}
		uid := r.UID(agent)
		p.Reloads = append(p.Reloads, Change{
			Target: agent.User(),
			Detail: "systemctl --user daemon-reload",
			reload: &reloadSpec{UID: uid, Argv: []string{"systemctl", "--user", "-M", agent.User() + "@", "daemon-reload"}},
		})
	}
	return p, nil
}

// PendingGateways names the enabled managers whose installed unit still holds
// unresolved channel ids, which is what keeps Apply from starting them.
func (a *Applier) PendingGateways(r *roster.Roster) ([]string, error) {
	var pending []string
	for _, m := range a.scope(r) {
		if m.Tier != roster.TierManager {
			continue
		}
		unit, err := os.ReadFile(a.path(path.Join(m.Home(r.Farm), userUnitDir, gatewayUnitName)))
		if err != nil {
			return nil, err
		}
		if bytes.Contains(unit, []byte(pendingChannels)) {
			pending = append(pending, m.Name)
		}
	}
	return pending, nil
}

// Channels is the channel map the guard's reconciler leaves for apply. A host
// whose guard has never run has no file, and every manager unit renders
// placeholders.
func (a *Applier) Channels() (map[string]string, error) {
	data, err := os.ReadFile(a.path(channelsPath))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var ch map[string]string
	if err := json.Unmarshal(data, &ch); err != nil {
		return nil, fmt.Errorf("%s: %w", channelsPath, err)
	}
	return ch, nil
}

func (a *Applier) scope(r *roster.Roster) []roster.Agent {
	agents := r.EnabledAgents()
	if len(a.Only) == 0 {
		return agents
	}
	return slices.DeleteFunc(agents, func(agent roster.Agent) bool {
		return !slices.Contains(a.Only, agent.Name)
	})
}

func (a *Applier) desired(r *roster.Roster, ch map[string]string) (state, error) {
	s := state{agents: a.scope(r), groups: []string{agentGroup}}
	s.dirs = sharedDirs(r)
	system, err := render.System(r)
	if err != nil {
		return state{}, err
	}
	s.files = shared(system)
	s.files = append(s.files, slotFiles(r)...)

	profiles := map[string]profileHash{}
	for _, agent := range s.agents {
		s.users = append(s.users, userSpec{
			Name:  agent.User(),
			Agent: agent.Name,
			UID:   r.UID(agent),
			Home:  a.path(agent.Home(r.Farm)),
		})
		s.dirs = append(s.dirs, agentDirs(r, agent)...)

		files, err := render.Hermes(r, agent)
		if err != nil {
			return state{}, err
		}
		hash := profileHash{}
		for _, f := range files {
			frozen := isProfileFile(f.Path)
			switch path.Base(f.Path) {
			case "config.yaml":
				hash.Config = sum(f.Content)
			case "SOUL.md":
				hash.Soul = sum(f.Content)
			}
			s.files = append(s.files, fileSpec{File: f, Agent: agent.Name, Frozen: frozen})
		}
		profiles[agent.Name] = hash
		s.files = append(s.files, ledgerFile(r, agent))

		units, err := render.Units(r, agent, r.UID(agent), ch)
		if err != nil {
			return state{}, err
		}
		for _, f := range units {
			s.units = append(s.units, fileSpec{File: f, Agent: agent.Name})
		}
	}
	hashes, err := a.profileHashFile(r, profiles)
	if err != nil {
		return state{}, err
	}
	s.files = append(s.files, hashes)
	return s, nil
}

func sharedDirs(r *roster.Roster) []dirSpec {
	return []dirSpec{
		{Path: guardStateDir, Mode: dirMode, Owner: guardUser, Group: guardUser},
		{Path: sharedRoot, Mode: traversedMode, Owner: rootOwner, Group: rootOwner},
		{Path: r.Farm.KanbanHome, Mode: boardMode, Owner: rootOwner, Group: agentGroup},
		{Path: r.Farm.ClaudeDir, Mode: traversedMode, Owner: rootOwner, Group: rootOwner},
		{Path: path.Join(r.Farm.ClaudeDir, "shared"), Mode: dirMode, Owner: guardUser, Group: agentGroup},
		{Path: path.Join(r.Farm.ClaudeDir, "shared", "slots"), Mode: dirMode, Owner: guardUser, Group: agentGroup},
		{Path: path.Join(r.Farm.ClaudeDir, "runs"), Mode: dirMode, Owner: guardUser, Group: agentGroup},
		{Path: libDir, Mode: traversedMode, Owner: rootOwner, Group: rootOwner},
		{Path: path.Join(libDir, "limits"), Mode: traversedMode, Owner: rootOwner, Group: rootOwner},
	}
}

// agentDirs is the agent's own tree. The two ancestors of the profile are
// agent-owned and 0755 so V6d's red side can fall, and frozen so the agent
// cannot rename or replace the profile the flags on the files protect.
func agentDirs(r *roster.Roster, agent roster.Agent) []dirSpec {
	home := agent.Home(r.Farm)
	profile := agent.HermesHome(r.Farm)
	owner, group := agent.User(), agent.User()
	dirs := []dirSpec{
		{Path: home, Mode: dirMode, Owner: owner, Group: group, Agent: agent.Name},
		{Path: path.Join(home, ".hermes"), Mode: profileDirMode, Owner: owner, Group: group, Agent: agent.Name, Frozen: true},
		{Path: path.Join(home, ".hermes", "profiles"), Mode: profileDirMode, Owner: owner, Group: group, Agent: agent.Name, Frozen: true},
		{Path: profile, Mode: dirMode, Owner: owner, Group: group, Agent: agent.Name},
		{Path: path.Join(profile, "logs"), Mode: dirMode, Owner: owner, Group: group, Agent: agent.Name},
	}
	if agent.Tier != roster.TierManager {
		return dirs
	}
	for _, dir := range []string{
		".config",
		".config/systemd",
		".config/systemd/user",
		".config/systemd/user/default.target.wants",
	} {
		dirs = append(dirs, dirSpec{Path: path.Join(home, dir), Mode: dirMode, Owner: owner, Group: group, Agent: agent.Name})
	}
	return dirs
}

func shared(files []render.File) []fileSpec {
	out := make([]fileSpec, 0, len(files))
	for _, f := range files {
		out = append(out, fileSpec{File: f})
	}
	return out
}

func slotFiles(r *roster.Roster) []fileSpec {
	slots := make([]fileSpec, 0, r.Guard.MaxClaudeSlots)
	for slot := 1; slot <= r.Guard.MaxClaudeSlots; slot++ {
		slots = append(slots, fileSpec{Keep: true, File: render.File{
			Path:  path.Join(r.Farm.ClaudeDir, "shared", "slots", strconv.Itoa(slot)),
			Mode:  slotMode,
			Owner: guardUser,
			Group: agentGroup,
		}})
	}
	return slots
}

func ledgerFile(r *roster.Roster, agent roster.Agent) fileSpec {
	return fileSpec{Agent: agent.Name, Keep: true, File: render.File{
		Path:  path.Join(r.Farm.ClaudeDir, "runs", agent.Name+".jsonl"),
		Mode:  ledgerMode,
		Owner: agent.User(),
		Group: agentGroup,
	}}
}

type profileHash struct {
	Config string `json:"config.yaml"`
	Soul   string `json:"SOUL.md"`
}

// profileHashFile is the baseline the guard's hygiene sweep compares against.
// An agent outside this apply's scope keeps the hash the apply that installed
// it recorded; an agent off the roster loses its entry.
func (a *Applier) profileHashFile(r *roster.Roster, planned map[string]profileHash) (fileSpec, error) {
	hashes := map[string]profileHash{}
	data, err := os.ReadFile(a.path(profileHashPath))
	switch {
	case err == nil:
		if err := json.Unmarshal(data, &hashes); err != nil {
			return fileSpec{}, fmt.Errorf("%s: %w", profileHashPath, err)
		}
	case !errors.Is(err, fs.ErrNotExist):
		return fileSpec{}, err
	}
	for name := range hashes {
		if _, ok := r.Agent(name); !ok {
			delete(hashes, name)
		}
	}
	for name, hash := range planned {
		hashes[name] = hash
	}
	content, err := json.MarshalIndent(hashes, "", "  ")
	if err != nil {
		return fileSpec{}, err
	}
	return fileSpec{File: render.File{
		Path:    profileHashPath,
		Mode:    hashesMode,
		Owner:   guardUser,
		Group:   guardUser,
		Content: append(content, '\n'),
	}}, nil
}

func sum(content []byte) string {
	digest := sha256.Sum256(content)
	return hex.EncodeToString(digest[:])
}

func isProfileFile(p string) bool {
	base := path.Base(p)
	return base == "config.yaml" || base == "SOUL.md"
}

func (a *Applier) dirChange(d dirSpec) (Change, bool, error) {
	target := a.path(d.Path)
	info, err := os.Lstat(target)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return Change{
			Target: d.Path,
			Detail: fmt.Sprintf("create %s %s:%s", modeString(d.Mode), d.Owner, d.Group),
			dir:    &d,
		}, true, nil
	case err != nil:
		return Change{}, false, err
	case !info.IsDir():
		return Change{Target: d.Path, Detail: "not a directory", dir: &d}, true, nil
	}
	var reasons []string
	if got := permBits(info.Mode()); got != d.Mode {
		reasons = append(reasons, fmt.Sprintf("mode %s, want %s", modeString(got), modeString(d.Mode)))
	}
	if d.Frozen {
		if !a.ownedBy(info, d.Owner) {
			reasons = append(reasons, "owner is not "+d.Owner)
		}
		frozen, err := a.immutable(target)
		if err != nil {
			return Change{}, false, err
		}
		if !frozen {
			reasons = append(reasons, "immutable flag missing")
		}
	}
	if len(reasons) == 0 {
		return Change{}, false, nil
	}
	return Change{Target: d.Path, Detail: strings.Join(reasons, ", "), dir: &d}, true, nil
}

func (a *Applier) fileChange(f fileSpec) (Change, bool, error) {
	target := a.path(f.File.Path)
	info, err := os.Lstat(target)
	missing := errors.Is(err, fs.ErrNotExist)
	if err != nil && !missing {
		return Change{}, false, err
	}
	change := Change{Target: f.File.Path, file: &f}
	if f.File.IsSymlink() {
		want := a.path(string(f.File.Content))
		switch {
		case missing:
			change.Detail = "create symlink to " + want
			return change, true, nil
		case info.Mode()&fs.ModeSymlink == 0:
			change.Detail = "not a symlink, want a link to " + want
			return change, true, nil
		}
		got, err := os.Readlink(target)
		if err != nil {
			return Change{}, false, err
		}
		if got != want {
			change.Detail = "links to " + got + ", want " + want
			return change, true, nil
		}
		return Change{}, false, nil
	}
	if missing {
		change.Detail = fmt.Sprintf("create %s %s:%s", modeString(f.File.Mode), f.File.Owner, f.File.Group)
		markPending(&change)
		return change, true, nil
	}
	if !info.Mode().IsRegular() {
		change.Detail = "not a regular file"
		return change, true, nil
	}
	var reasons []string
	if !f.Keep {
		content, err := os.ReadFile(target)
		if err != nil {
			return Change{}, false, err
		}
		if !bytes.Equal(content, f.File.Content) {
			reasons = append(reasons, "content differs")
		}
	}
	if got := permBits(info.Mode()); got != f.File.Mode {
		reasons = append(reasons, fmt.Sprintf("mode %s, want %s", modeString(got), modeString(f.File.Mode)))
	}
	if f.Frozen {
		frozen, err := a.immutable(target)
		if err != nil {
			return Change{}, false, err
		}
		if !frozen {
			reasons = append(reasons, "immutable flag missing")
		}
	}
	if len(reasons) == 0 {
		return Change{}, false, nil
	}
	change.Detail = strings.Join(reasons, ", ")
	markPending(&change)
	return change, true, nil
}

// markPending says of a manager unit whose channel ids are unresolved that
// apply installs it and leaves the gateway stopped.
func markPending(c *Change) {
	if path.Base(c.file.File.Path) != gatewayUnitName || c.file.File.IsSymlink() {
		return
	}
	if bytes.Contains(c.file.File.Content, []byte(pendingChannels)) {
		c.Detail += " (" + pendingChannels + ", not started)"
	}
}

func (a *Applier) immutable(target string) (bool, error) {
	out, err := a.run("lsattr", "-d", target)
	if err != nil {
		return false, err
	}
	fields := strings.Fields(string(out))
	if len(fields) == 0 {
		return false, fmt.Errorf("lsattr -d %s: no attributes reported", target)
	}
	return strings.Contains(fields[0], "i"), nil
}

func (a *Applier) ownedBy(info fs.FileInfo, owner string) bool {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return true
	}
	uid, ok := a.lookup(owner)
	return ok && uint32(uid) == stat.Uid //nolint:gosec // a uid from the account database is never negative
}

func permBits(mode fs.FileMode) fs.FileMode {
	return mode & (fs.ModePerm | fs.ModeSetgid | fs.ModeSetuid | fs.ModeSticky)
}

func modeString(mode fs.FileMode) string {
	bits := uint32(mode.Perm())
	for _, special := range []struct {
		flag fs.FileMode
		bit  uint32
	}{{fs.ModeSetuid, 0o4000}, {fs.ModeSetgid, 0o2000}, {fs.ModeSticky, 0o1000}} {
		if mode&special.flag != 0 {
			bits |= special.bit
		}
	}
	return fmt.Sprintf("%04o", bits)
}
