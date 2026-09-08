package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const fixture = "../../internal/roster/testdata/farm.yaml"

func TestRunDispatchesEveryCommand(t *testing.T) {
	for _, command := range []string{"guard", "reload", "secret-env"} {
		t.Run(command, func(t *testing.T) {
			err := run([]string{command})
			if !errors.Is(err, errNotImplemented) {
				t.Fatalf("%s: %v", command, err)
			}
			if !strings.HasPrefix(err.Error(), command+": ") {
				t.Fatalf("%s: error does not name the command: %v", command, err)
			}
		})
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
