// Package discord is the farm's Discord boundary: the client the guard
// talks to, and the reconciler that makes one guild match the roster.
package discord

import (
	"context"
	"time"
)

// Guild permission bits, §6.3 of the design. Only the bits the farm grants,
// denies or asserts are named.
const (
	PermAdministrator         uint64 = 1 << 3
	PermManageChannels        uint64 = 1 << 4
	PermAddReactions          uint64 = 1 << 6
	PermViewAuditLog          uint64 = 1 << 7
	PermViewChannel           uint64 = 1 << 10
	PermSendMessages          uint64 = 1 << 11
	PermEmbedLinks            uint64 = 1 << 14
	PermAttachFiles           uint64 = 1 << 15
	PermReadMessageHistory    uint64 = 1 << 16
	PermMentionEveryone       uint64 = 1 << 17
	PermManageRoles           uint64 = 1 << 28
	PermManageThreads         uint64 = 1 << 34
	PermCreatePublicThreads   uint64 = 1 << 35
	PermSendMessagesInThreads uint64 = 1 << 38
)

// ManagerPermissions never carries MENTION_EVERYONE and never
// ADMINISTRATOR, which would bypass every channel overwrite.
const ManagerPermissions = PermSendMessages | PermSendMessagesInThreads |
	PermCreatePublicThreads | PermReadMessageHistory | PermAttachFiles |
	PermEmbedLinks | PermAddReactions

// SupervisorPermissions is every permission the reconciler will ever write,
// since a bot may place in an overwrite only permissions it holds itself.
// The operator sets it on the Supervisor role at F0; the reconciler asserts
// it rather than granting it.
const SupervisorPermissions = ManagerPermissions | PermViewChannel |
	PermManageChannels | PermManageRoles | PermManageThreads |
	PermViewAuditLog | PermMentionEveryone

type ChannelType int

const (
	ChannelText     ChannelType = 0
	ChannelCategory ChannelType = 4
)

type OverwriteType int

const (
	OverwriteRole   OverwriteType = 0
	OverwriteMember OverwriteType = 1
)

type Guild struct {
	ID       string
	Roles    []Role
	Channels []Channel
}

type Role struct {
	ID          string
	Name        string
	Position    int
	Permissions uint64
	Managed     bool

	// BotID is the application a managed role belongs to, empty for an
	// ordinary role.
	BotID string
}

type Channel struct {
	ID         string
	Name       string
	Type       ChannelType
	ParentID   string
	Position   int
	Overwrites []Overwrite
}

type Overwrite struct {
	ID    string
	Type  OverwriteType
	Allow uint64
	Deny  uint64
}

// ChannelSpec is a channel as the roster wants it. Name carries no leading
// "#": that prefix belongs to the State keys, not to Discord.
type ChannelSpec struct {
	Name       string
	Type       ChannelType
	ParentID   string
	Overwrites []Overwrite
}

type Message struct {
	ID        string
	ChannelID string
	AuthorID  string
	AuthorBot bool
	Content   string
	CreatedAt time.Time
}

type SessionStartLimit struct {
	Total          int
	Remaining      int
	ResetAfter     time.Duration
	MaxConcurrency int
}

type EventKind string

const (
	EventMessageCreate EventKind = "message_create"
	EventMessageUpdate EventKind = "message_update"
	EventMessageDelete EventKind = "message_delete"
	EventAuditLogEntry EventKind = "audit_log_entry"
	EventThreadCreate  EventKind = "thread_create"
)

// Event is one gateway fact, flat so the logger can persist it as a line.
type Event struct {
	Kind      EventKind `json:"kind"`
	At        time.Time `json:"at"`
	ChannelID string    `json:"channel_id,omitempty"`
	MessageID string    `json:"message_id,omitempty"`
	AuthorID  string    `json:"author_id,omitempty"`
	Content   string    `json:"content,omitempty"`
	ThreadID  string    `json:"thread_id,omitempty"`
	ParentID  string    `json:"parent_id,omitempty"`
	Name      string    `json:"name,omitempty"`
	Action    string    `json:"action,omitempty"`
	ActorID   string    `json:"actor_id,omitempty"`
	TargetID  string    `json:"target_id,omitempty"`
	Reason    string    `json:"reason,omitempty"`
}

// Client is every Discord operation the farm performs. Every write carries
// the reason Discord records in the audit log.
type Client interface {
	Guild(ctx context.Context) (Guild, error)
	CreateRole(ctx context.Context, name string, perms uint64, reason string) (Role, error)
	EditRole(ctx context.Context, id string, perms uint64, reason string) error

	// PositionRoles asserts an ordering, ids highest first.
	PositionRoles(ctx context.Context, ids []string, reason string) error

	AddMemberRole(ctx context.Context, userID, roleID, reason string) error
	GuildMemberRoles(ctx context.Context, userID string) ([]string, error)
	EditEveryonePermissions(ctx context.Context, perms uint64, reason string) error
	CreateChannel(ctx context.Context, c ChannelSpec, reason string) (Channel, error)
	EditChannel(ctx context.Context, id string, c ChannelSpec, reason string) error
	Send(ctx context.Context, channelID, content string, suppressNotifications bool) (string, error)
	Messages(ctx context.Context, channelID, afterID string, limit int) ([]Message, error)

	// GatewayBot reads the session budget of the bot the given token
	// belongs to, so the breaker checks the restarting manager's own
	// budget. An empty token uses the client's own.
	GatewayBot(ctx context.Context, botToken string) (SessionStartLimit, error)

	Events(ctx context.Context) (<-chan Event, error)
}
