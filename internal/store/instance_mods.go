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
