package discord

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/disgoorg/disgo"
	"github.com/disgoorg/disgo/bot"
	dapi "github.com/disgoorg/disgo/discord"
	"github.com/disgoorg/disgo/events"
	"github.com/disgoorg/disgo/gateway"
	"github.com/disgoorg/disgo/rest"
	"github.com/disgoorg/snowflake/v2"
)

// FarmIntents is the whole subscription: the guild model, its moderation
// (audit log) entries, and message content. Members and presences stay off.
const FarmIntents = gateway.IntentGuilds | gateway.IntentGuildModeration |
	gateway.IntentGuildMessages | gateway.IntentMessageContent

const eventBuffer = 256

var _ Client = (*Disgo)(nil)

// Disgo is the Client backed by github.com/disgoorg/disgo against one guild.
type Disgo struct {
	client     *bot.Client
	guildID    snowflake.ID
	subscribed atomic.Bool
}

func NewDisgo(token, guildID string) (*Disgo, error) {
	id, err := snowflake.Parse(guildID)
	if err != nil {
		return nil, fmt.Errorf("guild id %q: %w", guildID, err)
	}
	client, err := disgo.New(token, bot.WithGatewayConfigOpts(gateway.WithIntents(FarmIntents)))
	if err != nil {
		return nil, fmt.Errorf("build discord client: %w", err)
	}
	return &Disgo{client: client, guildID: id}, nil
}

func (d *Disgo) Guild(ctx context.Context) (Guild, error) {
	roles, err := d.client.Rest.GetRoles(d.guildID, rest.WithCtx(ctx))
	if err != nil {
		return Guild{}, fmt.Errorf("get roles: %w", err)
	}
	channels, err := d.client.Rest.GetGuildChannels(d.guildID, rest.WithCtx(ctx))
	if err != nil {
		return Guild{}, fmt.Errorf("get channels: %w", err)
	}
	guild := Guild{ID: d.guildID.String()}
	for _, r := range roles {
		guild.Roles = append(guild.Roles, convertRole(r))
	}
	for _, c := range channels {
		guild.Channels = append(guild.Channels, convertChannel(c))
	}
	return guild, nil
}

func (d *Disgo) CreateRole(ctx context.Context, name string, perms uint64, reason string) (Role, error) {
	p := permissions(perms)
	role, err := d.client.Rest.CreateRole(d.guildID, dapi.RoleCreate{Name: name, Permissions: &p},
		rest.WithCtx(ctx), rest.WithReason(reason))
	if err != nil {
		return Role{}, fmt.Errorf("create role %q: %w", name, err)
	}
	return convertRole(*role), nil
}

func (d *Disgo) EditRole(ctx context.Context, id string, perms uint64, reason string) error {
	roleID, err := parseID(id)
	if err != nil {
		return err
	}
	p := permissions(perms)
	if _, err := d.client.Rest.UpdateRole(d.guildID, roleID, dapi.RoleUpdate{Permissions: &p},
		rest.WithCtx(ctx), rest.WithReason(reason)); err != nil {
		return fmt.Errorf("update role %s: %w", id, err)
	}
	return nil
}

func (d *Disgo) PositionRoles(ctx context.Context, ids []string, reason string) error {
	updates := make([]dapi.RolePositionUpdate, 0, len(ids))
	for i, id := range ids {
		roleID, err := parseID(id)
		if err != nil {
			return err
		}
		position := len(ids) - i
		updates = append(updates, dapi.RolePositionUpdate{ID: roleID, Position: &position})
	}
	if _, err := d.client.Rest.UpdateRolePositions(d.guildID, updates,
		rest.WithCtx(ctx), rest.WithReason(reason)); err != nil {
		return fmt.Errorf("update role positions: %w", err)
	}
	return nil
}

func (d *Disgo) AddMemberRole(ctx context.Context, userID, roleID, reason string) error {
	user, err := parseID(userID)
	if err != nil {
		return err
	}
	role, err := parseID(roleID)
	if err != nil {
		return err
	}
	if err := d.client.Rest.AddMemberRole(d.guildID, user, role,
		rest.WithCtx(ctx), rest.WithReason(reason)); err != nil {
		return fmt.Errorf("add role %s to %s: %w", roleID, userID, err)
	}
	return nil
}

func (d *Disgo) GuildMemberRoles(ctx context.Context, userID string) ([]string, error) {
	user, err := parseID(userID)
	if err != nil {
		return nil, err
	}
	member, err := d.client.Rest.GetMember(d.guildID, user, rest.WithCtx(ctx))
	if err != nil {
		return nil, fmt.Errorf("get member %s: %w", userID, err)
	}
	ids := make([]string, 0, len(member.RoleIDs))
	for _, id := range member.RoleIDs {
		ids = append(ids, id.String())
	}
	return ids, nil
}

func (d *Disgo) EditEveryonePermissions(ctx context.Context, perms uint64, reason string) error {
	return d.EditRole(ctx, d.guildID.String(), perms, reason)
}

func (d *Disgo) CreateChannel(ctx context.Context, c ChannelSpec, reason string) (Channel, error) {
	create, err := channelCreate(c)
	if err != nil {
		return Channel{}, err
	}
	created, err := d.client.Rest.CreateGuildChannel(d.guildID, create,
		rest.WithCtx(ctx), rest.WithReason(reason))
	if err != nil {
		return Channel{}, fmt.Errorf("create channel %q: %w", c.Name, err)
	}
	return convertChannel(created), nil
}

func (d *Disgo) EditChannel(ctx context.Context, id string, c ChannelSpec, reason string) error {
	channelID, err := parseID(id)
	if err != nil {
		return err
	}
	update, err := channelUpdate(c)
	if err != nil {
		return err
	}
	if _, err := d.client.Rest.UpdateChannel(channelID, update,
		rest.WithCtx(ctx), rest.WithReason(reason)); err != nil {
		return fmt.Errorf("update channel %q: %w", c.Name, err)
	}
	return nil
}

func (d *Disgo) Send(ctx context.Context, channelID, content string, suppressNotifications bool) (string, error) {
	id, err := parseID(channelID)
	if err != nil {
		return "", err
	}
	create := dapi.MessageCreate{Content: content}
	if suppressNotifications {
		create.Flags = dapi.MessageFlagSuppressNotifications
	}
	message, err := d.client.Rest.CreateMessage(id, create, rest.WithCtx(ctx))
	if err != nil {
		return "", fmt.Errorf("send to %s: %w", channelID, err)
	}
	return message.ID.String(), nil
}

func (d *Disgo) Messages(ctx context.Context, channelID, afterID string, limit int) ([]Message, error) {
	id, err := parseID(channelID)
	if err != nil {
		return nil, err
	}
	after, err := optionalID(afterID)
	if err != nil {
		return nil, err
	}
	messages, err := d.client.Rest.GetMessages(id, 0, 0, after, limit, rest.WithCtx(ctx))
	if err != nil {
		return nil, fmt.Errorf("read messages of %s: %w", channelID, err)
	}
	out := make([]Message, 0, len(messages))
	for _, m := range messages {
		out = append(out, convertMessage(m))
	}
	return out, nil
}

func (d *Disgo) GatewayBot(ctx context.Context, botToken string) (SessionStartLimit, error) {
	opts := []rest.RequestOpt{rest.WithCtx(ctx)}
	if botToken != "" {
		opts = append(opts, rest.WithToken(dapi.TokenTypeBot, botToken))
	}
	gateway, err := d.client.Rest.GetGatewayBot(opts...)
	if err != nil {
		return SessionStartLimit{}, fmt.Errorf("get gateway bot: %w", err)
	}
	limit := gateway.SessionStartLimit
	return SessionStartLimit{
		Total:          limit.Total,
		Remaining:      limit.Remaining,
		ResetAfter:     time.Duration(limit.ResetAfter) * time.Millisecond,
		MaxConcurrency: limit.MaxConcurrency,
	}, nil
}

// Events subscribes once. disgo appends listeners and dispatches to every
// one it ever took, and it reconnects the gateway underneath rather than
// ending the stream, so a second subscription would deliver each event twice
// and leave the first pipeline stalled; a caller that needs a fresh stream
// builds a fresh Disgo.
func (d *Disgo) Events(ctx context.Context) (<-chan Event, error) {
	if !d.subscribed.CompareAndSwap(false, true) {
		return nil, errors.New("gateway already subscribed")
	}
	raw := make(chan Event, eventBuffer)
	deliver := func(e Event) {
		select {
		case raw <- e:
		case <-ctx.Done():
		}
	}
	d.client.AddEventListeners(
		bot.NewListenerFunc(func(e *events.GuildMessageCreate) {
			deliver(messageEvent(EventMessageCreate, e.GenericGuildMessage))
		}),
		bot.NewListenerFunc(func(e *events.GuildMessageUpdate) {
			deliver(messageEvent(EventMessageUpdate, e.GenericGuildMessage))
		}),
		bot.NewListenerFunc(func(e *events.GuildMessageDelete) {
			deliver(messageEvent(EventMessageDelete, e.GenericGuildMessage))
		}),
		bot.NewListenerFunc(func(e *events.GuildAuditLogEntryCreate) {
			deliver(auditEvent(e.AuditLogEntry))
		}),
		bot.NewListenerFunc(func(e *events.ThreadCreate) {
			deliver(threadEvent(e.Thread))
		}),
		bot.NewListenerFunc(func(*events.Ready) {
			deliver(Event{Kind: EventGatewayReady, At: time.Now().UTC()})
		}),
		bot.NewListenerFunc(func(*events.Resumed) {
			deliver(Event{Kind: EventGatewayResume, At: time.Now().UTC()})
		}),
	)
	if err := d.client.OpenGateway(ctx); err != nil {
		return nil, fmt.Errorf("open gateway: %w", err)
	}

	out := make(chan Event)
	go func() {
		defer close(out)
		defer d.client.Close(context.WithoutCancel(ctx))
		for {
			select {
			case <-ctx.Done():
				return
			case e := <-raw:
				select {
				case out <- e:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// messageEvent reads the ids off the generic event rather than the message,
// which a delete carries only when the message was cached.
func messageEvent(kind EventKind, e *events.GenericGuildMessage) Event {
	event := Event{
		Kind:      kind,
		At:        time.Now().UTC(),
		ChannelID: e.ChannelID.String(),
		MessageID: e.MessageID.String(),
		Content:   e.Message.Content,
	}
	if e.Message.Author.ID != 0 {
		event.AuthorID = e.Message.Author.ID.String()
	}
	return event
}

func auditEvent(e dapi.AuditLogEntry) Event {
	event := Event{
		Kind:    EventAuditLogEntry,
		At:      time.Now().UTC(),
		Action:  strconv.Itoa(int(e.ActionType)),
		ActorID: e.UserID.String(),
	}
	if e.TargetID != nil {
		event.TargetID = e.TargetID.String()
	}
	if e.Reason != nil {
		event.Reason = *e.Reason
	}
	return event
}

func threadEvent(t dapi.GuildThread) Event {
	return Event{
		Kind:     EventThreadCreate,
		At:       time.Now().UTC(),
		ThreadID: t.ID().String(),
		ParentID: parentID(t),
		Name:     t.Name(),
	}
}

func parentID(c dapi.GuildChannel) string {
	if id := c.ParentID(); id != nil {
		return id.String()
	}
	return ""
}

func convertRole(r dapi.Role) Role {
	role := Role{
		ID:          r.ID.String(),
		Name:        r.Name,
		Position:    r.Position,
		Permissions: permissionBits(r.Permissions),
		Managed:     r.Managed,
	}
	if r.Tags != nil && r.Tags.BotID != nil {
		role.BotID = r.Tags.BotID.String()
	}
	return role
}

func convertChannel(c dapi.GuildChannel) Channel {
	channel := Channel{
		ID:       c.ID().String(),
		Name:     c.Name(),
		Type:     ChannelType(c.Type()),
		ParentID: parentID(c),
		Position: c.Position(),
	}
	for _, o := range c.PermissionOverwrites() {
		switch v := o.(type) {
		case dapi.RolePermissionOverwrite:
			channel.Overwrites = append(channel.Overwrites, Overwrite{
				ID:    v.RoleID.String(),
				Type:  OverwriteRole,
				Allow: permissionBits(v.Allow),
				Deny:  permissionBits(v.Deny),
			})
		case dapi.MemberPermissionOverwrite:
			channel.Overwrites = append(channel.Overwrites, Overwrite{
				ID:    v.UserID.String(),
				Type:  OverwriteMember,
				Allow: permissionBits(v.Allow),
				Deny:  permissionBits(v.Deny),
			})
		}
	}
	return channel
}

func convertMessage(m dapi.Message) Message {
	return Message{
		ID:        m.ID.String(),
		ChannelID: m.ChannelID.String(),
		AuthorID:  m.Author.ID.String(),
		AuthorBot: m.Author.Bot,
		Content:   m.Content,
		CreatedAt: m.CreatedAt,
	}
}

func channelCreate(c ChannelSpec) (dapi.GuildChannelCreate, error) {
	overwrites, err := convertOverwrites(c.Overwrites)
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case ChannelCategory:
		return dapi.GuildCategoryChannelCreate{Name: c.Name, PermissionOverwrites: overwrites}, nil
	case ChannelText:
		parent, err := optionalID(c.ParentID)
		if err != nil {
			return nil, err
		}
		return dapi.GuildTextChannelCreate{Name: c.Name, ParentID: parent, PermissionOverwrites: overwrites}, nil
	default:
		return nil, fmt.Errorf("channel %q: unsupported type %d", c.Name, c.Type)
	}
}

func channelUpdate(c ChannelSpec) (dapi.GuildChannelUpdate, error) {
	overwrites, err := convertOverwrites(c.Overwrites)
	if err != nil {
		return nil, err
	}
	switch c.Type {
	case ChannelCategory:
		return dapi.GuildCategoryChannelUpdate{Name: &c.Name, PermissionOverwrites: &overwrites}, nil
	case ChannelText:
		update := dapi.GuildTextChannelUpdate{Name: &c.Name, PermissionOverwrites: &overwrites}
		if c.ParentID != "" {
			parent, err := parseID(c.ParentID)
			if err != nil {
				return nil, err
			}
			update.ParentID = &parent
		}
		return update, nil
	default:
		return nil, fmt.Errorf("channel %q: unsupported type %d", c.Name, c.Type)
	}
}

func convertOverwrites(overwrites []Overwrite) ([]dapi.PermissionOverwrite, error) {
	out := make([]dapi.PermissionOverwrite, 0, len(overwrites))
	for _, o := range overwrites {
		id, err := parseID(o.ID)
		if err != nil {
			return nil, err
		}
		switch o.Type {
		case OverwriteRole:
			out = append(out, dapi.RolePermissionOverwrite{
				RoleID: id, Allow: permissions(o.Allow), Deny: permissions(o.Deny),
			})
		case OverwriteMember:
			out = append(out, dapi.MemberPermissionOverwrite{
				UserID: id, Allow: permissions(o.Allow), Deny: permissions(o.Deny),
			})
		default:
			return nil, fmt.Errorf("overwrite %s: unsupported type %d", o.ID, o.Type)
		}
	}
	return out, nil
}

func permissions(p uint64) dapi.Permissions {
	return dapi.Permissions(p & math.MaxInt64)
}

func permissionBits(p dapi.Permissions) uint64 {
	if p < 0 {
		return 0
	}
	return uint64(p)
}

func parseID(s string) (snowflake.ID, error) {
	id, err := snowflake.Parse(s)
	if err != nil {
		return 0, fmt.Errorf("discord id %q: %w", s, err)
	}
	return id, nil
}

func optionalID(s string) (snowflake.ID, error) {
	if s == "" {
		return 0, nil
	}
	return parseID(s)
}
