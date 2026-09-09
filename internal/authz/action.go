package authz

import "strconv"

// Action is what a caller wants to do. The registry below is closed: the unexported field
// means no other package can mint an Action, so an unknown action is a compile error
// (C8, 09 §4).
type Action struct{ name string }

// String returns the wire form, which is what allowed_actions carries (09 §4.2).
func (a Action) String() string { return a.name }

// MarshalJSON renders the action as its wire name.
func (a Action) MarshalJSON() ([]byte, error) { return []byte(strconv.Quote(a.name)), nil }

// The viewer base role (09 §3.1).
var (
	InstanceView = Action{"instance.view"}
	ConsoleRead  = Action{"console.read"}
	StatsRead    = Action{"stats.read"}
	BackupsList  = Action{"backups.list"}
	ModsList     = Action{"mods.list"}
	ConfigRead   = Action{"config.read"}
)

// What operator adds to viewer (09 §3.1): running the server, changing no content.
var (
	InstanceStart   = Action{"instance.start"}
	InstanceStop    = Action{"instance.stop"}
	InstanceRestart = Action{"instance.restart"}
	BackupsCreate   = Action{"backups.create"}
	BackupsDownload = Action{"backups.download"}
	PlayersManage   = Action{"players.manage"}
	CommandsSend    = Action{"commands.send"}
)

// Grantable extras (09 §3.2). Off by default; an admin toggles each on a grant.
var (
	ModsManage     = Action{"mods.manage"}
	ConfigEdit     = Action{"config.edit"}
	ConfigRaw      = Action{"config.raw"}
	BackupsRestore = Action{"backups.restore"}
	WorldImport    = Action{"world.import"}
	// InstanceSettings covers the launch fields that describe the server rather than shape
	// its container: name, password, discovery and world rules. Grantable because none of it
	// reaches the Docker socket (D15) and its argv is within D8's typed allowlist.
	InstanceSettings = Action{"instance.settings"}
)

// Never grantable (09 §3.3): admin-only globally, with no per-instance override. Everything
// that shapes container creation is here, so no grant becomes a path to the Docker socket
// (D7, D15).
var (
	InstanceCreate    = Action{"instance.create"}
	InstanceDelete    = Action{"instance.delete"}
	InstanceClone     = Action{"instance.clone"}
	InstanceAdopt     = Action{"instance.adopt"}
	InstanceLimits    = Action{"instance.limits"}
	InstanceExtraArgs = Action{"instance.extra_args"}
	InstanceImage     = Action{"instance.image"}
	// InstanceUpdate is the game update. Admin-only rather than grantable because it replaces
	// every byte under server/ from an upstream download and can leave a modded instance
	// unable to load its plugins (ADR-137, 03 §8).
	InstanceUpdate  = Action{"instance.update"}
	UsersManage     = Action{"users.manage"}
	InvitesManage   = Action{"invites.manage"}
	GrantsManage    = Action{"grants.manage"}
	SchedulesGlobal = Action{"schedules.global"}
	PanelSettings   = Action{"panel.settings"}
	AuditRead       = Action{"audit.read"}
)

// 09 §3's role sets as data. roleActions composes the first two, so "operator is viewer
// plus" is stated once.
var (
	viewerActions = []Action{InstanceView, ConsoleRead, StatsRead, BackupsList, ModsList, ConfigRead}
	operatorExtra = []Action{
		InstanceStart, InstanceStop, InstanceRestart,
		BackupsCreate, BackupsDownload, PlayersManage, CommandsSend,
	}
	grantableExtras = []Action{
		ModsManage, ConfigEdit, ConfigRaw, BackupsRestore, WorldImport, InstanceSettings,
	}
	neverGrantable = []Action{
		InstanceCreate, InstanceDelete, InstanceClone, InstanceAdopt,
		InstanceLimits, InstanceExtraArgs, InstanceImage, InstanceUpdate,
		UsersManage, InvitesManage, GrantsManage,
		SchedulesGlobal, PanelSettings, AuditRead,
	}
)

// roleActions is the base set each grant role carries.
var roleActions = map[string]map[Action]bool{
	"viewer":   set(viewerActions...),
	"operator": set(append(append([]Action{}, viewerActions...), operatorExtra...)...),
}

// neverGrantableSet is consulted before any grant is read; a perms row naming one of these
// is ignored rather than honoured.
var neverGrantableSet = set(neverGrantable...)

// byName resolves a perms entry to an Action. A grant stores capability names as JSON
// strings, so this is the only place a string becomes an Action.
var byName = func() map[string]Action {
	all := append([]Action{}, viewerActions...)
	all = append(all, operatorExtra...)
	all = append(all, grantableExtras...)
	all = append(all, neverGrantable...)

	m := make(map[string]Action, len(all))
	for _, a := range all {
		m[a.name] = a
	}
	return m
}()

// ParseAction resolves a wire name to its Action, for request bodies that name capabilities
// by string. An unresolved name is the caller's cue to answer 422.
func ParseAction(name string) (Action, bool) {
	a, ok := byName[name]
	return a, ok
}

// Grantable reports whether act may appear in a grant's perms. Base-role actions are harmless
// redundancies; never-grantable actions are rejected.
func Grantable(act Action) bool { return !neverGrantableSet[act] }

// GrantRoleOption is one daemon-owned base-role projection for grant editors.
type GrantRoleOption struct {
	Role           string   `json:"role"`
	AllowedActions []Action `json:"allowed_actions"`
}

// GrantExtraOption describes one additive capability and the risk an admin accepts. Risk
// is operator-facing copy, and 09 §3.2 requires the UI to state the consequence in these
// terms — including config.raw's implication, which Can() applies whether or not the admin
// ticked config.edit as well.
type GrantExtraOption struct {
	Action Action `json:"action"`
	Risk   string `json:"risk"`
}

// GrantRoleOptions returns the complete base-role vocabulary. The frontend does not derive
// permissions from role names.
func GrantRoleOptions() []GrantRoleOption {
	return []GrantRoleOption{
		{Role: "viewer", AllowedActions: sorted(roleActions["viewer"])},
		{Role: "operator", AllowedActions: sorted(roleActions["operator"])},
	}
}

// GrantExtraOptions returns the complete additive vocabulary with operator-facing risk copy.
func GrantExtraOptions() []GrantExtraOption {
	return []GrantExtraOption{
		{ModsManage, "Can install, update, and extract arbitrary third-party archives."},
		{ConfigEdit, "Can change mod settings through validated forms."},
		{ConfigRaw, "Can write arbitrary bytes to mod configuration files, and everything " +
			"config.edit allows."},
		{BackupsRestore, "Can replace the live world and permanently delete backup archives."},
		{WorldImport, "Can upload and replace the live world."},
		{InstanceSettings, "Can change the server name, password, discovery, crossplay, and world rules."},
	}
}

func set(actions ...Action) map[Action]bool {
	m := make(map[Action]bool, len(actions))
	for _, a := range actions {
		m[a] = true
	}
	return m
}
