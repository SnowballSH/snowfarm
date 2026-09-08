package unitctl

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	fakebinStateEnv = "SNOWFARM_FAKEBIN_STATE"
	wrapper         = "/usr/local/sbin/farm-unitctl"
	dispatchedUnit  = "snowfarm-run-hestia-1757000000123456789.service"
)

type harness struct {
	t     *testing.T
	state string
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	fakebin, err := filepath.Abs(filepath.Join("testdata", "fakebin"))
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", fakebin+string(os.PathListSeparator)+os.Getenv("PATH"))
	state := t.TempDir()
	t.Setenv(fakebinStateEnv, state)
	return &harness{t: t, state: state}
}

func (h *harness) calls() int {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.state, "calls"))
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		h.t.Fatal(err)
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		h.t.Fatal(err)
	}
	return n
}

func (h *harness) invocation(n int) (bin string, args []string) {
	h.t.Helper()
	data, err := os.ReadFile(filepath.Join(h.state, "argv."+strconv.Itoa(n)))
	if err != nil {
		h.t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSuffix(string(data), "\n"), "\n")
	return lines[0], lines[1:]
}

func TestExecBuildsSudoLine(t *testing.T) {
	h := newHarness(t)
	unit, err := NewExec("sudo", wrapper).RunDispatch(context.Background(), "hestia")
	if err != nil {
		t.Fatal(err)
	}
	if unit != dispatchedUnit {
		t.Errorf("RunDispatch returned %q, want %q", unit, dispatchedUnit)
	}
	if h.calls() != 1 {
		t.Fatalf("sudo ran %d times, want 1", h.calls())
	}
	bin, args := h.invocation(1)
	if filepath.Base(bin) != "sudo" {
		t.Errorf("ran %q, want sudo from PATH", bin)
	}
	want := []string{"-n", "-u", "farm-hestia", "--", wrapper, "run-dispatch"}
	if !slices.Equal(args, want) {
		t.Errorf("sudo argv %v, want %v", args, want)
	}
}

func TestExecArgv(t *testing.T) {
	cases := []struct {
		name string
		call func(Controller) error
		want []string
	}{
		{
			name: "gateway restart",
			call: func(c Controller) error {
				_, err := c.Gateway(context.Background(), "atlas", "restart")
				return err
			},
			want: []string{"-n", "-u", "farm-atlas", "--", wrapper, "gateway", "restart"},
		},
		{
			name: "maintenance pass",
			call: func(c Controller) error {
				_, err := c.RunMaintenance(context.Background(), "hestia")
				return err
			},
			want: []string{"-n", "-u", "farm-hestia", "--", wrapper, "run-maintenance"},
		},
		{
			name: "run stop",
			call: func(c Controller) error {
				return c.RunStop(context.Background(), "hestia", dispatchedUnit)
			},
			want: []string{"-n", "-u", "farm-hestia", "--", wrapper, "run-stop", dispatchedUnit},
		},
		{
			name: "run list",
			call: func(c Controller) error {
				_, err := c.RunList(context.Background(), "hestia")
				return err
			},
			want: []string{"-n", "-u", "farm-hestia", "--", wrapper, "run-list"},
		},
		{
			name: "kanban",
			call: func(c Controller) error {
				_, err := c.Kanban(context.Background(), "hestia", "block", "t_1", "--kind", "transient")
				return err
			},
			want: []string{"-n", "-u", "farm-hestia", "--", wrapper, "kanban", "block", "t_1", "--kind", "transient"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := tc.call(NewExec("sudo", wrapper)); err != nil {
				t.Fatal(err)
			}
			_, args := h.invocation(1)
			if !slices.Equal(args, tc.want) {
				t.Errorf("sudo argv %v, want %v", args, tc.want)
			}
		})
	}
}

func TestExecRefusesBeforeSudo(t *testing.T) {
	cases := []struct {
		name string
		call func(Controller) error
	}{
		{
			name: "a gateway verb outside the wrapper's list",
			call: func(c Controller) error {
				_, err := c.Gateway(context.Background(), "hestia", "enable")
				return err
			},
		},
		{
			name: "another agent's run unit",
			call: func(c Controller) error {
				return c.RunStop(context.Background(), "hestia", "snowfarm-run-atlas-1.service")
			},
		},
		{
			name: "a unit that is not a run",
			call: func(c Controller) error {
				return c.RunStop(context.Background(), "hestia", "hermes-gateway.service")
			},
		},
		{
			name: "a run unit without its suffix",
			call: func(c Controller) error {
				return c.RunStop(context.Background(), "hestia", "snowfarm-run-hestia-1")
			},
		},
		{
			name: "a kanban subverb outside the wrapper's list",
			call: func(c Controller) error {
				_, err := c.Kanban(context.Background(), "hestia", "archive", "t_1")
				return err
			},
		},
		{
			name: "kanban without a subverb",
			call: func(c Controller) error {
				_, err := c.Kanban(context.Background(), "hestia")
				return err
			},
		},
		{
			name: "an agent name no roster can hold",
			call: func(c Controller) error {
				_, err := c.RunDispatch(context.Background(), "hestia; rm -rf /")
				return err
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			if err := tc.call(NewExec("sudo", wrapper)); err == nil {
				t.Fatal("call succeeded, want a refusal")
			}
			if h.calls() != 0 {
				t.Errorf("sudo ran %d times, want none", h.calls())
			}
		})
	}
}

func TestRunListReturnsThisAgentsUnits(t *testing.T) {
	newHarness(t)
	units, err := NewExec("sudo", wrapper).RunList(context.Background(), "hestia")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"snowfarm-run-hestia-1.service", "snowfarm-run-hestia-2.service"}
	if !slices.Equal(units, want) {
		t.Errorf("RunList returned %v, want %v", units, want)
	}
}

func TestExecReportsFailureWithOutput(t *testing.T) {
	newHarness(t)
	out, err := NewExec("sudo", wrapper).Gateway(context.Background(), "ghost", "is-active")
	if err == nil {
		t.Fatal("Gateway succeeded, want the fake's failure")
	}
	if strings.TrimSpace(string(out)) != "inactive" {
		t.Errorf("Gateway returned %q, want the command's stdout", out)
	}
	if !strings.Contains(err.Error(), "a password is required") {
		t.Errorf("error %q carries no stderr", err)
	}
	if !strings.Contains(err.Error(), "farm-ghost") {
		t.Errorf("error %q does not name what ran", err)
	}
}

func TestPassRejectsAUnitItCouldNotStop(t *testing.T) {
	for _, agent := range []string{"impostor", "mute"} {
		t.Run(agent, func(t *testing.T) {
			newHarness(t)
			unit, err := NewExec("sudo", wrapper).RunDispatch(context.Background(), agent)
			if err == nil {
				t.Fatalf("RunDispatch returned %q, want a refusal", unit)
			}
		})
	}
}
