package main

import (
	"errors"
	"strings"
	"testing"
)

func TestRunDispatchesEveryCommand(t *testing.T) {
	for _, command := range []string{"plan", "apply", "guard", "reload", "secret-env"} {
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
