package manager

// PackageRequest is a package selection passed to domain mod operations.
type PackageRequest struct {
	FullName string `json:"full_name"`
	Version  string `json:"version"`
	Source   string `json:"source"`
}

// UpdateTarget records the version confirmed for a mod update.
type UpdateTarget struct {
	FullName    string `json:"full_name"`
	Source      string `json:"source"`
	FromVersion string `json:"from_version,omitempty"`
	Version     string `json:"version"`
}

// InstallPayload is the persisted argument of a mod installation job.
type InstallPayload struct {
	StagingDir string         `json:"staging_dir"`
	FullName   string         `json:"full_name"`
	Version    string         `json:"version"`
	Source     string         `json:"source"`
	Updates    []UpdateTarget `json:"updates,omitempty"`
	Backup     bool           `json:"backup,omitempty"`
	Minimum    bool           `json:"minimum,omitempty"`
}

// UninstallPayload records the complete authorized removal set.
type UninstallPayload struct {
	StagingDir string   `json:"staging_dir"`
	FullNames  []string `json:"full_names"`
}

// TogglePayload records the requested package state.
type TogglePayload struct {
	FullName string `json:"full_name"`
	Enable   bool   `json:"enable"`
}
