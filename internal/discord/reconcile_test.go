package discord

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/SnowballSH/snowfarm/internal/roster"
)

var wantCategories = []string{
	CategoryOperator,
	CategoryManagers,
	"TEAM · ASSISTANT",
	"TEAM · SNOWSYS",
	"TEAM · RESEARCH",
	"TEAM · SOFTWARE ENGINEERING",
	"TEAM · AVALANCHE",
}

var wantChannels = map[string]string{
	ChannelCommand:       CategoryOperator,
	ChannelFarmControl:   CategoryOperator,
	ChannelFarmStatus:    CategoryOperator,
	ChannelManagers:      CategoryManagers,
	"#assistant-general": "TEAM · ASSISTANT",
	"#assistant-log":     "TEAM · ASSISTANT",
	"#snowsys-general":   "TEAM · SNOWSYS",
	"#snowsys-log":       "TEAM · SNOWSYS",
	"#research-general":  "TEAM · RESEARCH",
	"#research-log":      "TEAM · RESEARCH",
	"#swe-general":       "TEAM · SOFTWARE ENGINEERING",
	"#swe-log":           "TEAM · SOFTWARE ENGINEERING",
	"#avalanche-general": "TEAM · AVALANCHE",
	"#avalanche-log":     "TEAM · AVALANCHE",
}

func reconcile(t *testing.T, f *fakeClient, r *roster.Roster) State {
	t.Helper()
	rc := &Reconciler{Client: f, Roster: r}
	state, err := rc.Reconcile(context.Background())
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	return state
}

func writesOfKind(writes []string, kind string) []string {
	var out []string
	for _, w := range writes {
		if strings.HasPrefix(w, kind+" ") {
			out = append(out, w)
		}
	}
	return out
}

func channelIndex(t *testing.T, f *fakeClient, name string) int {
	t.Helper()
	for i := range f.channels {
		if f.channels[i].Name == name && f.channels[i].Type == ChannelText {
			return i
		}
	}
	t.Fatalf("no text channel %q", name)
	return -1
}

func overwriteFor(t *testing.T, c Channel, id string) Overwrite {
	t.Helper()
	for _, o := range c.Overwrites {
		if o.ID == id {
			return o
		}
	}
	t.Fatalf("channel %q carries no overwrite for %s", c.Name, id)
	return Overwrite{}
}

func TestReconcileFromEmpty(t *testing.T) {
	f := newFake()
	state := reconcile(t, f, testRoster())

	supervisor, ok := f.role(RoleSupervisor)
	if !ok {
		t.Fatal("the Supervisor role vanished")
	}
	manager, ok := f.role(RoleManager)
	if !ok {
		t.Fatal("no Manager role was created")
	}
	if manager.Permissions != ManagerPermissions {
		t.Errorf("Manager permissions = %d, want %d", manager.Permissions, ManagerPermissions)
	}
	if manager.Position >= supervisor.Position {
		t.Errorf("Manager at position %d, want beneath Supervisor at %d", manager.Position, supervisor.Position)
	}

	var categories []string
	for _, c := range f.channels {
		if c.Type == ChannelCategory {
			categories = append(categories, c.Name)
		}
	}
	if !slices.Equal(categories, wantCategories) {
		t.Errorf("categories = %q, want %q", categories, wantCategories)
	}

	for _, name := range wantCategories {
		category, ok := f.channel(name, ChannelCategory)
		if !ok {
			t.Fatalf("category %q was not created", name)
		}
		if deny := overwriteFor(t, category, testGuildID); deny.Deny&PermViewChannel == 0 {
			t.Errorf("category %q does not deny @everyone VIEW_CHANNEL", name)
		}
		if allow := overwriteFor(t, category, supervisor.ID); allow.Allow&PermViewChannel == 0 {
			t.Errorf("category %q is not visible to Supervisor", name)
		}
	}

	for key, parent := range wantChannels {
		channel, ok := f.channel(key[1:], ChannelText)
		if !ok {
			t.Fatalf("channel %q was not created", key)
		}
		category, _ := f.channel(parent, ChannelCategory)
		if channel.ParentID != category.ID {
			t.Errorf("channel %q parent = %q, want category %q", key, channel.ParentID, parent)
		}
		if deny := overwriteFor(t, channel, testGuildID); deny.Deny&PermViewChannel == 0 {
			t.Errorf("channel %q does not deny @everyone VIEW_CHANNEL", key)
		}
		if allow := overwriteFor(t, channel, supervisor.ID); allow.Allow&PermViewChannel == 0 {
			t.Errorf("channel %q is not visible to Supervisor", key)
		}
		if allow := overwriteFor(t, channel, testOperatorID); allow.Allow&PermViewChannel == 0 {
			t.Errorf("channel %q is not visible to the operator", key)
		}
		forManager := overwriteFor(t, channel, manager.ID)
		if key == ChannelFarmControl {
			if forManager.Deny&PermViewChannel == 0 {
				t.Errorf("channel %q does not deny Manager VIEW_CHANNEL", key)
			}
			continue
		}
		if forManager.Allow&PermViewChannel == 0 {
			t.Errorf("channel %q is not visible to Manager", key)
		}
	}

	if got := state.Roles[RoleManager]; got != manager.ID {
		t.Errorf("state Manager id = %q, want %q", got, manager.ID)
	}
	if got := state.Roles[RoleSupervisor]; got != supervisor.ID {
		t.Errorf("state Supervisor id = %q, want %q", got, supervisor.ID)
	}
	if len(state.Channels) != len(wantChannels) {
		t.Errorf("state has %d channels, want %d", len(state.Channels), len(wantChannels))
	}
	for key := range wantChannels {
		channel, _ := f.channel(key[1:], ChannelText)
		if got := state.Channels[key]; got != channel.ID {
			t.Errorf("state channel %q = %q, want %q", key, got, channel.ID)
		}
	}
}

func TestReconcileIsIdempotent(t *testing.T) {
	f := newFake()
	r := testRoster()
	first := reconcile(t, f, r)

	f.writes = nil
	second := reconcile(t, f, r)

	if len(f.writes) != 0 {
		t.Errorf("second reconcile issued writes: %q", f.writes)
	}
	if len(second.Channels) != len(first.Channels) {
		t.Errorf("second reconcile returned %d channels, first returned %d", len(second.Channels), len(first.Channels))
	}
	for key, id := range first.Channels {
		if second.Channels[key] != id {
			t.Errorf("channel %q moved from %q to %q", key, id, second.Channels[key])
		}
	}
}

func TestReconcileRefusesManagedRole(t *testing.T) {
	f := newFake()
	f.roles = append(f.roles, Role{
		ID:      "300000000000000009",
		Name:    "atlas",
		Managed: true,
		BotID:   testAtlasApp,
	})

	rc := &Reconciler{Client: f, Roster: testRoster()}
	_, err := rc.Reconcile(context.Background())
	if !errors.Is(err, ErrManagedRole) {
		t.Fatalf("reconcile error = %v, want %v", err, ErrManagedRole)
	}
	if len(f.writes) != 0 {
		t.Errorf("reconcile wrote before refusing: %q", f.writes)
	}
}

func TestEveryoneStripped(t *testing.T) {
	f := newFake()
	f.roles[0].Permissions = PermViewChannel | PermSendMessages

	reconcile(t, f, testRoster())

	everyone, ok := f.role("@everyone")
	if !ok {
		t.Fatal("the @everyone role vanished")
	}
	if everyone.Permissions != 0 {
		t.Errorf("@everyone permissions = %d, want 0", everyone.Permissions)
	}

	f.writes = nil
	reconcile(t, f, testRoster())
	if len(f.writes) != 0 {
		t.Errorf("a stripped @everyone was written again: %q", f.writes)
	}
}

func TestReconcileAssignsManagerRole(t *testing.T) {
	f := newFake()
	r := testRoster()
	state := reconcile(t, f, r)

	managerID := state.Roles[RoleManager]
	for _, app := range []string{testAtlasApp, testIrisApp} {
		held, err := f.GuildMemberRoles(context.Background(), app)
		if err != nil {
			t.Fatalf("member roles for %s: %v", app, err)
		}
		if !slices.Contains(held, managerID) {
			t.Errorf("manager bot %s holds %q, want the Manager role %q", app, held, managerID)
		}
	}
	if _, ok := f.members[testDisabledMgrApp]; ok {
		t.Errorf("a disabled manager's bot was given a role")
	}

	f.writes = nil
	reconcile(t, f, r)
	for _, w := range f.writes {
		t.Errorf("second reconcile wrote %q", w)
	}
}

// A bot may manipulate only roles strictly below its own highest, and
// Supervisor is the supervisor bot's highest, so a Manager that is not
// beneath it is the operator's ceremony to repair, not a write to attempt.
func TestReconcileNeedsSupervisorAboveManager(t *testing.T) {
	tests := []struct {
		name   string
		guild  func(f *fakeClient)
		writes []string
	}{
		{
			name:   "the created Manager lands level with a bottom Supervisor",
			guild:  func(f *fakeClient) { f.roles[1].Position = 1 },
			writes: []string{"CreateRole " + RoleManager},
		},
		{
			name: "an existing Manager sits above Supervisor",
			guild: func(f *fakeClient) {
				f.roles = append(f.roles, Role{
					ID:          managerRoleID,
					Name:        RoleManager,
					Position:    3,
					Permissions: ManagerPermissions,
				})
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			f := newFake()
			tc.guild(f)

			rc := &Reconciler{Client: f, Roster: testRoster()}
			_, err := rc.Reconcile(context.Background())
			if !errors.Is(err, ErrSupervisorBeneathManager) {
				t.Fatalf("reconcile error = %v, want %v", err, ErrSupervisorBeneathManager)
			}
			if !slices.Equal(f.writes, tc.writes) {
				t.Errorf("writes = %q, want %q", f.writes, tc.writes)
			}
		})
	}
}

func TestReconcileRepairsManagerPermissions(t *testing.T) {
	f := newFake()
	f.roles = append(f.roles, Role{
		ID:          managerRoleID,
		Name:        RoleManager,
		Position:    1,
		Permissions: ManagerPermissions | PermMentionEveryone,
	})
	r := testRoster()

	state := reconcile(t, f, r)

	if got := state.Roles[RoleManager]; got != managerRoleID {
		t.Errorf("state Manager id = %q, want the existing role %q", got, managerRoleID)
	}
	if got := writesOfKind(f.writes, "EditRole"); !slices.Equal(got, []string{"EditRole " + managerRoleID}) {
		t.Errorf("role writes = %q, want one EditRole of %s", got, managerRoleID)
	}
	if got := writesOfKind(f.writes, "CreateRole"); len(got) != 0 {
		t.Errorf("a Manager role was created alongside the existing one: %q", got)
	}
	manager, ok := f.role(RoleManager)
	if !ok {
		t.Fatal("the Manager role vanished")
	}
	if manager.Permissions != ManagerPermissions {
		t.Errorf("Manager permissions = %d, want %d", manager.Permissions, ManagerPermissions)
	}

	f.writes = nil
	reconcile(t, f, r)
	if len(f.writes) != 0 {
		t.Errorf("the repaired role was written again: %q", f.writes)
	}
}

func TestReconcileRepairsDriftedChannels(t *testing.T) {
	f := newFake()
	r := testRoster()
	reconcile(t, f, r)

	// Two shapes of overwrite drift, so both of sameOverwrites' mismatch
	// branches decide a repair: a differing value, and a missing entry.
	stripped := channelIndex(t, f, "command")
	for i, o := range f.channels[stripped].Overwrites {
		if o.ID == testGuildID {
			f.channels[stripped].Overwrites[i].Deny = 0
		}
	}
	shortened := channelIndex(t, f, "managers")
	f.channels[shortened].Overwrites = slices.DeleteFunc(
		slices.Clone(f.channels[shortened].Overwrites),
		func(o Overwrite) bool { return o.ID == testOperatorID },
	)
	orphaned := channelIndex(t, f, "snowsys-log")
	f.channels[orphaned].ParentID = ""

	f.writes = nil
	second := reconcile(t, f, r)

	want := []string{
		"EditChannel " + f.channels[stripped].ID,
		"EditChannel " + f.channels[shortened].ID,
		"EditChannel " + f.channels[orphaned].ID,
	}
	if !slices.Equal(f.writes, want) {
		t.Fatalf("writes = %q, want %q", f.writes, want)
	}
	if deny := overwriteFor(t, f.channels[stripped], testGuildID); deny.Deny&PermViewChannel == 0 {
		t.Errorf("#command still does not deny @everyone VIEW_CHANNEL")
	}
	if allow := overwriteFor(t, f.channels[shortened], testOperatorID); allow.Allow&PermViewChannel == 0 {
		t.Errorf("#managers is still not visible to the operator")
	}
	category, ok := f.channel("TEAM · SNOWSYS", ChannelCategory)
	if !ok {
		t.Fatal("the TEAM · SNOWSYS category vanished")
	}
	if got := f.channels[orphaned].ParentID; got != category.ID {
		t.Errorf("#snowsys-log parent = %q, want %q", got, category.ID)
	}
	if got := second.Channels[ChannelCommand]; got != f.channels[stripped].ID {
		t.Errorf("state channel %q = %q, want %q", ChannelCommand, got, f.channels[stripped].ID)
	}

	f.writes = nil
	reconcile(t, f, r)
	if len(f.writes) != 0 {
		t.Errorf("the repaired channels were written again: %q", f.writes)
	}
}

func TestReconcileNeedsSupervisorRole(t *testing.T) {
	f := newFake()
	f.roles = f.roles[:1]
	f.members[testSupervisorApp] = nil

	rc := &Reconciler{Client: f, Roster: testRoster()}
	_, err := rc.Reconcile(context.Background())
	if !errors.Is(err, ErrNoSupervisorRole) {
		t.Fatalf("reconcile error = %v, want %v", err, ErrNoSupervisorRole)
	}
}

func TestReconcileNeedsManageRolesAndChannels(t *testing.T) {
	f := newFake()
	f.members[testSupervisorApp] = nil

	rc := &Reconciler{Client: f, Roster: testRoster()}
	_, err := rc.Reconcile(context.Background())
	if !errors.Is(err, ErrMissingPermissions) {
		t.Fatalf("reconcile error = %v, want %v", err, ErrMissingPermissions)
	}
	if len(f.writes) != 0 {
		t.Errorf("reconcile wrote without permissions: %q", f.writes)
	}
}
