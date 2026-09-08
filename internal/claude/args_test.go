package claude

import (
	"slices"
	"strings"
	"testing"
)

const wantSettings = `{"disableAllHooks":true,"permissions":{"deny":["Read(//etc/snowfarm/**)","Edit(//etc/snowfarm/**)","Read(//var/lib/snowfarm/**)","Edit(//var/lib/snowfarm/**)","Edit(//var/lib/farm/*/.hermes/profiles/*/config.yaml)","Edit(//var/lib/farm/*/.hermes/profiles/*/SOUL.md)","Bash(sudo *)","Bash(systemctl *)"]}}`

func TestDenyRulesArePresent(t *testing.T) {
	want := []string{
		"Read(//etc/snowfarm/**)",
		"Edit(//etc/snowfarm/**)",
		"Read(//var/lib/snowfarm/**)",
		"Edit(//var/lib/snowfarm/**)",
		"Edit(//var/lib/farm/*/.hermes/profiles/*/config.yaml)",
		"Edit(//var/lib/farm/*/.hermes/profiles/*/SOUL.md)",
		"Bash(sudo *)",
		"Bash(systemctl *)",
	}
	got := DenyRules()
	if !slices.Equal(got, want) {
		t.Fatalf("deny rules\n got %q\nwant %q", got, want)
	}
	for _, rule := range got {
		open := strings.Index(rule, "(")
		if open < 0 || !strings.HasSuffix(rule, ")") {
			t.Fatalf("%q is not a Tool(pattern) rule", rule)
		}
		pattern := rule[open+1 : len(rule)-1]
		if strings.HasPrefix(pattern, "/") && !strings.HasPrefix(pattern, "//") {
			t.Errorf("%q anchors at the settings source, so it matches <cwd>%s and denies nothing", rule, pattern)
		}
	}

	got[0] = "Read(//tmp/**)"
	if DenyRules()[0] != want[0] {
		t.Fatalf("a caller rewrote the deny rules: %q", DenyRules()[0])
	}
}

func TestArgsRejectContainmentOverrides(t *testing.T) {
	for _, flag := range []string{
		"--bare",
		"--dangerously-skip-permissions",
		"--permission-mode",
		"--settings",
		"--setting-sources",
		"--mcp-config",
		"--output-format",
		"--resume",
		"--continue",
	} {
		bare := strings.TrimPrefix(flag, "--")
		for _, form := range []string{flag, flag + "=x", "-" + bare, "-" + bare + "=x"} {
			args := []string{"-p", "print the git HEAD", form, "acceptPlease"}
			if _, err := ParseArgs(args); err == nil {
				t.Errorf("ParseArgs accepted %q", form)
			}
		}
	}
}

func TestArgsBuild(t *testing.T) {
	opts, err := ParseArgs([]string{
		"-p", "print the git HEAD",
		"--add-dir", "/srv/snowfarm/kanban/kanban/workspaces/card-17",
	})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := Argv("claude", opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"claude",
		"-p", "print the git HEAD",
		"--model", "claude-opus-5",
		"--setting-sources", "user",
		"--settings", wantSettings,
		"--strict-mcp-config",
		"--dangerously-skip-permissions",
		"--max-turns", "120",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
		"--add-dir", "/srv/snowfarm/kanban/kanban/workspaces/card-17",
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
	if slices.Contains(argv, "--effort") {
		t.Error("--effort reached the argv; effort travels only through CLAUDE_CODE_EFFORT_LEVEL")
	}
}

func TestArgsBuildCarriesCallerChoices(t *testing.T) {
	opts, err := ParseArgs([]string{
		"-p", "review the diff",
		"--model", "claude-fable-5-1",
		"--effort", "max",
		"--max-turns", "40",
		"--add-dir", "/a",
		"--add-dir", "/b",
		"--append-system-prompt", "answer in one paragraph",
	})
	if err != nil {
		t.Fatal(err)
	}
	argv, err := Argv("/usr/local/bin/claude", opts)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"/usr/local/bin/claude",
		"-p", "review the diff",
		"--model", "claude-fable-5-1",
		"--setting-sources", "user",
		"--settings", wantSettings,
		"--strict-mcp-config",
		"--dangerously-skip-permissions",
		"--max-turns", "40",
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
		"--add-dir", "/a",
		"--add-dir", "/b",
		"--append-system-prompt", "answer in one paragraph",
	}
	if !slices.Equal(argv, want) {
		t.Fatalf("argv\n got %q\nwant %q", argv, want)
	}
}

func TestArgsReject(t *testing.T) {
	for name, args := range map[string][]string{
		"no prompt":       {"--model", "claude-opus-5"},
		"empty prompt":    {"-p", "  "},
		"unknown model":   {"-p", "x", "--model", "gpt-5.6-luna"},
		"unknown effort":  {"-p", "x", "--effort", "ludicrous"},
		"zero turns":      {"-p", "x", "--max-turns", "0"},
		"trailing":        {"-p", "x", "somewhere"},
		"relative addDir": {"-p", "x", "--add-dir", "workspaces/card-17"},
	} {
		if _, err := ParseArgs(args); err == nil {
			t.Errorf("%s: ParseArgs accepted %q", name, args)
		}
	}
}

func TestEnvStripsCallerCredentials(t *testing.T) {
	opts, err := ParseArgs([]string{"-p", "x"})
	if err != nil {
		t.Fatal(err)
	}
	base := []string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=an-api-key",
		"ANTHROPIC_AUTH_TOKEN=an-auth-token",
		"CLAUDE_CODE_OAUTH_TOKEN=the-subscription-token",
		"CLAUDE_CODE_EFFORT_LEVEL=low",
	}
	env := Env(base, "/var/lib/farm/hestia/.claude", opts)

	for key, want := range map[string]string{
		"CLAUDE_CONFIG_DIR":                        "/var/lib/farm/hestia/.claude",
		"DISABLE_AUTOUPDATER":                      "1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB":         "1",
		"CLAUDE_CODE_OAUTH_TOKEN":                  "the-subscription-token",
		"PATH":                                     "/usr/bin",
	} {
		if got, ok := envLookup(env, key); !ok || got != want {
			t.Errorf("%s = %q (present %v), want %q", key, got, ok, want)
		}
		if n := envCount(env, key); n != 1 {
			t.Errorf("%s appears %d times, want once", key, n)
		}
	}
	for _, key := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN", "CLAUDE_CODE_EFFORT_LEVEL"} {
		if got, ok := envLookup(env, key); ok {
			t.Errorf("%s reached the child as %q", key, got)
		}
	}
}

func TestEffortResolution(t *testing.T) {
	inherited := []string{"CLAUDE_CODE_EFFORT_LEVEL=low"}

	raised, err := ParseArgs([]string{"-p", "x", "--effort", "xhigh"})
	if err != nil {
		t.Fatal(err)
	}
	env := Env(inherited, "/home/.claude", raised)
	if got, ok := envLookup(env, "CLAUDE_CODE_EFFORT_LEVEL"); !ok || got != "xhigh" {
		t.Errorf("CLAUDE_CODE_EFFORT_LEVEL = %q (present %v), want xhigh", got, ok)
	}
	if n := envCount(env, "CLAUDE_CODE_EFFORT_LEVEL"); n != 1 {
		t.Errorf("CLAUDE_CODE_EFFORT_LEVEL appears %d times, want once", n)
	}

	silent, err := ParseArgs([]string{"-p", "x"})
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := envLookup(Env(inherited, "/home/.claude", silent), "CLAUDE_CODE_EFFORT_LEVEL"); ok {
		t.Errorf("with no --effort the child inherited CLAUDE_CODE_EFFORT_LEVEL=%q", got)
	}
}

func envCount(env []string, key string) int {
	n := 0
	for _, entry := range env {
		if name, _, ok := strings.Cut(entry, "="); ok && name == key {
			n++
		}
	}
	return n
}
