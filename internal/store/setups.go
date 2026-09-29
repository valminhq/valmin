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
	ID           string    `json:"id"`
	InstanceID   string    `json:"instance_id"`
	Name         string    `json:"name"`
	CreatedBy    string    `json:"created_by,omitempty"`
	CreatedAt    time.Time `json:"created_at"`
	GameBuildID  string    `json:"game_build_id"`
	WorldName    string    `json:"world_name"`
	SnapshotJSON string    `json:"-"`
	BackupID     string    `json:"backup_id,omitempty"`
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
			snapshot_json, backup_id, created_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		setup.ID, setup.InstanceID, setup.Name, createdBy, setup.GameBuildID,
		setup.WorldName, setup.SnapshotJSON, backupID, FormatTime(setup.CreatedAt)); err != nil {
		return fmt.Errorf("insert saved setup %s: %w", setup.ID, err)
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
	world_name, snapshot_json, backup_id, created_at`

func scanSavedSetup(row scanner) (SavedSetup, error) {
	var s SavedSetup
	var createdBy, backupID sql.NullString
	var createdAt string
	if err := row.Scan(&s.ID, &s.InstanceID, &s.Name, &createdBy,
		&s.GameBuildID, &s.WorldName, &s.SnapshotJSON, &backupID, &createdAt); err != nil {
		return SavedSetup{}, fmt.Errorf("scan saved setup: %w", err)
	}
	s.CreatedBy = createdBy.String
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

// SetupByID returns a setup and its package payload references. A missing or foreign
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
	return &s, artifacts, nil
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

// ReferencedSetupArtifacts returns every digest still held by at least one setup.
// Run blob GC only while setup save jobs are idle, so a staged but unpublished blob is
// not mistaken for an orphan.
func (db *DB) ReferencedSetupArtifacts(ctx context.Context) (map[string]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT DISTINCT sha256 FROM saved_setup_artifacts`)
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

// TxApplySetupState replaces managed-mod rows and saved launch and backup settings in one
// transaction. World selection, game build, credentials, and other instance fields remain
// untouched. Files must already have been staged and verified before the transaction starts.
//
//nolint:gocritic // Keep the state-application API stable for existing job callers.
func TxApplySetupState(
	ctx context.Context, tx *sql.Tx, instanceID string,
	mods []InstanceMod, launch InstanceLaunch, backup BackupPolicy,
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
		UPDATE instances SET
			server_name = ?, public = ?, crossplay = ?, preset = ?, modifiers = ?,
			mem_limit_mb = ?, cpu_limit = ?, extra_args = ?,
			backup_keep_cold = ?, backup_keep_hot = ?, backup_on_restart = ?,
			modded = ?, bepinex_version = ?, restart_required = TRUE, updated_at = ?
		WHERE id = ? AND state = 'stopped'`,
		launch.ServerName, launch.Public, launch.Crossplay, launch.Preset,
		launch.Modifiers, launch.MemLimitMB, launch.CPULimit, launch.ExtraArgs,
		backup.KeepCold, backup.KeepHot, backup.OnRestart,
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
	return nil
}
