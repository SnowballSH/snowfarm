package render

import (
	"fmt"
	"slices"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

type soulData struct {
	Agent   roster.Agent
	Roster  *roster.Roster
	Teams   []roster.Team
	Workers []roster.Agent
}

// Soul renders the agent's persona: the protocol it is held to, expressed as
// the file Hermes reads at the start of every turn.
func Soul(r *roster.Roster, a roster.Agent) ([]byte, error) {
	name := "soul-worker.tmpl"
	if a.Tier == roster.TierManager {
		name = "soul-manager.tmpl"
	}
	body, err := execute(name, soulData{Agent: a, Roster: r, Teams: teamsOf(r, a), Workers: teammateWorkers(r, a)})
	if err != nil {
		return nil, fmt.Errorf("agent %q: %w", a.Name, err)
	}
	return body, nil
}

func teamsOf(r *roster.Roster, a roster.Agent) []roster.Team {
	var out []roster.Team
	for _, t := range r.Teams {
		if slices.Contains(a.Teams, t.Name) {
			out = append(out, t)
		}
	}
	return out
}

// teammateWorkers is the manager's assignable set, which is the enabled
// workers only: apply provisions no unit for a disabled agent, so a card
// assigned to one would never run and never report.
func teammateWorkers(r *roster.Roster, a roster.Agent) []roster.Agent {
	var out []roster.Agent
	for _, w := range r.EnabledWorkers() {
		if slices.ContainsFunc(w.Teams, func(team string) bool { return slices.Contains(a.Teams, team) }) {
			out = append(out, w)
		}
	}
	return out
}
