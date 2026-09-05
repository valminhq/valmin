package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// InstanceModVersion reads the currently-installed version of fullName on instanceID. ok is
// false when the package is not installed on this instance.
func (db *DB) InstanceModVersion(
	ctx context.Context,
	instanceID, fullName string,
) (version string, ok bool, err error) {
	err = db.Reader.QueryRowContext(ctx,
		`SELECT version FROM instance_mods WHERE instance_id = ? AND full_name = ?`, instanceID, fullName,
	).Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("read instance_mods %s/%s: %w", instanceID, fullName, err)
	}
	return version, true, nil
}

// InstanceMod is one row of instance_mods (04 §2): a package installed on one instance, with
// the file manifest that makes its removal exact (ADR-009). FileManifest is the column's raw
// JSON, `[{path, sha256}]`, stored here and interpreted elsewhere.
type InstanceMod struct {
	InstanceID   string
	FullName     string
	Version      string
	InstalledAs  string
	Side         string
	Enabled      bool
	FileManifest string
	InstalledAt  string
}

// InstalledAs values (04 §2's CHECK constraint).
const (
	InstalledExplicit   = "explicit"
	InstalledDependency = "dependency"
)

// SideUnknown is a fresh install's side. Thunderstore metadata does not encode whether a mod
// is server-only or client-required (03 §5.6), so an admin sets it over PATCH.
const SideUnknown = "unknown"

const instanceModColumns = `instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at`

// InstanceMods lists what is installed on one instance, ordered by full name so a page and a
// diff of it are stable.
func (db *DB) InstanceMods(ctx context.Context, instanceID string) ([]InstanceMod, error) {
	rows, err := db.Reader.QueryContext(ctx,
		`SELECT `+instanceModColumns+` FROM instance_mods WHERE instance_id = ? ORDER BY full_name`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("list instance_mods for %s: %w", instanceID, err)
	}
	defer func() { _ = rows.Close() }()

	var out []InstanceMod
	for rows.Next() {
		var m InstanceMod
		if err := rows.Scan(&m.InstanceID, &m.FullName, &m.Version, &m.InstalledAs,
			&m.Side, &m.Enabled, &m.FileManifest, &m.InstalledAt); err != nil {
			return nil, fmt.Errorf("scan instance_mods for %s: %w", instanceID, err)
		}
		out = append(out, m)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read instance_mods for %s: %w", instanceID, err)
	}
	return out, nil
}

// TxUpsertInstanceMods writes the manifest rows for an install. It takes a transaction rather
// than opening one, so the rows land in the same flip as the job's manifest_written checkpoint
// (12 §9.4, C1).
func TxUpsertInstanceMods(ctx context.Context, tx *sql.Tx, mods []InstanceMod) error {
	for i := range mods {
		m := &mods[i]
		// A row restored after a rolled-back update keeps its original timestamp: it records
		// the install still on disk, not the attempt that failed.
		now := m.InstalledAt
		if now == "" {
			now = Now()
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO instance_mods (`+instanceModColumns+`)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (instance_id, full_name) DO UPDATE SET
				version = excluded.version,
				installed_as = excluded.installed_as,
				file_manifest = excluded.file_manifest,
				installed_at = excluded.installed_at`,
			m.InstanceID, m.FullName, m.Version, m.InstalledAs,
			m.Side, m.Enabled, m.FileManifest, now); err != nil {
			return fmt.Errorf("write instance_mods %s/%s: %w", m.InstanceID, m.FullName, err)
		}
	}
	return nil
}

// TxDeleteInstanceMods removes rows by full name, inside the job's own Finish flip alongside
// the terminal status (12 §6).
func TxDeleteInstanceMods(ctx context.Context, tx *sql.Tx, instanceID string, fullNames []string) error {
	for _, name := range fullNames {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM instance_mods WHERE instance_id = ? AND full_name = ?`, instanceID, name); err != nil {
			return fmt.Errorf("delete instance_mods %s/%s: %w", instanceID, name, err)
		}
	}
	return nil
}

// RollbackInstanceMods restores the rows an unfinished install had replaced and deletes the
// ones it had added, in one transaction. It runs after the files are undone from the manifests.
//
// Both halves in one call: an update rewrites a row in place, so undoing it means restoring the
// previous row. Deleting it instead would leave the old version's files on disk with no row
// naming them (B9).
func (db *DB) RollbackInstanceMods(
	ctx context.Context, instanceID string, restore []InstanceMod, remove []string,
) error {
	if len(restore) == 0 && len(remove) == 0 {
		return nil
	}
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin instance_mods rollback: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := TxUpsertInstanceMods(ctx, tx, restore); err != nil {
		return err
	}
	if err := TxDeleteInstanceMods(ctx, tx, instanceID, remove); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit instance_mods rollback: %w", err)
	}
	return nil
}

// SetInstanceModTags writes the admin's own labels on an installed package (04 §3). A nil
// field is left as it is; ok is false when no such mod is installed on this instance. side is
// never derived, only set by a human (03 §5.6).
func (db *DB) SetInstanceModTags(
	ctx context.Context, instanceID, fullName string, side *string, enabled *bool,
) (ok bool, err error) {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE instance_mods
		SET side = COALESCE(?, side), enabled = COALESCE(?, enabled)
		WHERE instance_id = ? AND full_name = ?`, side, enabled, instanceID, fullName)
	if err != nil {
		return false, fmt.Errorf("update instance_mods %s/%s: %w", instanceID, fullName, err)
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("update instance_mods %s/%s: %w", instanceID, fullName, err)
	}
	return rows > 0, nil
}

// TxClearModded is TxSetModded's inverse: the framework package is gone, so the instance is a
// vanilla server again. The flag gates E1's startup assertion, which would otherwise warn about
// missing plugin lines on a server that has no plugins.
func TxClearModded(ctx context.Context, tx *sql.Tx, instanceID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET modded = FALSE, bepinex_version = NULL, updated_at = ? WHERE id = ?`,
		Now(), instanceID); err != nil {
		return fmt.Errorf("clear the modded flag on %s: %w", instanceID, err)
	}
	return nil
}

// TxSetRestartRequired marks an instance whose change only takes effect at launch, cleared by
// the next successful start (ADR-012). SetRestartRequired is the same for a caller with nothing
// else to write, such as a config edit, which changes a file rather than a row.
func (db *DB) SetRestartRequired(ctx context.Context, instanceID string) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE instances SET restart_required = TRUE, updated_at = ? WHERE id = ?`,
		Now(), instanceID); err != nil {
		return fmt.Errorf("mark %s as needing a restart: %w", instanceID, err)
	}
	return nil
}

func TxSetRestartRequired(ctx context.Context, tx *sql.Tx, instanceID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET restart_required = TRUE, updated_at = ? WHERE id = ?`,
		Now(), instanceID); err != nil {
		return fmt.Errorf("set restart_required on %s: %w", instanceID, err)
	}
	return nil
}

// WriteInstanceMods records an install's manifest rows and marks the instance as needing a
// restart, in one transaction. The download, extraction and hashing that produced the rows all
// finished before this is called (12 §6).
func (db *DB) WriteInstanceMods(ctx context.Context, instanceID string, mods []InstanceMod) error {
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin instance_mods write: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := TxUpsertInstanceMods(ctx, tx, mods); err != nil {
		return err
	}
	if err := TxSetRestartRequired(ctx, tx, instanceID); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit instance_mods write: %w", err)
	}
	return nil
}

// TxSetModded records that an instance now runs BepInEx (ADR-019, 04 §2). A flag and a
// version, not a state: 12 §2's state machine is about the container.
func TxSetModded(ctx context.Context, tx *sql.Tx, instanceID, bepinexVersion string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET modded = TRUE, bepinex_version = ?, updated_at = ? WHERE id = ?`,
		bepinexVersion, Now(), instanceID); err != nil {
		return fmt.Errorf("mark %s modded: %w", instanceID, err)
	}
	return nil
}
