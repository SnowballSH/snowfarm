// Package unitctl reaches an agent's user manager the one way the sudoers
// rule allows: sudo -u farm-<agent> farm-unitctl <verb>. The wrapper enforces
// its own allow-lists; the checks here refuse the same calls before a
// privileged command runs at all, so a defect in the guard shows up as an
// error rather than as an exit 2 nobody reads.
package unitctl

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strings"
)

type Controller interface {
	Gateway(ctx context.Context, agent, verb string) ([]byte, error)
	RunDispatch(ctx context.Context, agent string) (unit string, err error)
	RunMaintenance(ctx context.Context, agent string) (unit string, err error)
	RunStop(ctx context.Context, agent, unit string) error
	RunList(ctx context.Context, agent string) ([]string, error)
	Kanban(ctx context.Context, agent string, args ...string) ([]byte, error)
}

const (
	DefaultSudo    = "sudo"
	DefaultWrapper = "/usr/local/sbin/farm-unitctl"

	unitSuffix = ".service"
)

var (
	agentName = regexp.MustCompile(`^[a-z][a-z0-9-]{1,15}$`)

	gatewayVerbs = []string{"start", "stop", "restart", "is-active", "show"}
	kanbanVerbs  = []string{
		"create", "notify-subscribe", "notify-unsubscribe", "block",
		"reclaim", "diagnostics", "list", "show", "stats",
	}
)

func checkAgent(agent string) error {
	if !agentName.MatchString(agent) {
		return fmt.Errorf("agent name %q must match %s", agent, agentName)
	}
	return nil
}

func checkGatewayVerb(verb string) error {
	if !slices.Contains(gatewayVerbs, verb) {
		return fmt.Errorf("gateway verb %q is not one of %v", verb, gatewayVerbs)
	}
	return nil
}

func checkKanbanArgs(args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("kanban needs a subverb, one of %v", kanbanVerbs)
	}
	if !slices.Contains(kanbanVerbs, args[0]) {
		return fmt.Errorf("kanban subverb %q is not one of %v", args[0], kanbanVerbs)
	}
	return nil
}

func checkRunUnit(agent, unit string) error {
	prefix := runUnitPrefix(agent)
	if !strings.HasPrefix(unit, prefix) || !strings.HasSuffix(unit, unitSuffix) {
		return fmt.Errorf("unit %q is not a %s*%s run of this agent", unit, prefix, unitSuffix)
	}
	return nil
}

func runUnitPrefix(agent string) string { return "snowfarm-run-" + agent + "-" }
