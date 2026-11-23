package store

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SavedSetup records one immutable server setup. SnapshotJSON carries the settings and
// managed-mod state; package payloads are referenced separately by their SHA-256 digests.
type SavedSetup struct {
	ID         string `json:"id"`
	InstanceID string `json:"instance_id"`
	Name       string `json:"name"`
	CreatedBy  string `json:"created_by,omitempty"`
	// CreatedByName is the username of CreatedBy, read back and never written.
	CreatedByName string    `json:"-"`
	CreatedAt     time.Time `json:"created_at"`
	GameBuildID   string    `json:"game_build_id"`
	WorldName     string    `json:"world_name"`
	SnapshotJSON  string    `json:"-"`
	BackupID      string    `json:"backup_id,omitempty"`
	// Automatic marks a setup the panel saved before a restore or a mod update.
	Automatic bool `json:"automatic"`
	// ConfigsTruncated reports that the config tree held files past the listing limits.
	ConfigsTruncated bool `json:"configs_truncated"`
	// WorldPending marks a setup waiting to be linked to the world backup the next mod change
	// takes, if its mods are still the server's then.
	WorldPending bool `json:"world_pending"`
	// Configs are the config files the setup holds. SetupByID and TxSaveSetup fill and read
	// them; ListSetups leaves them empty.
	Configs []SetupConfigRef `json:"-"`
}

// SetupConfigRef names one config file of a setup, by path under BepInEx/config, and the blob
// holding its bytes.
type SetupConfigRef struct {
	File      string `json:"file"`
	SHA256    string `json:"sha256"`
	SizeBytes int64  `json:"size_bytes"`
}

// SetupArtifactRef ties an installed package to a verified content-addressed payload.
// Kind is "zip" for a registry archive and "files" for a snapshot of installed files.
type SetupArtifactRef struct {
	FullName string `json:"full_name"`
	Source   string `json:"source"`
	Version  string `json:"version"`
	Kind     string `json:"kind"`
	SHA256   string `json:"sha256"`
}

var (
	ErrInvalidSetupBackup = errors.New("setup backup must be a consistent backup of this instance")
	ErrSetupNotFound      = errors.New("saved setup not found")
)

// SaveSetup publishes a setup and all its artifact references in one transaction. Callers
// must store and verify every payload before calling it.
func (db *DB) SaveSetup(ctx context.Context, setup *SavedSetup, artifacts []SetupArtifactRef) error {
	return db.inTx(ctx, "save setup", func(tx *sql.Tx) error {
		return TxSaveSetup(ctx, tx, setup, artifacts)
	})
}

// TxSaveSetup allows a job to publish its setup with its terminal state.
func TxSaveSetup(ctx context.Context, tx *sql.Tx, setup *SavedSetup, artifacts []SetupArtifactRef) error {
	if err := validateSavedSetup(setup, artifacts); err != nil {
		return err
	}
	if err := checkSetupBackup(ctx, tx, setup); err != nil {
		return err
	}
	if setup.CreatedAt.IsZero() {
		setup.CreatedAt = time.Now().UTC()
	}
	var backupID, createdBy any
	if setup.BackupID != "" {
		backupID = setup.BackupID
	}
	if setup.CreatedBy != "" {
		createdBy = setup.CreatedBy
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO saved_setups (
			id, instance_id, name, created_by, game_build_id, world_name,
			snapshot_json, backup_id, created_at, automatic, configs_truncated, world_pending
		) VALUES (?, ?, ?, (SELECT id FROM users WHERE id = ?), ?, ?, ?, ?, ?, ?, ?, ?)`,
		setup.ID, setup.InstanceID, setup.Name, createdBy, setup.GameBuildID,
		setup.WorldName, setup.SnapshotJSON, backupID, FormatTime(setup.CreatedAt),
		setup.Automatic, setup.ConfigsTruncated, setup.WorldPending); err != nil {
		return fmt.Errorf("insert saved setup %s: %w", setup.ID, err)
	}
	for _, c := range setup.Configs {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO saved_setup_configs (setup_id, file, sha256, size_bytes) VALUES (?, ?, ?, ?)`,
			setup.ID, c.File, c.SHA256, c.SizeBytes); err != nil {
			return fmt.Errorf("insert setup config %s: %w", c.File, err)
		}
	}
	for _, a := range artifacts {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO saved_setup_artifacts (
				setup_id, full_name, source, version, kind, sha256
			) VALUES (?, ?, ?, ?, ?, ?)`,
			setup.ID, a.FullName, a.Source, a.Version, a.Kind, a.SHA256); err != nil {
			return fmt.Errorf("insert setup artifact %s: %w", a.FullName, err)
		}
	}
	return nil
}

func validateSavedSetup(setup *SavedSetup, artifacts []SetupArtifactRef) error {
	if setup == nil || setup.ID == "" || setup.InstanceID == "" || strings.TrimSpace(setup.Name) == "" ||
		!json.Valid([]byte(setup.SnapshotJSON)) {
		return errors.New("invalid saved setup")
	}
	for _, a := range artifacts {
		if a.FullName == "" || a.Source == "" || a.Version == "" ||
			(a.Kind != "zip" && a.Kind != "files") || !validDigest(a.SHA256) {
			return fmt.Errorf("invalid setup artifact for %q", a.FullName)
		}
	}
	return validateSetupConfigs(setup.Configs)
}

// validateSetupConfigs checks each config file reference of a setup has a path, a size and a
// digest.
func validateSetupConfigs(configs []SetupConfigRef) error {
	for _, c := range configs {
		if c.File == "" || c.SizeBytes < 0 || !validDigest(c.SHA256) {
			return fmt.Errorf("invalid setup config %q", c.File)
		}
	}
	return nil
}

func checkSetupBackup(ctx context.Context, tx *sql.Tx, setup *SavedSetup) error {
	if setup.BackupID == "" {
		return nil
	}
	var consistent bool
	err := tx.QueryRowContext(ctx,
		`SELECT consistent FROM backups WHERE id = ? AND instance_id = ?`,
		setup.BackupID, setup.InstanceID).Scan(&consistent)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && !consistent) {
		return ErrInvalidSetupBackup
	}
	if err != nil {
		return fmt.Errorf("check setup backup %s: %w", setup.BackupID, err)
	}
	return nil
}

const savedSetupColumns = `id, instance_id, name, created_by, game_build_id,
	world_name, snapshot_json, backup_id, created_at, automatic, configs_truncated, world_pending,
	(SELECT username FROM users WHERE users.id = saved_setups.created_by)`

func scanSavedSetup(row scanner) (SavedSetup, error) {
	var s SavedSetup
	var createdBy, backupID, createdByName sql.NullString
	var createdAt string
	if err := row.Scan(&s.ID, &s.InstanceID, &s.Name, &createdBy,
		&s.GameBuildID, &s.WorldName, &s.SnapshotJSON, &backupID, &createdAt,
		&s.Automatic, &s.ConfigsTruncated, &s.WorldPending, &createdByName); err != nil {
		return SavedSetup{}, fmt.Errorf("scan saved setup: %w", err)
	}
	s.CreatedBy, s.CreatedByName = createdBy.String, createdByName.String
	s.BackupID = backupID.String
	var err error
	s.CreatedAt, err = ParseTime(createdAt)
	if err != nil {
		return SavedSetup{}, fmt.Errorf("saved setup created_at: %w", err)
	}
	return s, nil
}

// ListSetups returns an instance's setups newest first.
func (db *DB) ListSetups(ctx context.Context, instanceID string) ([]SavedSetup, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+savedSetupColumns+`
		FROM saved_setups WHERE instance_id = ? ORDER BY created_at DESC, id DESC`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list saved setups for %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()
	setups := []SavedSetup{}
	for rows.Next() {
		s, err := scanSavedSetup(rows)
		if err != nil {
			return nil, fmt.Errorf("scan saved setup: %w", err)
		}
		setups = append(setups, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list saved setups for %s: %w", instanceID, err)
	}
	return setups, nil
}

// SetupByID returns a setup, with its config files, and its package payload references. A missing or foreign
// instance setup returns nil without exposing whether that ID exists elsewhere.
func (db *DB) SetupByID(ctx context.Context, instanceID, id string) (*SavedSetup, []SetupArtifactRef, error) {
	row := db.Reader.QueryRowContext(ctx, `SELECT `+savedSetupColumns+`
		FROM saved_setups WHERE instance_id = ? AND id = ?`, instanceID, id)
	s, err := scanSavedSetup(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("read saved setup %s: %w", id, err)
	}
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT full_name, source, version, kind, sha256
		FROM saved_setup_artifacts WHERE setup_id = ? ORDER BY full_name`, id)
	if err != nil {
		return nil, nil, fmt.Errorf("read saved setup artifacts %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	artifacts := []SetupArtifactRef{}
	for rows.Next() {
		var a SetupArtifactRef
		if err := rows.Scan(&a.FullName, &a.Source, &a.Version, &a.Kind, &a.SHA256); err != nil {
			return nil, nil, fmt.Errorf("scan saved setup artifact: %w", err)
		}
		artifacts = append(artifacts, a)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("read saved setup artifacts %s: %w", id, err)
	}
	s.Configs, err = db.setupConfigs(ctx, id)
	if err != nil {
		return nil, nil, err
	}
	return &s, artifacts, nil
}

// PendingSetups returns the instance's setups waiting for a world backup, newest first.
func (db *DB) PendingSetups(ctx context.Context, instanceID string) ([]SavedSetup, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+savedSetupColumns+`
		FROM saved_setups WHERE instance_id = ? AND world_pending
		ORDER BY created_at DESC, id DESC`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list setups of %s waiting for a world backup: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()
	var setups []SavedSetup
	for rows.Next() {
		s, err := scanSavedSetup(rows)
		if err != nil {
			return nil, err
		}
		setups = append(setups, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list setups of %s waiting for a world backup: %w", instanceID, err)
	}
	return setups, nil
}

// TxLinkPendingSetups links backupID to the setups of the instance named in ids, which were
// waiting for a world backup when it was taken. A setup waits only while the instance's mods are
// the ones it holds: any change to them ends the wait.
func TxLinkPendingSetups(ctx context.Context, tx *sql.Tx, instanceID, backupID string, ids []string) error {
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE saved_setups SET backup_id = ?, world_pending = FALSE
			WHERE instance_id = ? AND id = ? AND backup_id IS NULL`, backupID, instanceID, id); err != nil {
			return fmt.Errorf("link world backup %s to setup %s: %w", backupID, id, err)
		}
	}
	return nil
}

// TxKeepingSetupsWaiting runs fn, which rewrites the instance's mod rows without changing them,
// and leaves the setups that were waiting for a world backup waiting.
func TxKeepingSetupsWaiting(ctx context.Context, tx *sql.Tx, instanceID string, fn func() error) error {
	ids, err := txPendingSetupIDs(ctx, tx, instanceID)
	if err != nil {
		return err
	}
	if err := fn(); err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `UPDATE saved_setups SET world_pending = TRUE
			WHERE instance_id = ? AND id = ?`, instanceID, id); err != nil {
			return fmt.Errorf("keep setup %s waiting for a world backup: %w", id, err)
		}
	}
	return nil
}

// txPendingSetupIDs returns the IDs of the instance's setups waiting for a world backup.
func txPendingSetupIDs(ctx context.Context, tx *sql.Tx, instanceID string) ([]string, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT id FROM saved_setups WHERE instance_id = ? AND world_pending`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list setups of %s waiting for a world backup: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan a setup waiting for a world backup: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list setups of %s waiting for a world backup: %w", instanceID, err)
	}
	return ids, nil
}

// NewestSetup returns the instance's most recently saved setup as SetupByID does, nil when it
// has none.
func (db *DB) NewestSetup(ctx context.Context, instanceID string) (*SavedSetup, []SetupArtifactRef, error) {
	var id string
	err := db.Reader.QueryRowContext(ctx, `SELECT id FROM saved_setups WHERE instance_id = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, instanceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, fmt.Errorf("find the newest setup of %s: %w", instanceID, err)
	}
	return db.SetupByID(ctx, instanceID, id)
}

// setupConfigs returns the config files of setup id, by path.
func (db *DB) setupConfigs(ctx context.Context, id string) ([]SetupConfigRef, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT file, sha256, size_bytes FROM saved_setup_configs WHERE setup_id = ? ORDER BY file`, id)
	if err != nil {
		return nil, fmt.Errorf("read saved setup configs %s: %w", id, err)
	}
	defer func() { _ = rows.Close() }()
	configs := []SetupConfigRef{}
	for rows.Next() {
		var c SetupConfigRef
		if err := rows.Scan(&c.File, &c.SHA256, &c.SizeBytes); err != nil {
			return nil, fmt.Errorf("scan saved setup config: %w", err)
		}
		configs = append(configs, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read saved setup configs %s: %w", id, err)
	}
	return configs, nil
}

// TxPruneAutomaticSetups deletes an instance's automatic setups beyond its newest keep.Versions
// and releases the world backup of each linked one beyond the newest keep.Worlds, back to normal
// retention, sparing the setup with ID spare. Their blobs stay until the blob store's GC finds
// them unreferenced.
func TxPruneAutomaticSetups(
	ctx context.Context,
	ex execer,
	instanceID string,
	keep SetupRetention,
	spare string,
) error {
	if keep.Versions < 1 || keep.Worlds < 0 {
		return fmt.Errorf("prune automatic setups of %s: invalid retention %+v", instanceID, keep)
	}
	if _, err := ex.ExecContext(ctx, `
		DELETE FROM saved_setups WHERE instance_id = ? AND automatic AND id <> ? AND id NOT IN (
			SELECT id FROM saved_setups WHERE instance_id = ? AND automatic
			ORDER BY created_at DESC, id DESC LIMIT ?)`,
		instanceID, spare, instanceID, keep.Versions); err != nil {
		return fmt.Errorf("prune automatic setups of %s: %w", instanceID, err)
	}
	if _, err := ex.ExecContext(ctx, `
		UPDATE saved_setups SET backup_id = NULL
		WHERE instance_id = ? AND automatic AND backup_id IS NOT NULL AND id <> ? AND id NOT IN (
			SELECT id FROM saved_setups WHERE instance_id = ? AND automatic AND backup_id IS NOT NULL
			ORDER BY created_at DESC, id DESC LIMIT ?)`,
		instanceID, spare, instanceID, keep.Worlds); err != nil {
		return fmt.Errorf("release world backups of automatic setups of %s: %w", instanceID, err)
	}
	return nil
}

// DeleteSetup releases a setup's artifact and backup references. It does not remove blobs;
// use ReferencedSetupArtifacts and the blob store's GC after setup operations are idle.
func (db *DB) DeleteSetup(ctx context.Context, instanceID, id string) error {
	return db.inTx(ctx, "delete setup", func(tx *sql.Tx) error {
		return TxDeleteSetup(ctx, tx, instanceID, id)
	})
}

// TxDeleteSetup removes a setup inside a job's terminal transaction.
func TxDeleteSetup(ctx context.Context, tx *sql.Tx, instanceID, id string) error {
	res, err := tx.ExecContext(ctx,
		`DELETE FROM saved_setups WHERE instance_id = ? AND id = ?`, instanceID, id)
	if err != nil {
		return fmt.Errorf("delete saved setup %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count deleted saved setups %s: %w", id, err)
	}
	if n == 0 {
		return ErrSetupNotFound
	}
	return nil
}

// ErrBackupPinned is a backup a saved setup still links to.
var ErrBackupPinned = errors.New("backup is linked to a saved setup")

// DeleteBackupUnpinned removes one backup's catalogue row and records audit, in one
// transaction that first checks no saved setup links to it. The archive file is the caller's
// to remove once this returns, outside the transaction.
func (db *DB) DeleteBackupUnpinned(ctx context.Context, instanceID, backupID string, audit *AuditEntry) error {
	return db.inTx(ctx, "delete backup", func(tx *sql.Tx) error {
		var pinned bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM saved_setups WHERE instance_id = ? AND backup_id = ?)`,
			instanceID, backupID).Scan(&pinned); err != nil {
			return fmt.Errorf("check backup %s setup links: %w", backupID, err)
		}
		if pinned {
			return ErrBackupPinned
		}
		if err := TxDeleteBackup(ctx, tx, instanceID, backupID); err != nil {
			return err
		}
		return writeAuditLog(ctx, tx, audit, time.Now().UTC())
	})
}

// SetupLink names a saved setup that links a backup.
type SetupLink struct {
	ID   string
	Name string
}

// BackupSetupLinks maps each backup of an instance that saved setups link to those setups,
// oldest first.
func (db *DB) BackupSetupLinks(ctx context.Context, instanceID string) (map[string][]SetupLink, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT backup_id, id, name FROM saved_setups
		WHERE instance_id = ? AND backup_id IS NOT NULL ORDER BY created_at, id`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list setup links of %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()
	links := map[string][]SetupLink{}
	for rows.Next() {
		var backupID string
		var link SetupLink
		if err := rows.Scan(&backupID, &link.ID, &link.Name); err != nil {
			return nil, fmt.Errorf("scan setup link: %w", err)
		}
		links[backupID] = append(links[backupID], link)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list setup links of %s: %w", instanceID, err)
	}
	return links, nil
}

// BackupPinned reports whether any saved setup references an instance's backup.
func (db *DB) BackupPinned(ctx context.Context, instanceID, backupID string) (bool, error) {
	var pinned bool
	err := db.Reader.QueryRowContext(ctx, `SELECT EXISTS (
		SELECT 1 FROM saved_setups WHERE instance_id = ? AND backup_id = ?)`,
		instanceID, backupID).Scan(&pinned)
	if err != nil {
		return false, fmt.Errorf("check backup %s setup links: %w", backupID, err)
	}
	return pinned, nil
}

// ReferencedSetupArtifacts returns every package and config digest still held by at least one
// setup.
// Run blob GC only while setup save jobs are idle, so a staged but unpublished blob is
// not mistaken for an orphan.
func (db *DB) ReferencedSetupArtifacts(ctx context.Context) (map[string]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT sha256 FROM saved_setup_artifacts UNION SELECT sha256 FROM saved_setup_configs`)
	if err != nil {
		return nil, fmt.Errorf("list referenced setup artifacts: %w", err)
	}
	defer func() { _ = rows.Close() }()
	referenced := map[string]bool{}
	for rows.Next() {
		var digest string
		if err := rows.Scan(&digest); err != nil {
			return nil, fmt.Errorf("scan setup artifact digest: %w", err)
		}
		referenced[digest] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list referenced setup artifacts: %w", err)
	}
	return referenced, nil
}

func validDigest(digest string) bool {
	if len(digest) != 64 {
		return false
	}
	_, err := hex.DecodeString(digest)
	return err == nil && strings.ToLower(digest) == digest
}

// SetupSettings are the launch and backup settings a setup restore writes.
type SetupSettings struct {
	Launch InstanceLaunch
	Backup BackupPolicy
}

// TxApplySetupState replaces managed-mod rows and, unless settings is nil, the launch and backup
// settings in one transaction. World selection, game build, credentials, and other instance
// fields remain untouched. Files must already have been staged and verified before the
// transaction starts.
func TxApplySetupState(
	ctx context.Context, tx *sql.Tx, instanceID string, mods []InstanceMod, settings *SetupSettings,
) error {
	if instanceID == "" {
		return errors.New("empty setup instance id")
	}
	for i := range mods {
		mod := &mods[i]
		if mod.InstanceID != instanceID {
			return fmt.Errorf("setup mod %s belongs to another instance", mod.FullName)
		}
	}
	var state string
	if err := tx.QueryRowContext(ctx, "SELECT state FROM instances WHERE id = ?", instanceID).Scan(&state); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return ErrInstanceNotFound
		}
		return fmt.Errorf("read instance state for setup restore: %w", err)
	}
	if state != "stopped" {
		return ErrInstanceNotStopped
	}
	if _, err := tx.ExecContext(ctx, "DELETE FROM instance_mods WHERE instance_id = ?", instanceID); err != nil {
		return fmt.Errorf("clear instance mods for setup restore: %w", err)
	}
	if err := TxUpsertInstanceMods(ctx, tx, mods); err != nil {
		return err
	}
	var bepinexVersion any
	for i := range mods {
		mod := &mods[i]
		if mod.FullName == "denikson-BepInExPack_Valheim" {
			bepinexVersion = mod.Version
			break
		}
	}
	res, err := tx.ExecContext(ctx, `
		UPDATE instances SET modded = ?, bepinex_version = ?, pending_restart = TRUE, updated_at = ?
		WHERE id = ? AND state = 'stopped'`,
		bepinexVersion != nil, bepinexVersion, Now(), instanceID)
	if err != nil {
		return fmt.Errorf("apply saved setup to instance %s: %w", instanceID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("count updated instance for setup restore: %w", err)
	}
	if n == 0 {
		return ErrInstanceNotStopped
	}
	return txApplySetupSettings(ctx, tx, instanceID, settings)
}

// txApplySetupSettings writes a restore's launch and backup settings to the stopped instance,
// nothing when settings is nil.
func txApplySetupSettings(ctx context.Context, tx *sql.Tx, instanceID string, settings *SetupSettings) error {
	if settings == nil {
		return nil
	}
	launch, backup := &settings.Launch, &settings.Backup
	if _, err := tx.ExecContext(ctx, `
		UPDATE instances SET
			server_name = ?, public = ?, crossplay = ?, preset = ?, modifiers = ?,
			mem_limit_mb = ?, cpu_limit = ?, extra_args = ?,
			backup_keep_cold = ?, backup_keep_hot = ?, backup_on_restart = ?
		WHERE id = ? AND state = 'stopped'`,
		launch.ServerName, launch.Public, launch.Crossplay, launch.Preset,
		launch.Modifiers, launch.MemLimitMB, launch.CPULimit, launch.ExtraArgs,
		backup.KeepCold, backup.KeepHot, backup.OnRestart, instanceID); err != nil {
		return fmt.Errorf("apply saved settings to instance %s: %w", instanceID, err)
	}
	return nil
}
