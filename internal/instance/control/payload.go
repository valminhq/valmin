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
	// ArchivePlan names the archive a stop or restart may write once the server is down: the
	// catalogue id it will have and the file it will be written to. It is a restart's whole
	// payload, and sits beside the reason in a stop's. Both are empty when no archive is owed.
	ArchivePlan struct {
		ArchiveID   string `json:"archive_id,omitempty"`
		ArchivePath string `json:"archive_path,omitempty"`
	}
	GameUpdatePayload struct {
		ConfirmModded bool `json:"confirm_modded"`
		// FromBuild is the build the server ran when the update was queued. It is neither
		// stored nor accepted from a request.
		FromBuild string `json:"-"`
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
		Light      bool   `json:"light,omitempty"`
		// BackUpWorld takes a world backup during the save, to the archive ArchivePlan names,
		// and links it to the setup.
		BackUpWorld bool `json:"back_up_world,omitempty"`
		// LinkNextWorld saves the setup waiting for the world backup the next mod change takes.
		LinkNextWorld bool `json:"link_next_world_backup,omitempty"`
		ArchivePlan
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
