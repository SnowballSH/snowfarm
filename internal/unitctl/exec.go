package unitctl

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
)

type execController struct {
	sudo    string
	wrapper string
}

// NewExec returns the Controller the guard runs on the host. An empty sudo or
// wrapper takes the installed path.
func NewExec(sudo, wrapper string) Controller {
	if sudo == "" {
		sudo = DefaultSudo
	}
	if wrapper == "" {
		wrapper = DefaultWrapper
	}
	return &execController{sudo: sudo, wrapper: wrapper}
}

func (c *execController) Gateway(ctx context.Context, agent, verb string) ([]byte, error) {
	if err := checkAgent(agent); err != nil {
		return nil, err
	}
	if err := checkGatewayVerb(verb); err != nil {
		return nil, err
	}
	return c.run(ctx, agent, "gateway", verb)
}

func (c *execController) RunDispatch(ctx context.Context, agent string) (string, error) {
	return c.pass(ctx, agent, "run-dispatch")
}

func (c *execController) RunMaintenance(ctx context.Context, agent string) (string, error) {
	return c.pass(ctx, agent, "run-maintenance")
}

// pass reads back the transient unit the wrapper prints before it execs
// systemd-run, and refuses a name RunStop could not later act on: a pass whose
// unit the guard cannot address is a lost run, not a started one.
func (c *execController) pass(ctx context.Context, agent, verb string) (string, error) {
	if err := checkAgent(agent); err != nil {
		return "", err
	}
	out, err := c.run(ctx, agent, verb)
	if err != nil {
		return "", err
	}
	unit := firstLine(out)
	if err := checkRunUnit(agent, unit); err != nil {
		return "", fmt.Errorf("%s %s printed no usable unit: %w", verb, agent, err)
	}
	return unit, nil
}

func (c *execController) RunStop(ctx context.Context, agent, unit string) error {
	if err := checkAgent(agent); err != nil {
		return err
	}
	if err := checkRunUnit(agent, unit); err != nil {
		return err
	}
	_, err := c.run(ctx, agent, "run-stop", unit)
	return err
}

func (c *execController) RunList(ctx context.Context, agent string) ([]string, error) {
	if err := checkAgent(agent); err != nil {
		return nil, err
	}
	out, err := c.run(ctx, agent, "run-list")
	if err != nil {
		return nil, err
	}
	return listedUnits(agent, out), nil
}

func (c *execController) Kanban(ctx context.Context, agent string, args ...string) ([]byte, error) {
	if err := checkAgent(agent); err != nil {
		return nil, err
	}
	if err := checkKanbanArgs(args); err != nil {
		return nil, err
	}
	return c.run(ctx, agent, append([]string{"kanban"}, args...)...)
}

func (c *execController) run(ctx context.Context, agent string, args ...string) ([]byte, error) {
	argv := append([]string{"-n", "-u", "farm-" + agent, "--", c.wrapper}, args...)
	cmd := exec.CommandContext(ctx, c.sudo, argv...) // #nosec G204 -- every element is a checked verb, a checked unit name or the installed wrapper path
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), fmt.Errorf("%s %s: %w: %s", c.sudo, strings.Join(argv, " "), err, bytes.TrimSpace(stderr.Bytes()))
	}
	return stdout.Bytes(), nil
}

func firstLine(out []byte) string {
	line, _, _ := strings.Cut(string(out), "\n")
	return strings.TrimSpace(line)
}

// listedUnits keeps only this agent's run units, which drops both the status
// columns systemctl prints beside each unit and any marker it puts before one.
func listedUnits(agent string, out []byte) []string {
	prefix := runUnitPrefix(agent)
	var units []string
	for line := range strings.Lines(string(out)) {
		for _, field := range strings.Fields(line) {
			if strings.HasPrefix(field, prefix) && strings.HasSuffix(field, unitSuffix) {
				units = append(units, field)
				break
			}
		}
	}
	return units
}
