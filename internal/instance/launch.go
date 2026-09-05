package instance

// The measured launch-argument vocabulary of 03 §1.3, held here rather than in the frontend
// (F2). Every value below is measured, not inferred.

// GameBuild is the Steam build the values below were measured against.
const GameBuild = "21981559"

// Presets are the `-preset` values accepted by GameBuild, measured by feeding each candidate
// to the real parser (03 §1.3.1). The list is confirmed, not exhaustive: see PresetsComplete.
var Presets = []string{
	"normal", "casual", "easy", "hard", "hardcore", "immersive", "hammer", "default",
}

// PresetsComplete reports whether the preset list is known to be exhaustive. It is not.
const PresetsComplete = false

// ModifierKeys are the five `-modifier` axes of 03 §1.3. Their legal values are deliberately
// absent: the stored `.fwl` form is not proven identical to the command-line grammar, so the
// wizard takes a value as free text rather than offering a guessed list (E8).
var ModifierKeys = []string{"combat", "deathpenalty", "resources", "raids", "portals"}

// ModifierValuesMeasured reports whether the legal modifier values are known. They are not.
const ModifierValuesMeasured = false

// SaveDefaults are the server's own defaults with every save and backup flag omitted, all
// measured (03 §1.3.1).
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

// CrossplayUntested lists the crossplay combinations 03 §1.4 has not measured. Data rather
// than UI copy, so the panel cannot quietly stop warning about them.
var CrossplayUntested = []string{
	"crossplay together with mods",
}
