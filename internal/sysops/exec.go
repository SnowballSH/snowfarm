package sysops

import (
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
)

// Applier turns a roster into host state under Root. Every privileged or
// flag-setting operation goes through Run, and every account question through
// Lookup, so a test can drive the whole apply without root and without a
// filesystem that carries file attributes.
type Applier struct {
	Root   string
	Run    func(name string, args ...string) ([]byte, error)
	Lookup func(user string) (uid int, ok bool)

	// Only restricts every change to the named agents and the directories
	// and system files they share; nil applies the whole enabled roster.
	Only []string

	// Start makes Apply start each manager gateway whose unit carries
	// resolved channel ids.
	Start bool

	// Warn receives the diagnostics an apply reports without stopping for:
	// a probe whose failure is indistinguishable from the state it probes
	// for. A nil Warn discards them.
	Warn io.Writer
}

func (a *Applier) warnf(format string, args ...any) {
	if a.Warn == nil {
		return
	}
	_, _ = fmt.Fprintf(a.Warn, format+"\n", args...)
}

func (a *Applier) run(name string, args ...string) ([]byte, error) {
	if a.Run != nil {
		return a.Run(name, args...)
	}
	return Command(name, args...)
}

func (a *Applier) lookup(name string) (int, bool) {
	if a.Lookup != nil {
		return a.Lookup(name)
	}
	return LookupUser(name)
}

func (a *Applier) path(p string) string {
	return filepath.Join(a.Root, filepath.FromSlash(p))
}

// Command is the default Run: the host command, its output captured, and a
// failure that names what ran and what it printed.
func Command(name string, args ...string) ([]byte, error) {
	out, err := exec.Command(name, args...).CombinedOutput() // #nosec G204 -- an applier's argv is host commands built from the roster
	if err != nil {
		return out, fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, bytes.TrimSpace(out))
	}
	return out, nil
}

// LookupUser is the default Lookup, reading the host account database.
func LookupUser(name string) (int, bool) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, false
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return 0, false
	}
	return uid, true
}
