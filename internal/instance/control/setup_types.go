package control

// ManifestLaunch is the portable part of an instance definition.
type ManifestLaunch struct {
	ServerName      string            `json:"server_name"`
	WorldName       string            `json:"world_name"`
	Public          bool              `json:"public"`
	Crossplay       bool              `json:"crossplay"`
	Preset          string            `json:"preset,omitempty"`
	Modifiers       map[string]string `json:"modifiers,omitempty"`
	ExtraArgs       string            `json:"extra_args,omitempty"`
	MemLimitMB      int               `json:"mem_limit_mb"`
	CPULimit        *float64          `json:"cpu_limit"`
	BackupKeepCold  int               `json:"backup_keep_cold"`
	BackupKeepHot   int               `json:"backup_keep_hot"`
	BackupOnRestart bool              `json:"backup_on_restart"`
}

// ManifestMod is one pinned package in a portable definition. Source is the registry the files
// came from; one without it installs from whichever registry carries the version. Side is the
// admin's own classification of the package.
type ManifestMod struct {
	FullName string `json:"full_name"`
	Source   string `json:"source,omitempty"`
	Version  string `json:"version"`
	Side     string `json:"side,omitempty"`
}

// ManifestConfig is a complete managed configuration file in a portable definition.
type ManifestConfig struct {
	File    string `json:"file"`
	Content string `json:"content"`
}

// SetupMod retains the installed mod state needed to restore a saved setup.
type SetupMod struct {
	FullName     string `json:"full_name"`
	Source       string `json:"source"`
	Version      string `json:"version"`
	InstalledAs  string `json:"installed_as"`
	Side         string `json:"side"`
	Enabled      bool   `json:"enabled"`
	Locked       bool   `json:"locked"`
	FileManifest string `json:"file_manifest"`
}

// SetupSnapshot is stored as JSON with a saved setup row.
type SetupSnapshot struct {
	Instance ManifestLaunch   `json:"instance"`
	Mods     []SetupMod       `json:"mods"`
	Configs  []ManifestConfig `json:"configs"`
}
