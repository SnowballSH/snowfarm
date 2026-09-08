package dispatch

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func writeProcFile(t *testing.T, root string, pid int, name, body string) {
	t.Helper()
	dir := filepath.Join(root, strconv.Itoa(pid))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestUnitForPIDReadsTheRunUnit(t *testing.T) {
	root := t.TempDir()
	writeProcFile(t, root, 4242, "cgroup",
		"0::/user.slice/user-6003.slice/user@6003.service/app.slice/"+
			"snowfarm-run-hestia-1757000000123456789.service\n")

	unit, err := procFS(root).unitForPID(4242)
	if err != nil {
		t.Fatal(err)
	}
	if unit != "snowfarm-run-hestia-1757000000123456789.service" {
		t.Fatalf("unitForPID = %q, want the trailing run unit", unit)
	}
}

func TestUnitForPIDRefusesForeignCgroup(t *testing.T) {
	root := t.TempDir()
	writeProcFile(t, root, 77, "cgroup", "0::/user.slice/user-6003.slice/session-3.scope\n")

	if _, err := procFS(root).unitForPID(77); err == nil {
		t.Fatal("unitForPID named a unit for a process outside any run")
	}
}

func TestUnitForPIDReportsAMissingProcess(t *testing.T) {
	if _, err := procFS(t.TempDir()).unitForPID(4242); err == nil {
		t.Fatal("unitForPID succeeded for a process that is gone")
	}
}

func TestPidAliveReadsProcessState(t *testing.T) {
	root := t.TempDir()
	writeProcFile(t, root, 1, "status", "Name:\thermes\nState:\tS (sleeping)\n")
	writeProcFile(t, root, 2, "status", "Name:\thermes\nState:\tZ (zombie)\n")

	if !procFS(root).alive(1) {
		t.Error("a sleeping process reads as dead")
	}
	if procFS(root).alive(2) {
		t.Error("a zombie reads as alive")
	}
	if procFS(root).alive(3) {
		t.Error("a process with no status file reads as alive")
	}
}
