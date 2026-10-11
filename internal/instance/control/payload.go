package control

// These payloads are stored in job rows. Their JSON names are part of the recovery contract.
type (
	ProvisionPayload struct {
		StartAfterProvision bool `json:"start_after_provision"`
	}
	ClonePayload struct {
		SourceID    string `json:"source_instance_id"`
		ArchiveID   string `json:"archive_id"`
		ArchivePath string `json:"archive_path"`
	}
	BackupMode    string
	BackupPayload struct {
		Mode BackupMode `json:"mode"`
		Dest string     `json:"dest"`
	}
	RestorePayload struct {
		BackupID string `json:"backup_id"`
	}
	GameUpdatePayload struct {
		ConfirmModded bool `json:"confirm_modded"`
	}
	DeletePayload struct {
		KeepWorlds bool `json:"keep_worlds"`
	}
	WorldImportPayload struct {
		StagingDir         string `json:"staging_dir"`
		AllowBackupVariant bool   `json:"allow_backup_variant"`
	}
	WorldDeletePayload struct {
		World string `json:"world"`
	}
	SetupJobPayload struct {
		SetupID    string `json:"setup_id"`
		StagingDir string `json:"staging_dir,omitempty"`
		ETag       string `json:"etag,omitempty"`
		Name       string `json:"name,omitempty"`
		BackupID   string `json:"world_backup_id,omitempty"`
	}
	ConfigApplyPayload struct {
		Files int `json:"files"`
	}
	AdoptionPayload struct {
		ContainerID string `json:"container_id"`
	}
)

// Backup modes. A quiesced backup stops a running server first; a hot backup copies it live.
const (
	BackupQuiesced BackupMode = "quiesced"
	BackupHot      BackupMode = "hot"
)
