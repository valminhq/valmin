package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// InstanceModVersion reads the currently-installed version of fullName on instanceID, for
// the resolver's already-installed reconciliation (WP-M2-05, `05` M2). ok is false when
// the package is not installed on this instance at all.
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

// InstanceMod is one row of instance_mods (04 §2) — a package installed on one instance,
// with the file manifest that makes its removal exact (ADR-009). FileManifest is the
// column's raw JSON, `[{path, sha256}]`; this package stores it and never interprets it,
// the same division of labour ModVersion.DependenciesJSON uses.
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

// SideUnknown is a fresh install's side. 03 §5.6 is explicit that Thunderstore metadata
// does not encode whether a mod is server-only or client-required, so the panel never
// infers it — an admin sets it (WP-M2-09's PATCH).
const SideUnknown = "unknown"

const instanceModColumns = `instance_id, full_name, version, installed_as, side, enabled, file_manifest, installed_at`

// InstanceMods lists what is installed on one instance, ordered by full name so a page and
// a diff of it are stable.
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

// TxUpsertInstanceMods writes the manifest rows for an install. It takes a transaction
// rather than opening one: 12 §9.4 requires these rows to land in the same flip as the
// job's manifest_written checkpoint, and C1 forbids the filesystem work that produced them
// from happening inside it.
func TxUpsertInstanceMods(ctx context.Context, tx *sql.Tx, mods []InstanceMod) error {
	for i := range mods {
		m := &mods[i]
		// A row being restored after a rolled-back update carries its original timestamp,
		// and keeps it: the install it records is the one that is still on disk, and moving
		// the date to now would date it to the attempt that failed.
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

// TxDeleteInstanceMods removes rows by full name. It takes a transaction because an
// uninstall's rows go in the job's own Finish flip, alongside the terminal status (12 §6).
func TxDeleteInstanceMods(ctx context.Context, tx *sql.Tx, instanceID string, fullNames []string) error {
	for _, name := range fullNames {
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM instance_mods WHERE instance_id = ? AND full_name = ?`, instanceID, name); err != nil {
			return fmt.Errorf("delete instance_mods %s/%s: %w", instanceID, name, err)
		}
	}
	return nil
}

// RollbackInstanceMods restores the rows an install that did not finish had replaced and
// deletes the ones it had added, in one transaction — the database half of a rollback,
// after the files have been undone from the manifests.
//
// `↯` Both halves in one call, because an update's rollback is not a delete. Replacing a
// package rewrites its row in place, so undoing it means putting the *previous* row back;
// deleting it instead would leave the restored old version's files on disk with nothing
// left that names them, which is B9's orphan exactly.
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

// SetInstanceModTags is PATCH /instances/{id}/mods/{full_name} (04 §3): the admin's own
// labels on an installed package. A nil field is left as it is. ok is false when no such
// mod is installed on this instance.
//
// `↯` side is never derived. 03 §5.6 is explicit that Thunderstore metadata does not
// reliably encode whether a mod is needed on the client, so an install writes `unknown` and
// only a human ever changes it.
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

// TxClearModded is TxSetModded's inverse: the framework package has been removed, so the
// instance is a vanilla server again. It matters beyond tidiness — E1's startup assertion
// is skipped on an instance that is not modded, and an instance left flagged would warn
// about missing plugin lines for a server that has no plugins.
func TxClearModded(ctx context.Context, tx *sql.Tx, instanceID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET modded = FALSE, bepinex_version = NULL, updated_at = ? WHERE id = ?`,
		Now(), instanceID); err != nil {
		return fmt.Errorf("clear the modded flag on %s: %w", instanceID, err)
	}
	return nil
}

// TxSetRestartRequired is ADR-012: a change that only takes effect at launch marks the
// instance so the UI can say so, and the next successful start clears it.
func TxSetRestartRequired(ctx context.Context, tx *sql.Tx, instanceID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET restart_required = TRUE, updated_at = ? WHERE id = ?`,
		Now(), instanceID); err != nil {
		return fmt.Errorf("set restart_required on %s: %w", instanceID, err)
	}
	return nil
}

// WriteInstanceMods records an install's manifest rows and marks the instance as needing a
// restart, in one transaction. 12 §6: the transaction wraps the state flip, never the
// download, extraction and hashing that produced these rows — all of which finished before
// this is called.
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

// TxSetModded records that an instance now runs BepInEx (ADR-019, 04 §2). It is a flag and
// a version, not a state: 12 §2's state machine is about the container, and whether the
// server is modded is a fact about its filesystem.
func TxSetModded(ctx context.Context, tx *sql.Tx, instanceID, bepinexVersion string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET modded = TRUE, bepinex_version = ?, updated_at = ? WHERE id = ?`,
		bepinexVersion, Now(), instanceID); err != nil {
		return fmt.Errorf("mark %s modded: %w", instanceID, err)
	}
	return nil
}
