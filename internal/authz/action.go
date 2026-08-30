package authz

import "strconv"

// Action is what a caller wants to do. The registry below is closed: the unexported field
// means no other package can mint an Action, so an unknown action is a compile error
// rather than a silent false — or, worse, a silent true (C8, 09 §4).
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

// What operator adds to viewer (09 §3.1): run the server while I am away. It changes no
// content.
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
)

// Never grantable (09 §3.3): admin-only, globally, with no per-instance override, ever.
// Everything that shapes container creation is here, because a grant must not become a
// path to the Docker socket (D7, D15, 02 §6).
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

// viewerActions and operatorExtra are 09 §3.1 as data. roleActions composes them, so
// "operator is viewer plus" is stated once rather than copied.
var (
	viewerActions = []Action{InstanceView, ConsoleRead, StatsRead, BackupsList, ModsList, ConfigRead}
	operatorExtra = []Action{
		InstanceStart, InstanceStop, InstanceRestart,
		BackupsCreate, BackupsDownload, PlayersManage, CommandsSend,
	}
	grantableExtras = []Action{ModsManage, ConfigEdit, ConfigRaw, BackupsRestore, WorldImport}
	neverGrantable  = []Action{
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

// neverGrantableSet is consulted before any grant is read: no lookup can produce one of
// these for a member, and a perms row that names one is ignored rather than honoured.
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

// ParseAction resolves a wire name to the Action it names, for a request body that names
// capabilities by string — a grant's perms, an invite's grant_perms. An unresolved name is
// the caller's cue to answer 422 rather than store a capability nobody can spell.
func ParseAction(name string) (Action, bool) {
	a, ok := byName[name]
	return a, ok
}

// Grantable reports whether act may ever appear in a grant's perms. The never-grantable
// set of 09 §3.3 answers false, always, with no per-instance override — Can and the
// allowed_actions payload already enforce this; this export lets a handler reject the
// attempt at the point of request instead of silently dropping it later.
func Grantable(act Action) bool { return !neverGrantableSet[act] }

func set(actions ...Action) map[Action]bool {
	m := make(map[Action]bool, len(actions))
	for _, a := range actions {
		m[a] = true
	}
	return m
}
