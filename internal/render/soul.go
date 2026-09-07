package render

import (
	"bytes"
	"embed"
	"fmt"
	"slices"
	"strings"
	"text/template"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

//go:embed templates/*.tmpl
var templateFS embed.FS

var soulTemplates = template.Must(template.New("soul").
	Funcs(template.FuncMap{"join": strings.Join}).
	ParseFS(templateFS, "templates/*.tmpl"))

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
	var buf bytes.Buffer
	data := soulData{Agent: a, Roster: r, Teams: teamsOf(r, a), Workers: teammateWorkers(r, a)}
	if err := soulTemplates.ExecuteTemplate(&buf, name, data); err != nil {
		return nil, fmt.Errorf("agent %q: render %s: %w", a.Name, name, err)
	}
	return buf.Bytes(), nil
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

func teammateWorkers(r *roster.Roster, a roster.Agent) []roster.Agent {
	var out []roster.Agent
	for _, w := range r.Workers() {
		if slices.ContainsFunc(w.Teams, func(team string) bool { return slices.Contains(a.Teams, team) }) {
			out = append(out, w)
		}
	}
	return out
}
