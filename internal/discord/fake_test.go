package discord

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

const (
	testGuildID        = "100000000000000001"
	testOperatorID     = "100000000000000002"
	testSupervisorApp  = "100000000000000003"
	testAtlasApp       = "200000000000000001"
	testIrisApp        = "200000000000000002"
	testDisabledMgrApp = "200000000000000003"
)

var _ Client = (*fakeClient)(nil)

// fakeClient is a guild in memory: enough of Discord's model for the
// reconciler to converge on, and a log of every write it performed.
type fakeClient struct {
	roles    []Role
	channels []Channel
	members  map[string][]string
	messages map[string][]Message

	next   int
	writes []string
}

func newFake() *fakeClient {
	return &fakeClient{
		roles: []Role{
			{ID: testGuildID, Name: "@everyone", Position: 0},
			{ID: "300000000000000001", Name: RoleSupervisor, Position: 2, Permissions: SupervisorPermissions},
		},
		members: map[string][]string{
			testSupervisorApp: {"300000000000000001"},
			testAtlasApp:      {},
			testIrisApp:       {},
		},
		messages: map[string][]Message{},
	}
}

func (f *fakeClient) id() string {
	f.next++
	return fmt.Sprintf("40000000000000%04d", f.next)
}

func (f *fakeClient) record(format string, args ...any) {
	f.writes = append(f.writes, fmt.Sprintf(format, args...))
}

func (f *fakeClient) Guild(context.Context) (Guild, error) {
	g := Guild{ID: testGuildID, Roles: slices.Clone(f.roles)}
	for _, c := range f.channels {
		c.Overwrites = slices.Clone(c.Overwrites)
		g.Channels = append(g.Channels, c)
	}
	return g, nil
}

func (f *fakeClient) CreateRole(_ context.Context, name string, perms uint64, _ string) (Role, error) {
	f.record("CreateRole %s", name)
	role := Role{ID: f.id(), Name: name, Position: 1, Permissions: perms}
	f.roles = append(f.roles, role)
	return role, nil
}

func (f *fakeClient) EditRole(_ context.Context, id string, perms uint64, _ string) error {
	f.record("EditRole %s", id)
	for i := range f.roles {
		if f.roles[i].ID == id {
			f.roles[i].Permissions = perms
			return nil
		}
	}
	return fmt.Errorf("no role %s", id)
}

func (f *fakeClient) PositionRoles(_ context.Context, ids []string, _ string) error {
	f.record("PositionRoles %v", ids)
	for i, id := range ids {
		for j := range f.roles {
			if f.roles[j].ID == id {
				f.roles[j].Position = len(ids) - i
			}
		}
	}
	return nil
}

func (f *fakeClient) AddMemberRole(_ context.Context, userID, roleID, _ string) error {
	f.record("AddMemberRole %s %s", userID, roleID)
	f.members[userID] = append(f.members[userID], roleID)
	return nil
}

func (f *fakeClient) GuildMemberRoles(_ context.Context, userID string) ([]string, error) {
	held, ok := f.members[userID]
	if !ok {
		return nil, fmt.Errorf("no member %s", userID)
	}
	return slices.Clone(held), nil
}

func (f *fakeClient) EditEveryonePermissions(_ context.Context, perms uint64, _ string) error {
	f.record("EditEveryonePermissions %d", perms)
	for i := range f.roles {
		if f.roles[i].ID == testGuildID {
			f.roles[i].Permissions = perms
			return nil
		}
	}
	return fmt.Errorf("no @everyone role")
}

func (f *fakeClient) CreateChannel(_ context.Context, c ChannelSpec, _ string) (Channel, error) {
	f.record("CreateChannel %s", c.Name)
	channel := Channel{
		ID:         f.id(),
		Name:       c.Name,
		Type:       c.Type,
		ParentID:   c.ParentID,
		Position:   len(f.channels),
		Overwrites: slices.Clone(c.Overwrites),
	}
	f.channels = append(f.channels, channel)
	return channel, nil
}

func (f *fakeClient) EditChannel(_ context.Context, id string, c ChannelSpec, _ string) error {
	f.record("EditChannel %s", id)
	for i := range f.channels {
		if f.channels[i].ID == id {
			f.channels[i].ParentID = c.ParentID
			f.channels[i].Overwrites = slices.Clone(c.Overwrites)
			return nil
		}
	}
	return fmt.Errorf("no channel %s", id)
}

func (f *fakeClient) Send(_ context.Context, channelID, content string, _ bool) (string, error) {
	f.record("Send %s", channelID)
	m := Message{ID: f.id(), ChannelID: channelID, Content: content, CreatedAt: time.Unix(0, 0).UTC()}
	f.messages[channelID] = append(f.messages[channelID], m)
	return m.ID, nil
}

func (f *fakeClient) Messages(_ context.Context, channelID, afterID string, limit int) ([]Message, error) {
	var out []Message
	for _, m := range f.messages[channelID] {
		if m.ID > afterID && len(out) < limit {
			out = append(out, m)
		}
	}
	return out, nil
}

func (f *fakeClient) GatewayBot(context.Context, string) (SessionStartLimit, error) {
	return SessionStartLimit{Total: 1000, Remaining: 1000, MaxConcurrency: 1}, nil
}

func (f *fakeClient) Events(context.Context) (<-chan Event, error) {
	ch := make(chan Event)
	close(ch)
	return ch, nil
}

func (f *fakeClient) role(name string) (Role, bool) {
	for _, r := range f.roles {
		if r.Name == name {
			return r, true
		}
	}
	return Role{}, false
}

func (f *fakeClient) channel(name string, kind ChannelType) (Channel, bool) {
	for _, c := range f.channels {
		if c.Name == name && c.Type == kind {
			return c, true
		}
	}
	return Channel{}, false
}

func testRoster() *roster.Roster {
	return &roster.Roster{
		Farm: roster.Farm{Discord: roster.DiscordConfig{
			GuildID:                 testGuildID,
			OperatorUserID:          testOperatorID,
			SupervisorApplicationID: testSupervisorApp,
		}},
		Teams: []roster.Team{
			{Name: "assistant", Label: "TEAM · ASSISTANT"},
			{Name: "snowsys", Label: "TEAM · SNOWSYS"},
			{Name: "research", Label: "TEAM · RESEARCH"},
			{Name: "swe", Label: "TEAM · SOFTWARE ENGINEERING"},
			{Name: "avalanche", Label: "TEAM · AVALANCHE"},
		},
		Agents: []roster.Agent{
			{Name: "atlas", Tier: roster.TierManager, DiscordApplicationID: testAtlasApp, Enabled: true},
			{Name: "iris", Tier: roster.TierManager, DiscordApplicationID: testIrisApp, Enabled: true},
			{Name: "nyx", Tier: roster.TierManager, DiscordApplicationID: testDisabledMgrApp},
			{Name: "hestia", Tier: roster.TierWorker, Enabled: true},
		},
	}
}
