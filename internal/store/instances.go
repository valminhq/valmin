package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Instance is the safe-to-serialize shape of an instances row. password is deliberately
// absent — 11 §9 gives it its own audited endpoint and keeps it out of the list and detail
// payloads, and a field that is not on the struct cannot be marshalled by accident (the
// same reasoning User applies to password_hash).
type Instance struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	ContainerID *string `json:"container_id,omitempty"`
	// DataDir is the instance's host-side directory (02 §5) — never exposed over the API,
	// only consumed internally to build a container's bind mounts (08 §5).
	DataDir             string    `json:"-"`
	BasePort            int       `json:"base_port"`
	ServerName          string    `json:"server_name"`
	WorldName           string    `json:"world_name"`
	Public              bool      `json:"public"`
	Crossplay           bool      `json:"crossplay"`
	CrossplayInstanceID string    `json:"crossplay_instance_id"`
	Preset              *string   `json:"preset,omitempty"`
	Modifiers           *string   `json:"modifiers,omitempty"`
	ExtraArgs           *string   `json:"extra_args,omitempty"`
	Modded              bool      `json:"modded"`
	RestartRequired     bool      `json:"restart_required"`
	MemLimitMB          int       `json:"mem_limit_mb"`
	CPULimit            *float64  `json:"cpu_limit,omitempty"`
	GameBuildID         *string   `json:"game_build_id,omitempty"`
	CreatedAt           time.Time `json:"created_at"`
	UpdatedAt           time.Time `json:"updated_at"`
}

const instanceColumns = `id, name, state, container_id, data_dir, base_port, server_name, world_name,
	public, crossplay, crossplay_instance_id, preset, modifiers, extra_args, modded,
	restart_required, mem_limit_mb, cpu_limit, game_build_id, created_at, updated_at`

func scanInstance(s scanner) (Instance, error) {
	var inst Instance
	var containerID, preset, modifiers, extraArgs, gameBuildID sql.NullString
	var cpuLimit sql.NullFloat64
	var createdAt, updatedAt string

	if err := s.Scan(
		&inst.ID,
		&inst.Name,
		&inst.State,
		&containerID,
		&inst.DataDir,
		&inst.BasePort,
		&inst.ServerName,
		&inst.WorldName,
		&inst.Public,
		&inst.Crossplay,
		&inst.CrossplayInstanceID,
		&preset,
		&modifiers,
		&extraArgs,
		&inst.Modded,
		&inst.RestartRequired,
		&inst.MemLimitMB,
		&cpuLimit,
		&gameBuildID,
		&createdAt,
		&updatedAt,
	); err != nil {
		return Instance{}, fmt.Errorf("scan instance row: %w", err)
	}

	var err error
	if inst.CreatedAt, err = ParseTime(createdAt); err != nil {
		return Instance{}, fmt.Errorf("created_at: %w", err)
	}
	if inst.UpdatedAt, err = ParseTime(updatedAt); err != nil {
		return Instance{}, fmt.Errorf("updated_at: %w", err)
	}
	if containerID.Valid {
		inst.ContainerID = &containerID.String
	}
	if preset.Valid {
		inst.Preset = &preset.String
	}
	if modifiers.Valid {
		inst.Modifiers = &modifiers.String
	}
	if extraArgs.Valid {
		inst.ExtraArgs = &extraArgs.String
	}
	if gameBuildID.Valid {
		inst.GameBuildID = &gameBuildID.String
	}
	if cpuLimit.Valid {
		inst.CPULimit = &cpuLimit.Float64
	}
	return inst, nil
}

// InstanceByID reads one instance, or (nil, nil) when it does not exist — the common answer
// for a caller that pairs this with an authorization decision (D2, ADR-038).
func (db *DB) InstanceByID(ctx context.Context, id string) (*Instance, error) {
	row := db.Reader.QueryRowContext(ctx,
		fmt.Sprintf(`SELECT %s FROM instances WHERE id = ?`, instanceColumns), id)
	inst, err := scanInstance(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("look up instance %s: %w", id, err)
	}
	return &inst, nil
}

// ListInstances returns every row named in ids, newest first. ids == nil lists every
// instance — the admin path; a member's ids come from authz.VisibleInstances first, so an
// empty (non-nil) slice correctly returns no rows rather than every one.
//
// `↯` Filtered in Go, not by a dynamic `WHERE id IN (...)`: this is a friend-group panel
// (01 §4 N3), not a hosting business, so one static query plus an in-memory filter is the
// boring mechanism, and it is what keeps every instances query built from a fixed string
// rather than one assembled per call.
func (db *DB) ListInstances(ctx context.Context, ids []string) ([]Instance, error) {
	if ids != nil && len(ids) == 0 {
		return []Instance{}, nil
	}

	rows, err := db.Reader.QueryContext(ctx,
		fmt.Sprintf(`SELECT %s FROM instances ORDER BY name`, instanceColumns))
	if err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var want map[string]bool
	if ids != nil {
		want = make(map[string]bool, len(ids))
		for _, id := range ids {
			want[id] = true
		}
	}

	instances := []Instance{}
	for rows.Next() {
		inst, err := scanInstance(rows)
		if err != nil {
			return nil, fmt.Errorf("scan instance: %w", err)
		}
		if want == nil || want[inst.ID] {
			instances = append(instances, inst)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list instances: %w", err)
	}
	return instances, nil
}

// InstancePassword reads the encrypted envelope of GET /instances/{id}/password's one job
// (11 §9): its own query, never folded into instanceColumns, so the ciphertext is never in
// memory alongside a struct anything else marshals.
func (db *DB) InstancePassword(ctx context.Context, id string) (string, error) {
	var password string
	err := db.Reader.QueryRowContext(ctx, `SELECT password FROM instances WHERE id = ?`, id).Scan(&password)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("read password for instance %s: %w", id, err)
	}
	return password, nil
}

// UsedBasePorts backs instance.Allocator's DB-side check (03 §2).
func (db *DB) UsedBasePorts(ctx context.Context) (map[int]bool, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT base_port FROM instances`)
	if err != nil {
		return nil, fmt.Errorf("list used base ports: %w", err)
	}
	defer func() { _ = rows.Close() }()

	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan base port: %w", err)
		}
		used[p] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list used base ports: %w", err)
	}
	return used, nil
}

// AuditEntry is one row of the permanent record of who did what (09 §4). It never
// cascades — deleting an instance must not erase the trail of what was done to it.
type AuditEntry struct {
	UserID     string
	InstanceID string
	Action     string
	Detail     string
	IP         string
}

// WriteAuditLog records one entry. 11 §9 names its first caller: every read of
// GET /instances/{id}/password.
func (db *DB) WriteAuditLog(ctx context.Context, e *AuditEntry) error {
	var instanceID, ip any
	if e.InstanceID != "" {
		instanceID = e.InstanceID
	}
	if e.IP != "" {
		ip = e.IP
	}
	if _, err := db.Writer.ExecContext(ctx, `
		INSERT INTO audit_log (id, user_id, instance_id, action, detail, ip, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		NewID(), e.UserID, instanceID, e.Action, e.Detail, ip, Now()); err != nil {
		return fmt.Errorf("write audit log entry %s: %w", e.Action, err)
	}
	return nil
}

// ErrInstanceNotFound reports that an id names no row.
var ErrInstanceNotFound = errors.New("instance not found")

// InstanceLimits is PATCH /instances/{id}'s one M1 field set (06 §1 write-back: 09 §3 gives
// InstanceLimits and InstanceExtraArgs actions to gate these two, and no action for the
// rest of "launch config" — server_name, world_name, password, preset, modifiers, public,
// crossplay stay unwritable via this endpoint until that gap is closed).
type InstanceLimits struct {
	MemLimitMB int
	CPULimit   *float64
	ExtraArgs  *string
}

// UpdateInstanceLimits applies patch and sets restart_required — these are launch-time
// container properties, and 12 §2.5 names restart_required as exactly the flag that tells
// an operator their change has not taken effect yet.
func (db *DB) UpdateInstanceLimits(ctx context.Context, id string, patch InstanceLimits) error {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE instances SET mem_limit_mb = ?, cpu_limit = ?, extra_args = ?,
			restart_required = TRUE, updated_at = ?
		WHERE id = ?`,
		patch.MemLimitMB, patch.CPULimit, patch.ExtraArgs, Now(), id)
	if err != nil {
		return fmt.Errorf("update limits for instance %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update limits for instance %s: %w", id, err)
	}
	if n == 0 {
		return ErrInstanceNotFound
	}
	return nil
}

// NewInstance is what CreateInstance needs to insert a fresh row in `created` (12 §2.1).
// Password is already the encrypted envelope — this package never sees plaintext (10 §3).
type NewInstance struct {
	ID                  string
	Name                string
	DataDir             string
	BasePort            int
	ServerName          string
	WorldName           string
	Password            string
	Public              bool
	Crossplay           bool
	CrossplayInstanceID string
	Preset              string
	Modifiers           string // JSON object (04 §2), or ""
	MemLimitMB          int
}

// ErrInstanceNameTaken and ErrBasePortTaken report which of instances' two user-visible
// UNIQUE columns collided. name is the caller's own choice, so it is disambiguated from a
// base_port collision — the panel's own allocation, and, at this scale, only ever a race
// between two concurrent creates (01 §4 N3: not a hosting business, so a name-existence
// pre-check plus this fallback is the boring mechanism, not a dedicated locking scheme).
var (
	ErrInstanceNameTaken = errors.New("instance name already taken")
	ErrBasePortTaken     = errors.New("base port already reserved")
)

// CreateInstance inserts a new instance row already `created`, reserving base_port and
// crossplay_instance_id in the same statement as the row itself (A5, A6) — a single INSERT
// is atomic on SQLite's one writer connection, so this needs no explicit transaction.
func (db *DB) CreateInstance(ctx context.Context, n *NewInstance) error {
	var preset, modifiers any
	if n.Preset != "" {
		preset = n.Preset
	}
	if n.Modifiers != "" {
		modifiers = n.Modifiers
	}
	now := Now()
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			public, crossplay, crossplay_instance_id, preset, modifiers, mem_limit_mb,
			created_at, updated_at
		) VALUES (?, ?, 'created', ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		n.ID, n.Name, n.DataDir, n.BasePort, n.ServerName, n.WorldName, n.Password,
		n.Public, n.Crossplay, n.CrossplayInstanceID, preset, modifiers, n.MemLimitMB,
		now, now)
	if err == nil {
		return nil
	}
	if !isUniqueViolation(err) {
		return fmt.Errorf("create instance %s: %w", n.Name, err)
	}
	var nameExists bool
	if scanErr := db.Reader.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM instances WHERE name = ?)`, n.Name,
	).Scan(&nameExists); scanErr != nil {
		return fmt.Errorf("create instance %s: check name: %w", n.Name, scanErr)
	}
	if nameExists {
		return ErrInstanceNameTaken
	}
	return ErrBasePortTaken
}

// TxUpdateInstanceState is UpdateInstanceState's compare-and-swap, run inside a caller's
// own transaction rather than as its own autocommit statement — the seam a job's
// OnClaim/OnFinish hook needs to land a state flip atomically with the lock (12 §6), built
// in WP-10 and first used here.
func TxUpdateInstanceState(ctx context.Context, tx *sql.Tx, id, from, to string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, Now(), id, from)
	if err != nil {
		return false, fmt.Errorf("move instance %s from %s to %s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("move instance %s from %s to %s: %w", id, from, to, err)
	}
	return n == 1, nil
}

// TxFinishProvisioning is the provision job's OnFinish (12 §6): the terminal state flip and
// the container id it produced, written from data already in memory — never a read inside
// this transaction.
func TxFinishProvisioning(ctx context.Context, tx *sql.Tx, id, from, to, containerID, gameBuildID string) error {
	res, err := tx.ExecContext(ctx, `
		UPDATE instances SET state = ?, container_id = ?, game_build_id = ?, updated_at = ?
		WHERE id = ? AND state = ?`,
		to, containerID, gameBuildID, Now(), id, from)
	if err != nil {
		return fmt.Errorf("finish provisioning instance %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish provisioning instance %s: %w", id, err)
	}
	if n != 1 {
		return fmt.Errorf("finish provisioning instance %s: not in state %s", id, from)
	}
	return nil
}

// TxFinishStart is a successful start/restart's OnFinish (12 §6): the terminal state flip,
// plus clearing restart_required — ADR-012's "cleared by the next successful start". A
// failed start leaves the flag alone (nothing actually restarted), which is why this is not
// folded into TxUpdateInstanceState.
func TxFinishStart(ctx context.Context, tx *sql.Tx, id, from, to string) error {
	res, err := tx.ExecContext(ctx,
		`UPDATE instances SET state = ?, restart_required = FALSE, updated_at = ? WHERE id = ? AND state = ?`,
		to, Now(), id, from)
	if err != nil {
		return fmt.Errorf("finish start for instance %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("finish start for instance %s: %w", id, err)
	}
	if n != 1 {
		return fmt.Errorf("finish start for instance %s: not in state %s", id, from)
	}
	return nil
}

// TxDeleteInstance is the delete job's OnFinish (12 §6): the row is removed outright, which
// is `deleting`'s only successor (12 §2.1) — ON DELETE SET NULL then clears job_runs's
// reference to it, including this very job's own row (12 §4.2).
func TxDeleteInstance(ctx context.Context, tx *sql.Tx, id, from string) error {
	res, err := tx.ExecContext(ctx, `DELETE FROM instances WHERE id = ? AND state = ?`, id, from)
	if err != nil {
		return fmt.Errorf("delete instance %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete instance %s: %w", id, err)
	}
	if n != 1 {
		return fmt.Errorf("delete instance %s: not in state %s", id, from)
	}
	return nil
}

// UpdateInstanceState is the compare-and-swap 12 §1 needs for its two writers: this row
// only moves if it is still in from when the write lands, which is what makes acknowledge
// (12 §2.4) safe to call concurrently with itself.
func (db *DB) UpdateInstanceState(ctx context.Context, id, from, to string) (bool, error) {
	res, err := db.Writer.ExecContext(ctx,
		`UPDATE instances SET state = ?, updated_at = ? WHERE id = ? AND state = ?`,
		to, Now(), id, from)
	if err != nil {
		return false, fmt.Errorf("move instance %s from %s to %s: %w", id, from, to, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("move instance %s from %s to %s: %w", id, from, to, err)
	}
	return n == 1, nil
}
