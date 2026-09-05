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
	InstanceLimits    = Action{"instance.limits"}
	InstanceExtraArgs = Action{"instance.extra_args"}
	InstanceImage     = Action{"instance.image"}
	UsersManage       = Action{"users.manage"}
	InvitesManage     = Action{"invites.manage"}
	GrantsManage      = Action{"grants.manage"}
	SchedulesGlobal   = Action{"schedules.global"}
	PanelSettings     = Action{"panel.settings"}
	AuditRead         = Action{"audit.read"}
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
		InstanceCreate, InstanceDelete, InstanceClone,
		InstanceLimits, InstanceExtraArgs, InstanceImage,
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

// Grantable reports whether act may ever appear in a grant's perms, so a handler can reject
// the attempt at the point of request rather than dropping it later (09 §3.3).
func Grantable(act Action) bool { return !neverGrantableSet[act] }

func set(actions ...Action) map[Action]bool {
	m := make(map[Action]bool, len(actions))
	for _, a := range actions {
		m[a] = true
	}
	return m
}
