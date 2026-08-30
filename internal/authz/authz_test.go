package authz

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// fakeGrants decides without a database, so the table below states the rule rather than
// the SQL. The expiry rule is the one thing this cannot cover, and it is tested against a
// real database in store_test.go where the filter actually lives.
type fakeGrants struct {
	grants map[string]*store.Grant // keyed "user/instance"
	err    error
}

func (f *fakeGrants) GrantFor(_ context.Context, userID, instanceID string) (*store.Grant, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.grants[userID+"/"+instanceID], nil
}

func (f *fakeGrants) GrantedInstances(_ context.Context, userID string) ([]string, error) {
	if f.err != nil {
		return nil, f.err
	}
	var ids []string
	for key := range f.grants {
		user, instance, _ := strings.Cut(key, "/")
		if user == userID {
			ids = append(ids, instance)
		}
	}
	return ids, nil
}

func admin() *store.User  { return &store.User{ID: "u-admin", Role: store.RoleAdmin} }
func member() *store.User { return &store.User{ID: "u-member", Role: store.RoleMember} }

func withGrant(g *store.Grant) *Authz {
	return New(&fakeGrants{grants: map[string]*store.Grant{"u-member/inst-a": g}})
}

func TestCan(t *testing.T) {
	viewer := &store.Grant{Role: store.GrantViewer}
	operator := &store.Grant{Role: store.GrantOperator}
	curator := &store.Grant{Role: store.GrantViewer, Perms: []string{"mods.manage"}}

	tests := []struct {
		name     string
		user     *store.User
		grant    *store.Grant
		action   Action
		instance string
		want     bool
	}{
		{name: "admin does everything", user: admin(), action: InstanceDelete, instance: "inst-a", want: true},
		{name: "admin needs no grant", user: admin(), action: ConsoleRead, instance: "inst-zzz", want: true},
		{name: "admin holds global actions", user: admin(), action: UsersManage, want: true},

		{name: "member with no grant sees nothing", user: member(), action: InstanceView, instance: "inst-a"},
		{
			name: "viewer reads the console", user: member(), grant: viewer,
			action: ConsoleRead, instance: "inst-a", want: true,
		},
		{
			name: "viewer cannot start", user: member(), grant: viewer,
			action: InstanceStart, instance: "inst-a",
		},
		{
			name: "operator starts", user: member(), grant: operator,
			action: InstanceStart, instance: "inst-a", want: true,
		},
		{
			name: "operator is viewer plus", user: member(), grant: operator,
			action: ConfigRead, instance: "inst-a", want: true,
		},
		{
			name: "operator changes no content", user: member(), grant: operator,
			action: ModsManage, instance: "inst-a",
		},
		{
			name: "an extra is additive", user: member(), grant: curator,
			action: ModsManage, instance: "inst-a", want: true,
		},
		{
			name: "an extra does not widen the base role", user: member(), grant: curator,
			action: InstanceStart, instance: "inst-a",
		},
		{
			name: "a grant does not reach another instance", user: member(), grant: operator,
			action: ConsoleRead, instance: "inst-b",
		},
		{
			name: "a member holds no global action", user: member(), grant: operator,
			action: InstanceView,
		},

		{name: "nil user", action: ConsoleRead, instance: "inst-a"},
		{
			name:   "disabled user keeps nothing",
			user:   &store.User{ID: "u-admin", Role: store.RoleAdmin, Disabled: true},
			action: InstanceView, instance: "inst-a",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := withGrant(tt.grant)
			if got := a.Can(t.Context(), tt.user, tt.action, tt.instance); got != tt.want {
				t.Errorf("Can(%s, %s) = %v, want %v", tt.action, tt.instance, got, tt.want)
			}
		})
	}
}

// TestNeverGrantableCannotBeGranted is 09 §3.3. Everything that shapes container creation
// is on that list, so a grant row that names one must be ignored rather than honoured —
// otherwise a grant becomes a path to the Docker socket (D7, D15, 02 §6).
func TestNeverGrantableCannotBeGranted(t *testing.T) {
	forbidden := []Action{
		InstanceCreate, InstanceDelete, InstanceClone,
		InstanceLimits, InstanceExtraArgs, InstanceImage,
		UsersManage, InvitesManage, GrantsManage,
		SchedulesGlobal, PanelSettings, AuditRead,
	}

	names := make([]string, 0, len(forbidden))
	for _, act := range forbidden {
		names = append(names, act.String())
	}
	// A grant hand-edited to carry the whole admin-only list.
	a := withGrant(&store.Grant{Role: store.GrantOperator, Perms: names})

	for _, act := range forbidden {
		if a.Can(t.Context(), member(), act, "inst-a") {
			t.Errorf("a grant naming %s was honoured; 09 §3.3 has no per-instance override", act)
		}
	}

	allowed, err := a.Allowed(t.Context(), member(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	for _, act := range allowed {
		if neverGrantableSet[act] {
			t.Errorf("allowed_actions leaked %s", act)
		}
	}
}

// TestConfigRawImpliesConfigEdit is 09 §3.2: raw text editing is strictly the larger
// power, so holding it without the form is a distinction with no meaning.
func TestConfigRawImpliesConfigEdit(t *testing.T) {
	a := withGrant(&store.Grant{Role: store.GrantViewer, Perms: []string{"config.raw"}})
	for _, act := range []Action{ConfigRaw, ConfigEdit} {
		if !a.Can(t.Context(), member(), act, "inst-a") {
			t.Errorf("config.raw did not carry %s", act)
		}
	}
}

// TestUnknownCapabilityIsIgnoredLoudly: a capability nobody can spell is a grant that
// silently does nothing, which is the shape to report rather than swallow.
func TestUnknownCapabilityIsIgnoredLoudly(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	defer slog.SetDefault(restore)

	a := withGrant(&store.Grant{Role: store.GrantViewer, Perms: []string{"mods.manag"}})
	if a.Can(t.Context(), member(), ModsManage, "inst-a") {
		t.Error("a misspelled capability was honoured")
	}
	if !strings.Contains(buf.String(), "unknown capability") {
		t.Errorf("nothing logged about the misspelling: %s", buf.String())
	}
}

// TestLookupFailureFailsClosed: a database that cannot answer must deny, and say so.
func TestLookupFailureFailsClosed(t *testing.T) {
	a := New(&fakeGrants{err: errors.New("database is gone")})
	if a.Can(t.Context(), member(), ConsoleRead, "inst-a") {
		t.Error("a failed grant lookup allowed the request")
	}
}

// TestEveryDenialIsLoggedOnce holds 09 §4: denials are a signal, and one call produces one
// line carrying user, action and instance.
func TestEveryDenialIsLoggedOnce(t *testing.T) {
	var buf bytes.Buffer
	restore := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
	defer slog.SetDefault(restore)

	a := withGrant(&store.Grant{Role: store.GrantViewer})
	if a.Can(t.Context(), member(), InstanceStart, "inst-a") {
		t.Fatal("viewer was allowed to start")
	}

	var lines int
	var line map[string]any
	for _, raw := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		var entry map[string]any
		if err := json.Unmarshal([]byte(raw), &entry); err != nil {
			t.Fatalf("log line is not JSON: %q", raw)
		}
		if entry["msg"] == "authorization denied" {
			lines++
			line = entry
		}
	}
	if lines != 1 {
		t.Fatalf("one denial produced %d log lines, want exactly 1", lines)
	}
	for _, field := range []string{"user_id", "action", "instance_id"} {
		if v, ok := line[field]; !ok || v == "" {
			t.Errorf("denial line has no %s: %v", field, line)
		}
	}
}

// TestAllowedIsWhatTheSPARendersFrom is 09 §4.2 / F3: the payload is a list of actions, so
// the frontend never has to map a role name to a button.
func TestAllowedIsWhatTheSPARendersFrom(t *testing.T) {
	a := withGrant(&store.Grant{Role: store.GrantViewer})

	got, err := a.Allowed(t.Context(), member(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	want := []Action{BackupsList, ConfigRead, ConsoleRead, InstanceView, ModsList, StatsRead}
	if len(got) != len(want) {
		t.Fatalf("viewer allowed %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("viewer allowed %v, want %v (sorted)", got, want)
		}
	}

	everything, err := a.Allowed(t.Context(), admin(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(everything) != len(byName) {
		t.Errorf("admin allowed %d actions, want all %d", len(everything), len(byName))
	}
}

func TestVisibleInstances(t *testing.T) {
	a := New(&fakeGrants{grants: map[string]*store.Grant{
		"u-member/inst-a": {Role: store.GrantViewer},
	}})

	ids, all, err := a.VisibleInstances(t.Context(), member())
	if err != nil {
		t.Fatal(err)
	}
	if all {
		t.Error("a member was reported as seeing everything")
	}
	if len(ids) != 1 || ids[0] != "inst-a" {
		t.Errorf("member sees %v, want only inst-a", ids)
	}

	if _, all, err = a.VisibleInstances(t.Context(), admin()); err != nil || !all {
		t.Errorf("admin: all = %v, err = %v; want no filter", all, err)
	}

	ids, all, err = a.VisibleInstances(t.Context(), &store.User{ID: "u-x", Role: store.RoleMember})
	if err != nil || all || len(ids) != 0 {
		t.Errorf("a member with no grants sees %v (all=%v, err=%v), want an empty dashboard", ids, all, err)
	}
}

// TestActionNamesAreStable guards the wire contract: allowed_actions strings are what the
// SPA and the grant rows are written against, so a rename is a breaking change.
func TestActionNamesAreStable(t *testing.T) {
	for name, act := range byName {
		if act.String() != name {
			t.Errorf("action %q reports itself as %q", name, act)
		}
		raw, err := json.Marshal(act)
		if err != nil {
			t.Fatal(err)
		}
		if string(raw) != `"`+name+`"` {
			t.Errorf("action %q marshals as %s", name, raw)
		}
	}

	// 09 §3.1's two base roles, spelled out, so a silent edit to the sets is visible here.
	if len(roleActions["viewer"]) != 6 {
		t.Errorf("viewer carries %d actions, want the 6 of 09 §3.1", len(roleActions["viewer"]))
	}
	if len(roleActions["operator"]) != 13 {
		t.Errorf("operator carries %d actions, want viewer's 6 plus 7", len(roleActions["operator"]))
	}
}
