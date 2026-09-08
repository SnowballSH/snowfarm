package discord

const (
	RoleSupervisor = "Supervisor"
	RoleManager    = "Manager"
)

const (
	CategoryOperator = "OPERATOR"
	CategoryManagers = "MANAGERS"
)

// The fixed channels, keyed as the rendered units and channels.json key
// them: with the "#" a Discord channel name does not itself carry.
const (
	ChannelCommand     = "#command"
	ChannelFarmControl = "#farm-control"
	ChannelFarmStatus  = "#farm-status"
	ChannelManagers    = "#managers"
)

// State is what a reconciled guild is, by name: the role and channel ids
// every later step — unit rendering, the poster, the logger — addresses
// Discord with.
type State struct {
	Roles    map[string]string `json:"roles"`
	Channels map[string]string `json:"channels"`
}
