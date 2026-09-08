package render

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

func agentNamed(t *testing.T, r *roster.Roster, name string) roster.Agent {
	t.Helper()
	a, ok := r.Agent(name)
	if !ok {
		t.Fatalf("%s is missing from the fixture", name)
	}
	return a
}

func withSoulDir(r *roster.Roster, dir string) *roster.Roster {
	clone := *r
	clone.Farm.SoulDir = dir
	return &clone
}

func TestSoulAppendsThePersonaFile(t *testing.T) {
	r := loadRoster(t)
	agent := agentNamed(t, r, personaAgent)

	tier, err := Soul(withSoulDir(r, t.TempDir()), agent)
	if err != nil {
		t.Fatal(err)
	}
	full, err := Soul(r, agent)
	if err != nil {
		t.Fatal(err)
	}
	persona, err := os.ReadFile(filepath.Join(soulFixtureDir, personaAgent+".md"))
	if err != nil {
		t.Fatal(err)
	}

	want := slices.Concat(bytes.TrimRight(tier, "\n"), []byte("\n\n"), persona)
	if !bytes.Equal(full, want) {
		t.Fatalf("SOUL.md must be the tier template, one blank line, then the persona verbatim; got:\n%s", full)
	}
}

func TestSoulWithoutAPersonaFileIsTheTierTemplateAlone(t *testing.T) {
	r := loadRoster(t)
	agent := agentNamed(t, r, "euclid")

	full, err := Soul(r, agent)
	if err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{t.TempDir(), filepath.Join(t.TempDir(), "never-staged")} {
		tier, err := Soul(withSoulDir(r, dir), agent)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(full, tier) {
			t.Fatalf("soul dir %s holds no file for the agent, so the render is the tier template alone; got:\n%s", dir, tier)
		}
	}
}

func TestSoulRejectsANonRegularPersona(t *testing.T) {
	r := loadRoster(t)
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, personaAgent+".md"), 0o750); err != nil {
		t.Fatal(err)
	}
	_, err := Soul(withSoulDir(r, dir), agentNamed(t, r, personaAgent))
	if err == nil || !strings.Contains(err.Error(), personaAgent+".md") {
		t.Fatalf("a persona that is not a regular file must fail the render, got %v", err)
	}
}

func TestSoulRejectsAnUnreadablePersona(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root reads a 0000 file")
	}
	r := loadRoster(t)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, personaAgent+".md"), []byte("secret\n"), 0o000); err != nil {
		t.Fatal(err)
	}
	_, err := Soul(withSoulDir(r, dir), agentNamed(t, r, personaAgent))
	if err == nil || !strings.Contains(err.Error(), personaAgent+".md") {
		t.Fatalf("a persona the renderer cannot read must fail the render, got %v", err)
	}
}

func TestSoulWithoutASoulDirReadsNothingFromTheWorkingDirectory(t *testing.T) {
	r := loadRoster(t)
	agent := agentNamed(t, r, personaAgent)
	tier, err := Soul(withSoulDir(r, t.TempDir()), agent)
	if err != nil {
		t.Fatal(err)
	}

	t.Chdir(t.TempDir())
	if err := os.WriteFile(personaAgent+".md", []byte("## Not a persona\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := Soul(withSoulDir(r, ""), agent)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, tier) {
		t.Fatalf("an unset soul dir must read nothing; got:\n%s", got)
	}
}
