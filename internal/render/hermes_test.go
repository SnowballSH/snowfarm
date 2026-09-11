package render

import (
	"bytes"
	"flag"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SnowballSH/snowfarm/internal/roster"
	yaml "go.yaml.in/yaml/v3"
)

var update = flag.Bool("update", false, "rewrite golden files")

const (
	fixture        = "../roster/testdata/farm.yaml"
	soulFixtureDir = "testdata/soul"
	personaAgent   = "hestia"
)

func loadRoster(t *testing.T) *roster.Roster {
	t.Helper()
	r, err := roster.Load(fixture)
	if err != nil {
		t.Fatalf("load %s: %v", fixture, err)
	}
	dir, err := filepath.Abs(soulFixtureDir)
	if err != nil {
		t.Fatal(err)
	}
	r.Farm.SoulDir = dir
	return r
}

func fileNamed(files []File, rel string) *File {
	for i, f := range files {
		if strings.HasSuffix(f.Path, "/"+rel) {
			return &files[i]
		}
	}
	return nil
}

func TestHermesGolden(t *testing.T) {
	r := loadRoster(t)
	for _, a := range r.Agents {
		t.Run(a.Name, func(t *testing.T) {
			files, err := Hermes(r, a)
			if err != nil {
				t.Fatal(err)
			}
			rendered := map[string]bool{}
			for _, f := range files {
				rendered[strings.TrimPrefix(f.Path, a.HermesHome(r.Farm)+"/")] = true
			}
			for _, f := range files {
				rel := strings.TrimPrefix(f.Path, a.HermesHome(r.Farm)+"/")
				golden := filepath.Join("testdata", "golden", a.Name, rel)
				if *update {
					if err := os.MkdirAll(filepath.Dir(golden), 0o755); err != nil {
						t.Fatal(err)
					}
					if err := os.WriteFile(golden, f.Content, 0o644); err != nil {
						t.Fatal(err)
					}
				}
				want, err := os.ReadFile(golden)
				if err != nil {
					t.Fatalf("missing golden %s", golden)
				}
				if !bytes.Equal(want, f.Content) {
					t.Fatalf("%s differs from golden", rel)
				}
				if f.Owner != rootOwner || f.Group != a.User() {
					t.Fatalf("%s owner %s:%s, want %s:%s: an agent that owns a file apply renders can rewrite it", rel, f.Owner, f.Group, rootOwner, a.User())
				}
				if f.Mode.Perm() != rootOwnedMode {
					t.Fatalf("%s mode %v, want %v: the agent reads what apply rendered and writes none of it", rel, f.Mode.Perm(), rootOwnedMode)
				}
			}
			for _, stale := range staleGoldens(t, filepath.Join("testdata", "golden", a.Name), rendered) {
				t.Fatalf("golden %s is no longer rendered", stale)
			}
		})
	}
}

func TestHermesInvariants(t *testing.T) {
	r := loadRoster(t)
	for _, a := range r.Agents {
		files, err := Hermes(r, a)
		if err != nil {
			t.Fatalf("%s: %v", a.Name, err)
		}
		cfg := fileNamed(files, "config.yaml")
		if cfg == nil {
			t.Fatalf("%s: no config.yaml", a.Name)
		}
		var doc map[string]any
		if err := yaml.Unmarshal(cfg.Content, &doc); err != nil {
			t.Fatalf("%s: %v", a.Name, err)
		}
		kanban := doc["kanban"].(map[string]any)
		if kanban["dispatch_in_gateway"] != false {
			t.Fatalf("%s: dispatch must be disabled", a.Name)
		}
		if _, present := doc["model"].(map[string]any)["max_tokens"]; present {
			t.Fatalf("%s: model.max_tokens has no reader at the pinned Hermes; the provider's default applies whatever is rendered", a.Name)
		}
		if _, present := doc["session_reset"]; present {
			t.Fatalf("%s: session_reset is legacy at the pinned Hermes and ignored; the restart window is the only session boundary", a.Name)
		}
		if root, _ := doc["toolsets"].([]any); !slices.Equal(root, []any{"kanban"}) {
			t.Fatalf("%s: the root toolsets list is the gate for the orchestrator kanban tools and nothing else; got %v", a.Name, root)
		}
		platforms, _ := doc["platform_toolsets"].(map[string]any)
		wantPlatforms := []string{"cli"}
		if a.Tier == roster.TierManager {
			wantPlatforms = []string{"cron", "discord"}
		}
		if got := slices.Sorted(maps.Keys(platforms)); !slices.Equal(got, wantPlatforms) {
			t.Fatalf("%s: platform_toolsets names %v, but a %s runs on %v", a.Name, got, a.Tier, wantPlatforms)
		}
		for platform, raw := range platforms {
			toolsets := raw.([]any)
			for _, needed := range []any{"kanban", "terminal", "skills"} {
				if !slices.Contains(toolsets, needed) {
					t.Fatalf("%s: platform %s lacks %s, so Claude Code is unreachable or no card can terminate", a.Name, platform, needed)
				}
			}
			if platform != "discord" && slices.Contains(toolsets, any("discord")) {
				t.Fatalf("%s: the discord toolset is bound to the discord platform; Hermes drops it from %s", a.Name, platform)
			}
		}
		if fileNamed(files, "skills/farm-claude-code/SKILL.md") == nil {
			t.Fatalf("%s: the farm-claude-code skill was not rendered", a.Name)
		}
		agentCfg := doc["agent"].(map[string]any)
		if _, present := agentCfg["disabled_tools"]; present {
			t.Fatalf("%s: agent.disabled_tools has no reader at the pinned Hermes; rendering it promises a restriction that does not exist", a.Name)
		}
		term := doc["terminal"].(map[string]any)
		if term["backend"] != "local" {
			t.Fatalf("%s: terminal backend must be local", a.Name)
		}
		for k := range term {
			if strings.HasPrefix(k, "docker_") {
				t.Fatalf("%s: no container key may be rendered, found %s", a.Name, k)
			}
		}
		if strings.Contains(string(cfg.Content), "docker_") {
			t.Fatalf("%s: the container runtime is gone; no docker_ key may be rendered anywhere", a.Name)
		}
		if strings.Contains(string(cfg.Content), "DISCORD_BOT_TOKEN") || strings.Contains(string(cfg.Content), "sk-") {
			t.Fatalf("%s: a secret-looking value in config", a.Name)
		}
		if doc["secrets"].(map[string]any)["command"].(map[string]any)["command"] != "/usr/local/bin/snowfarm secret-env" {
			t.Fatalf("%s: secrets helper", a.Name)
		}
	}
}

func staleGoldens(t *testing.T, root string, rendered map[string]bool) []string {
	t.Helper()
	var stale []string
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, relErr := filepath.Rel(root, p)
		if relErr != nil {
			return relErr
		}
		if !rendered[filepath.ToSlash(rel)] {
			stale = append(stale, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return stale
}

func TestSkillsRejectsUnshippedSkill(t *testing.T) {
	r := loadRoster(t)
	a, ok := r.Agent("euclid")
	if !ok {
		t.Fatal("euclid is missing from the fixture")
	}
	a.Skills = append(slices.Clone(a.Skills), "sdlc-review")
	if _, err := Hermes(r, a); err == nil {
		t.Fatal("a skill the supervisor does not ship must not render as an empty file")
	}
}

func TestSoulNamesOnlyEnabledWorkers(t *testing.T) {
	r := loadRoster(t)
	const off = "argus"
	r.Agents = slices.Clone(r.Agents)
	for i := range r.Agents {
		if r.Agents[i].Name == off {
			r.Agents[i].Enabled = false
		}
	}
	atlas, ok := r.Agent("atlas")
	if !ok {
		t.Fatal("atlas is missing from the fixture")
	}
	soul, err := Soul(r, atlas)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(soul), off) {
		t.Fatalf("the manager SOUL offers %s as an assignee, but apply provisions no unit for a disabled agent", off)
	}
	for _, want := range []string{"hestia", "euclid", "hypatia", "daedalus"} {
		if !strings.Contains(string(soul), want) {
			t.Fatalf("the manager SOUL dropped the enabled worker %s", want)
		}
	}
}
