package instance

// The measured launch-argument vocabulary of 03 §1.3, in one place, so the frontend does
// not have to hold any of it (F2, 02 §2.1). Everything here is a measurement with a build
// stamp behind it, not a plausible list.

// GameBuild is the Steam build the values below were measured against. It travels with them
// so an operator reading a stale panel can tell.
const GameBuild = "21981559"

// Presets are the `-preset` values accepted by build 21981559, measured 31 Aug 2026 by
// feeding each candidate to the real parser (03 §1.3.1). Nine other plausible names were
// rejected.
//
// `↯` PresetsComplete is false and must stay false until someone proves otherwise. Black-box
// probing confirms the values it is given; it cannot enumerate the ones nobody thought to
// try. Two methods that look authoritative were tried and both were falsified — `strings`
// over assembly_valheim.dll finds only three of these eight, so the DLL is not where the
// names live and any future enumeration read out of it is wrong by construction.
var Presets = []string{
	"normal", "casual", "easy", "hard", "hardcore", "immersive", "hammer", "default",
}

// PresetsComplete reports whether the preset list is known to be exhaustive. It is not.
const PresetsComplete = false

// ModifierKeys are the five `-modifier` axes of 03 §1.3, introduced with Hildir's Request.
//
// `↯` Their legal *values* are deliberately absent. 03 §4.2 measured a `.fwl` storing
// `combat_default:deathpenalty_default:...`, and says in the same breath that the stored
// form is not proven identical to the command-line grammar — so a value list built from it
// would be inference presented as measurement (E8). The wizard takes a value as free text
// and says so; a guessed dropdown would be the failure this project designs against.
var ModifierKeys = []string{"combat", "deathpenalty", "resources", "raids", "portals"}

// ModifierValuesMeasured reports whether the legal modifier values are known. They are not.
const ModifierValuesMeasured = false

// SaveDefaults are the server's own defaults with every save and backup flag omitted,
// measured 31 Aug 2026 (03 §1.3.1). The backup three print themselves in a startup line
// whose values were proved effective by a control run; the save interval was measured as
// the gap between two consecutive autosaves, because the *first* save a server writes may
// be its shutdown save and is not an autosave at all.
type SaveDefaults struct {
	SaveIntervalSeconds int `json:"save_interval_seconds"`
	Backups             int `json:"backups"`
	BackupShortSeconds  int `json:"backup_short_seconds"`
	BackupLongSeconds   int `json:"backup_long_seconds"`
}

// Saves is 03 §1.3's measured defaults.
var Saves = SaveDefaults{
	SaveIntervalSeconds: 1800,
	Backups:             4,
	BackupShortSeconds:  7200,
	BackupLongSeconds:   43200,
}

// CrossplayUntested is 03 §1.4 rule 5, verbatim in substance: the two combinations nobody
// has run. `↯` It is data rather than UI copy so the panel cannot quietly stop saying it —
// Q6 no longer blocks M1, but it does block *advertising crossplay as supported*.
var CrossplayUntested = []string{
	"crossplay together with more than one instance on this host",
	"crossplay together with mods",
}
