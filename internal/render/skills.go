package render

import (
	"embed"
	"fmt"
	"path"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

//go:embed skills/*/SKILL.md
var skillFS embed.FS

// Skills renders the skill bodies the roster gives the agent into its
// profile, where Hermes reaches them through skill_view.
func Skills(a roster.Agent, home string) ([]File, error) {
	files := make([]File, 0, len(a.Skills))
	for _, name := range a.Skills {
		body, err := skillFS.ReadFile(path.Join("skills", name, "SKILL.md"))
		if err != nil {
			return nil, fmt.Errorf("agent %q: skill %q is not one the supervisor ships", a.Name, name)
		}
		files = append(files, File{
			Path:    path.Join(home, "skills", name, "SKILL.md"),
			Mode:    rootOwnedMode,
			Owner:   rootOwner,
			Group:   a.User(),
			Content: body,
		})
	}
	return files, nil
}
