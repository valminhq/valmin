package instance

import "slices"

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

// Option is one value of a launch setting and the label a screen shows for it.
type Option struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

// Modifier is one `-modifier` axis with its values in the game's slider order. The empty
// value is Normal: the axis is left out, so the server default or the preset's value applies.
type Modifier struct {
	Key    string   `json:"key"`
	Label  string   `json:"label"`
	Values []Option `json:"values"`
}

// normal is every axis's default position.
var normal = Option{"", "Normal"}

// Modifiers are the axes and values the dedicated server manual lists and the game's
// WorldModifiers and WorldModifierOption enums contain.
var Modifiers = []Modifier{
	{Key: "combat", Label: "Combat", Values: []Option{
		{"veryeasy", "Very easy"},
		{"easy", "Easy"},
		normal,
		{"hard", "Hard"},
		{"veryhard", "Very hard"},
	}},
	{Key: "deathpenalty", Label: "Death penalty", Values: []Option{
		{"casual", "Casual"},
		{"veryeasy", "Very easy"},
		{"easy", "Easy"},
		normal,
		{"hard", "Hard"},
		{"hardcore", "Hardcore"},
	}},
	{Key: "resources", Label: "Resources", Values: []Option{
		{"muchless", "Much less"},
		{"less", "Less"},
		normal,
		{"more", "More"},
		{"muchmore", "Much more"},
		{"most", "Most"},
	}},
	{Key: "raids", Label: "Raids", Values: []Option{
		{"none", "None"},
		{"muchless", "Much less"},
		{"less", "Less"},
		normal,
		{"more", "More"},
		{"muchmore", "Much more"},
	}},
	{Key: "portals", Label: "Portals", Values: []Option{
		{"casual", "Casual"}, normal, {"hard", "Hard"}, {"veryhard", "Very hard"},
	}},
}

// WorldKeys are the world modifier checkboxes, passed as `-setkey`. They share the modifiers
// map with the axes, where a set key holds WorldKeyOn.
var WorldKeys = []Option{
	{"nobuildcost", "No build cost"},
	{"playerevents", "Player-based raids"},
	{"fire", "Fire hazard"},
	{"passivemobs", "Passive enemies"},
	{"nomap", "No map"},
}

// WorldKeyOn is the modifiers-map value of a set world key.
const WorldKeyOn = "on"

// IsWorldKey reports whether a modifiers-map key is a world key rather than an axis.
func IsWorldKey(key string) bool {
	return slices.ContainsFunc(WorldKeys, func(o Option) bool { return o.Value == key })
}

// CustomSeedMod names the package that lets a new world take a chosen seed, and the seed's limits.
type CustomSeedMod struct {
	Namespace string `json:"namespace"`
	Name      string `json:"name"`
	Source    string `json:"source"`
	MaxLength int    `json:"max_length"`
}

// FullName is the package's catalogue name.
func (m CustomSeedMod) FullName() string { return m.Namespace + "-" + m.Name }

// CustomSeed is the custom seed package. Its config file is named after the plugin, not the package.
var CustomSeed = CustomSeedMod{Namespace: "DeeJay", Name: "CustomSeed", Source: "thunderstore", MaxLength: 10}

// CustomSeedConfig is the custom seed plugin's config file.
const CustomSeedConfig = "ZeroBandwidth.CustomSeed.cfg"

// ValidSeed reports whether s is a seed the custom seed plugin accepts: 1 to MaxLength ASCII
// letters or digits.
func ValidSeed(s string) bool {
	if s == "" || len(s) > CustomSeed.MaxLength {
		return false
	}
	for _, r := range s {
		if (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9') {
			return false
		}
	}
	return true
}

// SeedConfig is the custom seed plugin's config setting seed.
func SeedConfig(seed string) string {
	return "[CustomSeed]\ncustom_seed = " + seed + "\n"
}

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
// than UI copy, so the panel cannot quietly stop warning about them. Empty since the
// modded-client join was measured (Q6, evidence/real-game-2026-09-14.md); the screens render
// nothing while it is.
var CrossplayUntested = []string{}
