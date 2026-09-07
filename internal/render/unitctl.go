package render

import "github.com/SnowballSH/snowfarm/internal/roster"

type unitctlData struct {
	HomeRoot   string
	KanbanHome string
	HermesBin  string
}

// unitctlScript renders farm-unitctl, the only surface the guard reaches an
// agent's user manager through. Its paths come from the roster so a gateway
// and a dispatched run can never address different trees.
func unitctlScript(r *roster.Roster) ([]byte, error) {
	return execute("farm-unitctl.sh.tmpl", unitctlData{
		HomeRoot:   r.Farm.HomeRoot,
		KanbanHome: r.Farm.KanbanHome,
		HermesBin:  r.Farm.HermesBin,
	})
}
