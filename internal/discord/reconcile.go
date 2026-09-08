package discord

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

var (
	// ErrManagedRole is the invite that granted permissions: a bot invited
	// with permissions=0 leaves no integration role behind, so one that
	// exists means the guild is not the model the design assumes.
	ErrManagedRole = errors.New("a role is managed by a farm application")

	// ErrNoSupervisorRole and ErrMissingPermissions are the two halves of
	// the F0 ceremony a bot cannot perform for itself: the operator creates
	// the Supervisor role with MANAGE_ROLES and MANAGE_CHANNELS, assigns it
	// to the supervisor bot, and positions it above Manager.
	ErrNoSupervisorRole   = errors.New("the Supervisor role does not exist; the operator must create it and assign it to the supervisor bot")
	ErrMissingPermissions = errors.New("the supervisor bot holds no role granting the permissions reconcile writes with; the operator must assign it the Supervisor role")
)

const (
	reasonRoles     = "snowfarm reconcile: roles"
	reasonEveryone  = "snowfarm reconcile: strip @everyone"
	reasonChannels  = "snowfarm reconcile: channels"
	reasonAdmission = "snowfarm reconcile: manager admission"
)

type Reconciler struct {
	Client Client
	Roster *roster.Roster
}

type plannedChannel struct {
	key        string
	overwrites []Overwrite
}

type plannedCategory struct {
	name       string
	overwrites []Overwrite
	channels   []plannedChannel
}

// Reconcile makes the guild match the roster: the Manager role beneath the
// operator's Supervisor, one category per team plus OPERATOR and MANAGERS,
// their channels and overwrites, and the Manager role on every enabled
// manager's bot member. It writes only what differs, so a second run against
// a reconciled guild issues no request.
func (rc *Reconciler) Reconcile(ctx context.Context) (State, error) {
	guild, err := rc.Client.Guild(ctx)
	if err != nil {
		return State{}, fmt.Errorf("read guild: %w", err)
	}
	if err := rc.refuseManagedRoles(guild); err != nil {
		return State{}, err
	}
	supervisor, ok := roleByName(guild.Roles, RoleSupervisor)
	if !ok {
		return State{}, ErrNoSupervisorRole
	}
	if err := rc.checkPermissions(ctx, guild); err != nil {
		return State{}, err
	}
	if err := rc.stripEveryone(ctx, guild); err != nil {
		return State{}, err
	}
	manager, err := rc.ensureManagerRole(ctx, guild, supervisor)
	if err != nil {
		return State{}, err
	}
	channels, err := rc.ensureChannels(ctx, guild, supervisor.ID, manager.ID)
	if err != nil {
		return State{}, err
	}
	if err := rc.admitManagers(ctx, manager.ID); err != nil {
		return State{}, err
	}
	return State{
		Roles:    map[string]string{RoleSupervisor: supervisor.ID, RoleManager: manager.ID},
		Channels: channels,
	}, nil
}

func (rc *Reconciler) refuseManagedRoles(guild Guild) error {
	apps := []string{rc.Roster.Farm.Discord.SupervisorApplicationID}
	for _, a := range rc.Roster.Agents {
		if a.DiscordApplicationID != "" {
			apps = append(apps, a.DiscordApplicationID)
		}
	}
	for _, r := range guild.Roles {
		if r.BotID != "" && slices.Contains(apps, r.BotID) {
			return fmt.Errorf("%w: role %q (%s) belongs to application %s", ErrManagedRole, r.Name, r.ID, r.BotID)
		}
	}
	return nil
}

func (rc *Reconciler) checkPermissions(ctx context.Context, guild Guild) error {
	held, err := rc.Client.GuildMemberRoles(ctx, rc.Roster.Farm.Discord.SupervisorApplicationID)
	if err != nil {
		return fmt.Errorf("read supervisor bot roles: %w", err)
	}
	var perms uint64
	if everyone, ok := roleByID(guild.Roles, guild.ID); ok {
		perms |= everyone.Permissions
	}
	for _, id := range held {
		if r, ok := roleByID(guild.Roles, id); ok {
			perms |= r.Permissions
		}
	}
	if perms&PermAdministrator != 0 {
		return nil
	}
	var missing []string
	for _, need := range []struct {
		name string
		bit  uint64
	}{{"MANAGE_ROLES", PermManageRoles}, {"MANAGE_CHANNELS", PermManageChannels}} {
		if perms&need.bit == 0 {
			missing = append(missing, need.name)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%w (missing %s)", ErrMissingPermissions, strings.Join(missing, " and "))
	}
	return nil
}

func (rc *Reconciler) stripEveryone(ctx context.Context, guild Guild) error {
	everyone, ok := roleByID(guild.Roles, guild.ID)
	if !ok || everyone.Permissions == 0 {
		return nil
	}
	if err := rc.Client.EditEveryonePermissions(ctx, 0, reasonEveryone); err != nil {
		return fmt.Errorf("strip @everyone: %w", err)
	}
	return nil
}

func (rc *Reconciler) ensureManagerRole(ctx context.Context, guild Guild, supervisor Role) (Role, error) {
	manager, ok := roleByName(guild.Roles, RoleManager)
	switch {
	case !ok:
		created, err := rc.Client.CreateRole(ctx, RoleManager, ManagerPermissions, reasonRoles)
		if err != nil {
			return Role{}, fmt.Errorf("create the %s role: %w", RoleManager, err)
		}
		manager = created
	case manager.Permissions != ManagerPermissions:
		if err := rc.Client.EditRole(ctx, manager.ID, ManagerPermissions, reasonRoles); err != nil {
			return Role{}, fmt.Errorf("set %s permissions: %w", RoleManager, err)
		}
		manager.Permissions = ManagerPermissions
	}
	if manager.Position >= supervisor.Position {
		if err := rc.Client.PositionRoles(ctx, []string{supervisor.ID, manager.ID}, reasonRoles); err != nil {
			return Role{}, fmt.Errorf("position %s beneath %s: %w", RoleManager, RoleSupervisor, err)
		}
	}
	return manager, nil
}

func (rc *Reconciler) ensureChannels(ctx context.Context, guild Guild, supervisorID, managerID string) (map[string]string, error) {
	ids := make(map[string]string)
	for _, category := range rc.plan(guild.ID, supervisorID, managerID) {
		parent, err := rc.ensureChannel(ctx, guild, ChannelSpec{
			Name:       category.name,
			Type:       ChannelCategory,
			Overwrites: category.overwrites,
		})
		if err != nil {
			return nil, err
		}
		for _, channel := range category.channels {
			id, err := rc.ensureChannel(ctx, guild, ChannelSpec{
				Name:       strings.TrimPrefix(channel.key, "#"),
				Type:       ChannelText,
				ParentID:   parent,
				Overwrites: channel.overwrites,
			})
			if err != nil {
				return nil, err
			}
			ids[channel.key] = id
		}
	}
	return ids, nil
}

func (rc *Reconciler) ensureChannel(ctx context.Context, guild Guild, spec ChannelSpec) (string, error) {
	existing, ok := channelByName(guild.Channels, spec.Name, spec.Type)
	if !ok {
		created, err := rc.Client.CreateChannel(ctx, spec, reasonChannels)
		if err != nil {
			return "", fmt.Errorf("create channel %q: %w", spec.Name, err)
		}
		return created.ID, nil
	}
	if existing.ParentID == spec.ParentID && sameOverwrites(existing.Overwrites, spec.Overwrites) {
		return existing.ID, nil
	}
	if err := rc.Client.EditChannel(ctx, existing.ID, spec, reasonChannels); err != nil {
		return "", fmt.Errorf("edit channel %q: %w", spec.Name, err)
	}
	return existing.ID, nil
}

func (rc *Reconciler) admitManagers(ctx context.Context, managerID string) error {
	for _, m := range rc.Roster.EnabledManagers() {
		held, err := rc.Client.GuildMemberRoles(ctx, m.DiscordApplicationID)
		if err != nil {
			return fmt.Errorf("read %s bot roles: %w", m.Name, err)
		}
		if slices.Contains(held, managerID) {
			continue
		}
		if err := rc.Client.AddMemberRole(ctx, m.DiscordApplicationID, managerID, reasonAdmission); err != nil {
			return fmt.Errorf("give %s the %s role: %w", m.Name, RoleManager, err)
		}
	}
	return nil
}

func (rc *Reconciler) plan(guildID, supervisorID, managerID string) []plannedCategory {
	operator := rc.Roster.Farm.Discord.OperatorUserID
	visible := []Overwrite{
		{ID: guildID, Type: OverwriteRole, Deny: PermViewChannel},
		{ID: supervisorID, Type: OverwriteRole, Allow: PermViewChannel},
		{ID: managerID, Type: OverwriteRole, Allow: PermViewChannel},
		{ID: operator, Type: OverwriteMember, Allow: PermViewChannel},
	}
	operatorOnly := []Overwrite{
		{ID: guildID, Type: OverwriteRole, Deny: PermViewChannel},
		{ID: supervisorID, Type: OverwriteRole, Allow: PermViewChannel},
		{ID: managerID, Type: OverwriteRole, Deny: PermViewChannel},
		{ID: operator, Type: OverwriteMember, Allow: PermViewChannel},
	}

	categories := []plannedCategory{
		{name: CategoryOperator, overwrites: visible, channels: []plannedChannel{
			{key: ChannelCommand, overwrites: visible},
			{key: ChannelFarmControl, overwrites: operatorOnly},
			{key: ChannelFarmStatus, overwrites: visible},
		}},
		{name: CategoryManagers, overwrites: visible, channels: []plannedChannel{
			{key: ChannelManagers, overwrites: visible},
		}},
	}
	for _, team := range rc.Roster.Teams {
		categories = append(categories, plannedCategory{
			name:       team.Label,
			overwrites: visible,
			channels: []plannedChannel{
				{key: "#" + team.Name + "-general", overwrites: visible},
				{key: "#" + team.Name + "-log", overwrites: visible},
			},
		})
	}
	return categories
}

func roleByName(roles []Role, name string) (Role, bool) {
	for _, r := range roles {
		if r.Name == name {
			return r, true
		}
	}
	return Role{}, false
}

func roleByID(roles []Role, id string) (Role, bool) {
	for _, r := range roles {
		if r.ID == id {
			return r, true
		}
	}
	return Role{}, false
}

func channelByName(channels []Channel, name string, kind ChannelType) (Channel, bool) {
	for _, c := range channels {
		if c.Name == name && c.Type == kind {
			return c, true
		}
	}
	return Channel{}, false
}

type overwriteKey struct {
	kind OverwriteType
	id   string
}

func sameOverwrites(have, want []Overwrite) bool {
	if len(have) != len(want) {
		return false
	}
	byKey := make(map[overwriteKey]Overwrite, len(have))
	for _, o := range have {
		byKey[overwriteKey{o.Type, o.ID}] = o
	}
	for _, o := range want {
		if byKey[overwriteKey{o.Type, o.ID}] != o {
			return false
		}
	}
	return true
}
