package render

import (
	"bytes"
	"path"
	"slices"

	"github.com/SnowballSH/snowfarm/internal/roster"
	yaml "go.yaml.in/yaml/v3"
)

const (
	providerKey        = "modelgate"
	modelProvider      = "custom:" + providerKey
	modelgateKeyEnv    = "FARM_MODELGATE_KEY"
	modelgateTransport = "chat_completions"

	auxiliaryModel  = "gpt-5.6-luna"
	auxiliaryEffort = "low"

	terminalBackend = "local"
	approvalsMode   = "smart"
	toolProgress    = "log"
	secretsHelper   = "/usr/local/bin/snowfarm secret-env"

	// max_tokens caps reasoning and answer together on both upstreams, so the
	// xhigh workers get twice the manager budget.
	managerMaxTokens = 16384
	workerMaxTokens  = 32768

	sessionResetMode   = "daily"
	sessionResetHour   = 3
	maxInProgress      = 1
	kanbanFailureLimit = 2
)

var (
	auxiliarySlots = []string{
		"approval",
		"background_review",
		"compression",
		"curator",
		"kanban_decomposer",
		"title_generation",
		"triage_specifier",
		"vision",
	}
	managerDisabledTools = []string{"skill_manage"}
	workerDisabledTools  = []string{"skill_manage", "kanban_create", "kanban_link"}
	managerServerActions = []string{"fetch_messages"}
)

type hermesConfig struct {
	Model        modelConfig               `yaml:"model"`
	Providers    map[string]providerConfig `yaml:"providers"`
	Agent        agentConfig               `yaml:"agent"`
	Auxiliary    map[string]auxConfig      `yaml:"auxiliary"`
	Toolsets     []string                  `yaml:"toolsets"`
	Terminal     terminalConfig            `yaml:"terminal"`
	Discord      *discordConfig            `yaml:"discord,omitempty"`
	Display      displayConfig             `yaml:"display"`
	Kanban       kanbanConfig              `yaml:"kanban"`
	Security     securityConfig            `yaml:"security"`
	Secrets      secretsConfig             `yaml:"secrets"`
	SessionReset *sessionResetConfig       `yaml:"session_reset,omitempty"`
	Approvals    approvalsConfig           `yaml:"approvals"`
	MCPServers   map[string]mcpServer      `yaml:"mcp_servers,omitempty"`
}

type modelConfig struct {
	Provider      string `yaml:"provider"`
	Default       string `yaml:"default"`
	MaxTokens     int    `yaml:"max_tokens"`
	ContextLength int    `yaml:"context_length"`
}

type providerConfig struct {
	API       string `yaml:"api"`
	KeyEnv    string `yaml:"key_env"`
	Transport string `yaml:"transport"`
}

type agentConfig struct {
	ReasoningEffort  string   `yaml:"reasoning_effort"`
	DisabledToolsets []string `yaml:"disabled_toolsets,omitempty"`
	DisabledTools    []string `yaml:"disabled_tools"`
}

type auxConfig struct {
	Provider        string `yaml:"provider"`
	Model           string `yaml:"model"`
	ReasoningEffort string `yaml:"reasoning_effort"`
}

type terminalConfig struct {
	Backend string `yaml:"backend"`
	CWD     string `yaml:"cwd"`
}

type discordConfig struct {
	ServerActions []string `yaml:"server_actions"`
}

type displayConfig struct {
	ToolProgress string                    `yaml:"tool_progress"`
	Platforms    map[string]platformConfig `yaml:"platforms,omitempty"`
}

type platformConfig struct {
	InterimAssistantMessages bool `yaml:"interim_assistant_messages"`
	CleanupProgress          bool `yaml:"cleanup_progress"`
}

type kanbanConfig struct {
	DispatchInGateway       bool `yaml:"dispatch_in_gateway"`
	ReviewDispatch          bool `yaml:"review_dispatch"`
	AutoDecompose           bool `yaml:"auto_decompose"`
	MaxInProgressPerProfile int  `yaml:"max_in_progress_per_profile"`
	FailureLimit            int  `yaml:"failure_limit"`
}

type securityConfig struct {
	AllowLazyInstalls bool `yaml:"allow_lazy_installs"`
	AllowPrivateURLs  bool `yaml:"allow_private_urls"`
}

type secretsConfig struct {
	Command secretsCommand `yaml:"command"`
}

type secretsCommand struct {
	Enabled bool   `yaml:"enabled"`
	Command string `yaml:"command"`
}

type sessionResetConfig struct {
	Mode   string `yaml:"mode"`
	AtHour int    `yaml:"at_hour"`
}

type approvalsConfig struct {
	Mode string `yaml:"mode"`
}

type mcpServer struct {
	Command string            `yaml:"command"`
	Args    []string          `yaml:"args,omitempty"`
	Env     map[string]string `yaml:"env,omitempty"`
}

// Hermes renders one agent's whole profile directory: the Hermes
// configuration, the persona, and every skill the roster gives it.
func Hermes(r *roster.Roster, a roster.Agent) ([]File, error) {
	home := a.HermesHome(r.Farm)
	config, err := marshalYAML(hermesConfigFor(r, a))
	if err != nil {
		return nil, err
	}
	soul, err := Soul(r, a)
	if err != nil {
		return nil, err
	}
	files := []File{
		{Path: path.Join(home, "config.yaml"), Mode: rootOwnedMode, Owner: rootOwner, Group: a.User(), Content: config},
		{Path: path.Join(home, "SOUL.md"), Mode: rootOwnedMode, Owner: rootOwner, Group: a.User(), Content: soul},
	}
	skills, err := Skills(a, home)
	if err != nil {
		return nil, err
	}
	return append(files, skills...), nil
}

func hermesConfigFor(r *roster.Roster, a roster.Agent) hermesConfig {
	config := hermesConfig{
		Model: modelConfig{
			Provider:      modelProvider,
			Default:       a.Model,
			MaxTokens:     maxTokens(a),
			ContextLength: a.ContextLength,
		},
		Providers: map[string]providerConfig{providerKey: {
			API:       r.Farm.Modelgate.API,
			KeyEnv:    modelgateKeyEnv,
			Transport: modelgateTransport,
		}},
		Agent: agentConfig{
			ReasoningEffort:  a.Reasoning,
			DisabledToolsets: a.DisabledToolsets,
			DisabledTools:    disabledTools(a),
		},
		Auxiliary: auxiliary(),
		Toolsets:  a.Toolsets,
		Terminal:  terminalConfig{Backend: terminalBackend, CWD: a.Home(r.Farm)},
		Display:   displayConfig{ToolProgress: toolProgress},
		Kanban: kanbanConfig{
			DispatchInGateway:       false,
			ReviewDispatch:          false,
			AutoDecompose:           false,
			MaxInProgressPerProfile: maxInProgress,
			FailureLimit:            kanbanFailureLimit,
		},
		Security:   securityConfig{AllowLazyInstalls: false, AllowPrivateURLs: a.AllowPrivateURLs},
		Secrets:    secretsConfig{Command: secretsCommand{Enabled: true, Command: secretsHelper}},
		Approvals:  approvalsConfig{Mode: approvalsMode},
		MCPServers: mcpServers(a),
	}
	if a.Tier == roster.TierManager {
		config.Discord = &discordConfig{ServerActions: managerServerActions}
		config.Display.Platforms = map[string]platformConfig{"discord": {
			InterimAssistantMessages: false,
			CleanupProgress:          true,
		}}
		config.SessionReset = &sessionResetConfig{Mode: sessionResetMode, AtHour: sessionResetHour}
	}
	return config
}

func maxTokens(a roster.Agent) int {
	if a.Tier == roster.TierWorker {
		return workerMaxTokens
	}
	return managerMaxTokens
}

func disabledTools(a roster.Agent) []string {
	base := managerDisabledTools
	if a.Tier == roster.TierWorker {
		base = workerDisabledTools
	}
	out := slices.Clone(base)
	for _, tool := range a.DisabledTools {
		if !slices.Contains(out, tool) {
			out = append(out, tool)
		}
	}
	return out
}

func auxiliary() map[string]auxConfig {
	slots := make(map[string]auxConfig, len(auxiliarySlots))
	for _, slot := range auxiliarySlots {
		slots[slot] = auxConfig{Provider: providerKey, Model: auxiliaryModel, ReasoningEffort: auxiliaryEffort}
	}
	return slots
}

func mcpServers(a roster.Agent) map[string]mcpServer {
	if len(a.MCPServers) == 0 {
		return nil
	}
	servers := make(map[string]mcpServer, len(a.MCPServers))
	for _, s := range a.MCPServers {
		servers[s.Name] = mcpServer{Command: s.Command, Args: s.Args, Env: s.Env}
	}
	return servers
}

func marshalYAML(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := yaml.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	if err := enc.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
