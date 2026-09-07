package render

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	// CronJobName is the name apply looks for in the manager's cron store: a
	// job already under it is the record that this command has been run.
	CronJobName = "farm-report"

	cronDeliver = "discord:" + managerChannel
	cronEffort  = "low"
	cronPrompt  = "Read the board with kanban_list for every team you own. " +
		"Report, in under 300 words: cards opened, completed, blocked and stale " +
		"since your last report, and what you intend next. If nothing changed, " +
		"reply exactly [SILENT]."
)

var reportHours = []int{8, 12, 16, 20}

// CronExpression is the manager report schedule in UTC, which is what Hermes
// stores. The farm's location decides it, so the expression a January apply
// renders differs from a July one and apply recreates the job on the shift.
func CronExpression(r *roster.Roster, now time.Time) (string, error) {
	location, err := time.LoadLocation(r.Farm.Location)
	if err != nil {
		return "", fmt.Errorf("farm location %q: %w", r.Farm.Location, err)
	}
	hours := make([]string, 0, len(reportHours))
	for _, hour := range reportHours {
		local := time.Date(now.Year(), now.Month(), now.Day(), hour, 0, 0, 0, location)
		hours = append(hours, strconv.Itoa(local.UTC().Hour()))
	}
	return "0 " + strings.Join(hours, ",") + " * * *", nil
}

// CronCommand is the argv apply runs, as the manager's own user, to create
// its report job. The job's stored record carries scheduler state whose shape
// is not documented at the pin, so hermes writes it rather than snowfarm.
func CronCommand(r *roster.Roster, a roster.Agent, now time.Time) ([]string, error) {
	if a.Tier != roster.TierManager {
		return nil, fmt.Errorf("agent %q is a %s; only a manager reports on a schedule", a.Name, a.Tier)
	}
	expression, err := CronExpression(r, now)
	if err != nil {
		return nil, err
	}
	return []string{
		"runuser", "-u", a.User(), "--", "env",
		"HOME=" + a.Home(r.Farm),
		"HERMES_HOME=" + a.HermesHome(r.Farm),
		r.Farm.HermesBin, "cron", "create", expression, cronPrompt,
		"--name", CronJobName,
		"--deliver", cronDeliver,
		"--model", a.Model,
		"--provider", modelProvider,
		"--reasoning-effort", cronEffort,
	}, nil
}
