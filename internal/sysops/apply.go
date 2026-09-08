package sysops

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/SnowballSH/snowfarm/internal/render"
	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	nologinShell        = "/usr/sbin/nologin"
	guardSearchACL      = "u:" + guardUser + ":rx"
	guardReadACL        = "u:" + guardUser + ":r"
	userManagerAttempts = 30
	userManagerInterval = time.Second
)

// Apply performs the plan. It clears the immutable flags first, because every
// step below writes into the tree they freeze, and restores them from a defer,
// so an apply that dies part way through leaves no writable profile behind.
func (a *Applier) Apply(r *roster.Roster, p Plan) (err error) {
	agents := a.scope(r)
	frozen := a.frozenPaths(r, agents)
	if err := a.thaw(frozen); err != nil {
		return err
	}
	defer func() {
		if relock := a.freeze(frozen); err == nil {
			err = relock
		}
		if err == nil {
			err = a.verifyFrozen(frozen)
		}
	}()

	if err := a.accounts(p, agents); err != nil {
		return err
	}
	for _, c := range p.Dirs {
		if err := a.makeDir(*c.dir); err != nil {
			return err
		}
	}
	for _, agent := range agents {
		if err := a.searchACLs(r, agent); err != nil {
			return err
		}
	}
	for _, c := range slices.Concat(p.Files, p.Units) {
		if err := a.install(*c.file); err != nil {
			return err
		}
	}
	for _, agent := range agents {
		if err := a.readACLs(r, agent); err != nil {
			return err
		}
	}
	for _, c := range p.Reloads {
		if err := a.reload(*c.reload); err != nil {
			return err
		}
	}
	if err := a.reportJobs(r, agents); err != nil {
		return err
	}
	if !a.Start {
		return nil
	}
	return a.startGateways(r, agents)
}

func (a *Applier) accounts(p Plan, agents []roster.Agent) error {
	for _, c := range p.Groups {
		if _, err := a.run("groupadd", "--system", c.Target); err != nil {
			return err
		}
	}
	for _, c := range p.Users {
		if _, err := a.run("useradd", "--system",
			"--uid", strconv.Itoa(c.user.UID),
			"--home-dir", c.user.Home,
			"--create-home",
			"--shell", nologinShell,
			"--user-group", c.user.Name); err != nil {
			return err
		}
	}
	for _, agent := range agents {
		if _, err := a.run("usermod", "-aG", agentGroup, agent.User()); err != nil {
			return err
		}
		if _, err := a.run("loginctl", "enable-linger", agent.User()); err != nil {
			return err
		}
	}
	return nil
}

func (a *Applier) makeDir(d dirSpec) error {
	target := a.path(d.Path)
	if err := os.MkdirAll(target, parentDirMode); err != nil {
		return err
	}
	if err := os.Chmod(target, d.Mode); err != nil {
		return err
	}
	return a.chown(d.Owner, d.Group, target)
}

func (a *Applier) install(f fileSpec) error {
	target := a.path(f.File.Path)
	if err := os.MkdirAll(filepath.Dir(target), parentDirMode); err != nil {
		return err
	}
	switch {
	case f.File.IsSymlink():
		want := a.path(string(f.File.Content))
		if err := os.Remove(target); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		if err := os.Symlink(want, target); err != nil {
			return err
		}
		return a.chown(f.File.Owner, f.File.Group, target)
	case f.Keep:
		file, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY, f.File.Mode)
		if err != nil {
			return err
		}
		if err := file.Close(); err != nil {
			return err
		}
	default:
		if err := writeAtomically(target, f.File.Content, f.File.Mode); err != nil {
			return err
		}
	}
	if err := os.Chmod(target, f.File.Mode); err != nil {
		return err
	}
	return a.chown(f.File.Owner, f.File.Group, target)
}

func writeAtomically(target string, content []byte, mode fs.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(target), "."+filepath.Base(target)+".*")
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp.Name()) }()
	if _, err := tmp.Write(content); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), target)
}

func (a *Applier) chown(owner, group, target string) error {
	_, err := a.run("chown", "-h", owner+":"+group, target)
	return err
}

// searchACLs give the guard, which is in none of the per-agent groups, the
// execute bit it needs on every component of the path to an agent's logs, and
// one default entry so a log subdirectory inherits it.
func (a *Applier) searchACLs(r *roster.Roster, agent roster.Agent) error {
	home := agent.Home(r.Farm)
	profile := agent.HermesHome(r.Farm)
	logs := path.Join(profile, "logs")
	for _, p := range []string{home, path.Join(home, ".hermes"), path.Join(home, ".hermes", "profiles"), profile, logs} {
		if _, err := a.run("setfacl", "-m", guardSearchACL, a.path(p)); err != nil {
			return err
		}
	}
	_, err := a.run("setfacl", "-d", "-m", guardSearchACL, a.path(logs))
	return err
}

// readACLs run after the profile files are installed and before they are
// frozen: a rename installs a new inode, and the kernel refuses setxattr on an
// immutable one.
func (a *Applier) readACLs(r *roster.Roster, agent roster.Agent) error {
	for _, name := range []string{"config.yaml", "SOUL.md"} {
		if _, err := a.run("setfacl", "-m", guardReadACL, a.path(path.Join(agent.HermesHome(r.Farm), name))); err != nil {
			return err
		}
	}
	return nil
}

func (a *Applier) reload(spec reloadSpec) error {
	if spec.UID != 0 {
		if err := a.waitForUserManager(spec.UID); err != nil {
			return err
		}
	}
	_, err := a.run(spec.Argv[0], spec.Argv[1:]...)
	return err
}

// waitForUserManager bounds the gap between enabling linger and the user
// manager's bus being up, which a --user call fails without.
func (a *Applier) waitForUserManager(uid int) error {
	unit := fmt.Sprintf("user@%d.service", uid)
	if _, err := a.run("systemctl", "start", unit); err != nil {
		return err
	}
	var err error
	for attempt := 0; attempt < userManagerAttempts; attempt++ {
		if _, err = a.run("systemctl", "is-active", "--quiet", unit); err == nil {
			return nil
		}
		time.Sleep(userManagerInterval)
	}
	return fmt.Errorf("%s never became active: %w", unit, err)
}

// reportJobs creates each manager's progress report. The manager's own cron
// store is the record that this ran; a store that cannot be read yet is a
// first apply, where creating the job is right.
func (a *Applier) reportJobs(r *roster.Roster, agents []roster.Agent) error {
	for _, agent := range agents {
		if agent.Tier != roster.TierManager {
			continue
		}
		out, err := a.run("runuser", hermesArgs(r, agent, "cron", "list")...)
		if err == nil && strings.Contains(string(out), render.CronJobName) {
			continue
		}
		argv, err := render.CronCommand(r, agent, time.Now())
		if err != nil {
			return err
		}
		if _, err := a.run(argv[0], argv[1:]...); err != nil {
			return err
		}
	}
	return nil
}

func hermesArgs(r *roster.Roster, agent roster.Agent, args ...string) []string {
	argv := []string{
		"-u", agent.User(), "--", "env",
		"HOME=" + agent.Home(r.Farm),
		"HERMES_HOME=" + agent.HermesHome(r.Farm),
		r.Farm.HermesBin,
	}
	return append(argv, args...)
}

func (a *Applier) startGateways(r *roster.Roster, agents []roster.Agent) error {
	pending, err := a.PendingGateways(r)
	if err != nil {
		return err
	}
	for _, agent := range agents {
		if agent.Tier != roster.TierManager || slices.Contains(pending, agent.Name) {
			continue
		}
		if _, err := a.run("runuser", "-u", agent.User(), "--", unitctlPath, "gateway", "start"); err != nil {
			return err
		}
	}
	return nil
}

// frozenPaths is the set chattr +i covers, ordered from the outermost
// directory inwards: the two files, and the two ancestors that would
// otherwise let an agent rename the whole profile aside and rebuild it.
func (a *Applier) frozenPaths(r *roster.Roster, agents []roster.Agent) []string {
	paths := make([]string, 0, 4*len(agents))
	for _, agent := range agents {
		home := agent.Home(r.Farm)
		profile := agent.HermesHome(r.Farm)
		paths = append(paths,
			a.path(path.Join(home, ".hermes")),
			a.path(path.Join(home, ".hermes", "profiles")),
			a.path(path.Join(profile, "config.yaml")),
			a.path(path.Join(profile, "SOUL.md")))
	}
	return paths
}

func (a *Applier) thaw(paths []string) error {
	for _, target := range paths {
		exists, err := exists(target)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := a.run("chattr", "-i", target); err != nil {
			return err
		}
	}
	return nil
}

func (a *Applier) freeze(paths []string) error {
	for _, target := range slices.Backward(paths) {
		exists, err := exists(target)
		if err != nil {
			return err
		}
		if !exists {
			continue
		}
		if _, err := a.run("chattr", "+i", target); err != nil {
			return err
		}
	}
	return nil
}

func (a *Applier) verifyFrozen(paths []string) error {
	for _, target := range paths {
		frozen, err := a.immutable(target)
		if err != nil {
			return err
		}
		if !frozen {
			return fmt.Errorf("%s is not immutable after apply", target)
		}
	}
	return nil
}

func exists(target string) (bool, error) {
	_, err := os.Lstat(target)
	if errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}
