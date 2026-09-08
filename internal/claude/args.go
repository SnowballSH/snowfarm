package claude

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

const (
	DefaultModel    = "claude-opus-5"
	DefaultMaxTurns = 120
)

var (
	models  = []string{DefaultModel, "claude-fable-5-1"}
	efforts = []string{"low", "medium", "high", "xhigh", "max"}

	// Every one of these decides how contained the run is, and the wrapper
	// owns all of them: a caller that could set one would run Claude Code
	// outside the farm's boundary.
	reserved = []string{
		"bare",
		"dangerously-skip-permissions",
		"permission-mode",
		"settings",
		"setting-sources",
		"mcp-config",
		"output-format",
		"resume",
		"continue",
	}

	// A rule's pattern is anchored at the settings source unless it begins
	// with a second slash, so a single-slash path denies nothing that exists.
	denyRules = []string{
		"Read(//etc/snowfarm/**)",
		"Edit(//etc/snowfarm/**)",
		"Read(//var/lib/snowfarm/**)",
		"Edit(//var/lib/snowfarm/**)",
		"Edit(//var/lib/farm/*/.hermes/profiles/*/config.yaml)",
		"Edit(//var/lib/farm/*/.hermes/profiles/*/SOUL.md)",
		"Bash(sudo *)",
		"Bash(systemctl *)",
	}
)

// Options is the whole surface a caller controls.
type Options struct {
	Prompt             string
	Model              string
	Effort             string
	MaxTurns           int
	AddDirs            []string
	AppendSystemPrompt string
}

type dirList []string

func (d *dirList) String() string { return strings.Join(*d, ",") }

func (d *dirList) Set(value string) error {
	if !filepath.IsAbs(value) {
		return fmt.Errorf("--add-dir %q is not an absolute path", value)
	}
	*d = append(*d, value)
	return nil
}

func DenyRules() []string { return slices.Clone(denyRules) }

func ParseArgs(args []string) (Options, error) {
	if err := checkReserved(args); err != nil {
		return Options{}, err
	}
	var opts Options
	var dirs dirList
	set := flag.NewFlagSet("farm-claude", flag.ContinueOnError)
	set.SetOutput(io.Discard)
	set.StringVar(&opts.Prompt, "p", "", "the task, whole and self-contained")
	set.StringVar(&opts.Model, "model", DefaultModel, "the Claude model to run")
	set.StringVar(&opts.Effort, "effort", "", "the reasoning effort to run at")
	set.IntVar(&opts.MaxTurns, "max-turns", DefaultMaxTurns, "the turn ceiling for the run")
	set.StringVar(&opts.AppendSystemPrompt, "append-system-prompt", "", "text appended to the system prompt")
	set.Var(&dirs, "add-dir", "an extra directory the run may read (repeatable)")
	if err := set.Parse(args); err != nil {
		return Options{}, err
	}
	opts.AddDirs = dirs
	if set.NArg() > 0 {
		return Options{}, fmt.Errorf("unexpected argument %q", set.Arg(0))
	}
	if strings.TrimSpace(opts.Prompt) == "" {
		return Options{}, fmt.Errorf("-p is required: the prompt is the whole context the run gets")
	}
	if !slices.Contains(models, opts.Model) {
		return Options{}, fmt.Errorf("--model %q is not one of %v", opts.Model, models)
	}
	if opts.Effort != "" && !slices.Contains(efforts, opts.Effort) {
		return Options{}, fmt.Errorf("--effort %q is not one of %v", opts.Effort, efforts)
	}
	if opts.MaxTurns < 1 {
		return Options{}, fmt.Errorf("--max-turns %d is not positive", opts.MaxTurns)
	}
	return opts, nil
}

func checkReserved(args []string) error {
	valueFlags := []string{"p", "model", "effort", "max-turns", "add-dir", "append-system-prompt"}
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "-") || arg == "-" || arg == "--" {
			continue
		}
		name, _, joined := strings.Cut(strings.TrimLeft(arg, "-"), "=")
		if slices.Contains(reserved, name) {
			return fmt.Errorf("--%s is the wrapper's to set, not the caller's", name)
		}
		if !joined && slices.Contains(valueFlags, name) {
			i++
		}
	}
	return nil
}

type settings struct {
	DisableAllHooks bool               `json:"disableAllHooks"`
	Permissions     settingPermissions `json:"permissions"`
}

type settingPermissions struct {
	Deny []string `json:"deny"`
}

// settingsArg is the --settings object: hooks off, and the deny rules that
// bind in every permission mode, bypassPermissions included.
func settingsArg() (string, error) {
	encoded, err := json.Marshal(settings{DisableAllHooks: true, Permissions: settingPermissions{Deny: DenyRules()}})
	return string(encoded), err
}

// Argv is the exec argv, bin included. Effort is absent by design: it reaches
// the run through CLAUDE_CODE_EFFORT_LEVEL, which outranks any flag and is
// the only way to ask for max.
func Argv(bin string, opts Options) ([]string, error) {
	settings, err := settingsArg()
	if err != nil {
		return nil, err
	}
	argv := []string{
		bin,
		"-p", opts.Prompt,
		"--model", opts.Model,
		"--setting-sources", "user",
		"--settings", settings,
		"--strict-mcp-config",
		"--dangerously-skip-permissions",
		"--max-turns", strconv.Itoa(opts.MaxTurns),
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
	}
	for _, dir := range opts.AddDirs {
		argv = append(argv, "--add-dir", dir)
	}
	if opts.AppendSystemPrompt != "" {
		argv = append(argv, "--append-system-prompt", opts.AppendSystemPrompt)
	}
	return argv, nil
}

// Env is the child's environment: the caller's, minus every credential the
// farm does not run on, plus the farm's own settings.
func Env(base []string, configDir string, opts Options) []string {
	env := deleteKeys(base,
		"ANTHROPIC_API_KEY",
		"ANTHROPIC_AUTH_TOKEN",
		"CLAUDE_CODE_EFFORT_LEVEL",
		"CLAUDE_CONFIG_DIR",
		"DISABLE_AUTOUPDATER",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB",
	)
	env = append(env,
		"CLAUDE_CONFIG_DIR="+configDir,
		"DISABLE_AUTOUPDATER=1",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1",
		"CLAUDE_CODE_SUBPROCESS_ENV_SCRUB=1",
	)
	if opts.Effort != "" {
		env = append(env, "CLAUDE_CODE_EFFORT_LEVEL="+opts.Effort)
	}
	return env
}

func envLookup(env []string, key string) (string, bool) {
	for _, entry := range env {
		if name, value, ok := strings.Cut(entry, "="); ok && name == key {
			return value, true
		}
	}
	return "", false
}

func deleteKeys(env []string, keys ...string) []string {
	out := make([]string, 0, len(env)+4)
	for _, entry := range env {
		name, _, _ := strings.Cut(entry, "=")
		if !slices.Contains(keys, name) {
			out = append(out, entry)
		}
	}
	return out
}
