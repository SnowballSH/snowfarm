package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const fixture = "../../internal/roster/testdata/farm.yaml"

// Every command names itself in its errors, so an operator reading the
// journal knows which one refused.
func TestRunDispatchesEveryCommand(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "absent")
	for command, args := range map[string][]string{
		"guard":  {"guard", "--config", missing},
		"reload": {"reload", "--pidfile", missing},
	} {
		t.Run(command, func(t *testing.T) {
			err := run(args)
			if err == nil {
				t.Fatalf("%s accepted a path that does not exist", command)
			}
			if !strings.HasPrefix(err.Error(), command+": ") {
				t.Fatalf("%s: error does not name the command: %v", command, err)
			}
		})
	}
}

// `snowfarm reload` reaches the running guard through the pid file the guard
// wrote, because the guard treats SIGHUP as a reload and nothing else
// addresses it from another process.
func TestReloadSignalsThePIDInTheFile(t *testing.T) {
	hup := make(chan os.Signal, 1)
	signal.Notify(hup, syscall.SIGHUP)
	defer signal.Stop(hup)

	pidfile := filepath.Join(t.TempDir(), "guard.pid")
	if err := os.WriteFile(pidfile, fmt.Appendf(nil, "%d\n", os.Getpid()), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	if err := run([]string{"reload", "--pidfile", pidfile}); err != nil {
		t.Fatalf("reload: %v", err)
	}
	select {
	case <-hup:
	case <-time.After(5 * time.Second):
		t.Fatal("reload delivered no SIGHUP")
	}
}

func TestReloadRefusesAPIDFileThatIsNotOne(t *testing.T) {
	pidfile := filepath.Join(t.TempDir(), "guard.pid")
	if err := os.WriteFile(pidfile, []byte("not a pid\n"), 0o600); err != nil {
		t.Fatalf("write pidfile: %v", err)
	}
	err := run([]string{"reload", "--pidfile", pidfile})
	if err == nil || !strings.Contains(err.Error(), "does not hold a pid") {
		t.Fatalf("reload: %v", err)
	}
}

func TestRunRejectsUnknownAndEmpty(t *testing.T) {
	if err := run(nil); err == nil || !strings.Contains(err.Error(), "usage:") {
		t.Fatalf("no arguments: %v", err)
	}
	err := run([]string{"deploy"})
	if err == nil || !strings.Contains(err.Error(), `unknown command "deploy"`) {
		t.Fatalf("unknown command: %v", err)
	}
}

func TestVersionIsReplaceableAtLinkTime(t *testing.T) {
	if version == "" {
		t.Fatal("version must carry a default for an unstamped build")
	}
	if err := run([]string{"version"}); err != nil {
		t.Fatal(err)
	}
}

// A roster whose agents are not all enabled, and the only-selection checks
// that must refuse before either command reaches the host.
func TestApplyRefusesOnlyNamingDisabledAgent(t *testing.T) {
	config := rosterWithDisabledHypatia(t)
	for _, command := range []string{"plan", "apply"} {
		t.Run(command, func(t *testing.T) {
			err := run([]string{command, "--config", config, "--only", "hypatia"})
			if err == nil || !strings.Contains(err.Error(), "disabled") {
				t.Fatalf("%s: %v", command, err)
			}
		})
	}
}

func TestApplyRefusesOnlyNamingUnknownAgent(t *testing.T) {
	err := run([]string{"apply", "--config", fixture, "--only", "prometheus"})
	if err == nil || !strings.Contains(err.Error(), "not in the roster") {
		t.Fatalf("%v", err)
	}
}

func TestPlanRefusesStart(t *testing.T) {
	err := run([]string{"plan", "--config", fixture, "--start"})
	if err == nil || !strings.Contains(err.Error(), "--start belongs to apply") {
		t.Fatalf("%v", err)
	}
}

func TestApplyRejectsAPositionalArgument(t *testing.T) {
	err := run([]string{"apply", "everything"})
	if err == nil || !strings.Contains(err.Error(), `unexpected argument "everything"`) {
		t.Fatalf("%v", err)
	}
}

func TestSecretEnvRejectsAPositionalArgument(t *testing.T) {
	err := run([]string{"secret-env", "hestia"})
	if err == nil || !strings.Contains(err.Error(), `unexpected argument "hestia"`) {
		t.Fatalf("%v", err)
	}
}

func rosterWithDisabledHypatia(t *testing.T) string {
	t.Helper()
	data, err := os.ReadFile(fixture)
	if err != nil {
		t.Fatal(err)
	}
	const anchor = `  - name: hypatia
    tier: worker`
	text := strings.Replace(string(data), anchor, `  - name: hypatia
    enabled: false
    tier: worker`, 1)
	if text == string(data) {
		t.Fatal("the fixture no longer carries hypatia")
	}
	text = strings.Replace(text, `    enabled: true
    modelgate_key_minted: ""
  - name: daedalus`, `    modelgate_key_minted: ""
  - name: daedalus`, 1)
	path := filepath.Join(t.TempDir(), "farm.yaml")
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
