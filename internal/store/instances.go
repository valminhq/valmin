package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Instance is the safe-to-serialize shape of an instances row. password is deliberately
// absent: it has its own audited endpoint (11 §9), and a field that is not on the struct
// cannot be marshalled by accident.
type Instance struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	State       string  `json:"state"`
	ContainerID *string `json:"container_id,omitempty"`
	// DataDir is the instance's host-side directory (02 §5). Never exposed over the API, only
	// used to build a container's bind mounts (08 §5).
	DataDir             string   `json:"-"`
	BasePort            int      `json:"base_port"`
	ServerName          string   `json:"server_name"`
	WorldName           string   `json:"world_name"`
	Public              bool     `json:"public"`
	Crossplay           bool     `json:"crossplay"`
	CrossplayInstanceID string   `json:"crossplay_instance_id"`
	Preset              *string  `json:"preset,omitempty"`
	Modifiers           *string  `json:"modifiers,omitempty"`
	ExtraArgs           *string  `json:"extra_args,omitempty"`
	Modded              bool     `json:"modded"`
	BepInExVersion      *string  `json:"bepinex_version,omitempty"`
	RestartRequired     bool     `json:"restart_required"`
	MemLimitMB          int      `json:"mem_limit_mb"`
	CPULimit            *float64 `json:"cpu_limit,omitempty"`
	GameBuildID         *string  `json:"game_build_id,omitempty"`
	// BackupKeepCold and BackupKeepHot are the retention counts, applied to quiesced and
	// hot-copy archives independently so a burst of hot copies cannot evict a cold one
	// (02 §4.4 step 7, B12). 0 keeps everything in that class.
	BackupKeepCold int `json:"backup_keep_cold"`
	BackupKeepHot  int `json:"backup_keep_hot"`
	// BackupOnRestart takes a cold archive between a restart's stop and its start.
	BackupOnRestart bool      `json:"backup_on_restart"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

const instanceColumns = `id, name, state, container_id, data_dir, base_port, server_name, world_name,
	public, crossplay, crossplay_instance_id, preset, modifiers, extra_args, modded, bepinex_version,
	restart_required, mem_limit_mb, cpu_limit, game_build_id,
	backup_keep_cold, backup_keep_hot, backup_on_restart, created_at, updated_at`

func scanInstance(s scanner) (Instance, error) {
	var inst Instance
	var containerID, preset, modifiers, extraArgs, gameBuildID, bepinexVersion sql.NullString
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
		&bepinexVersion,
		&inst.RestartRequired,
		&inst.MemLimitMB,
		&cpuLimit,
		&gameBuildID,
		&inst.BackupKeepCold,
		&inst.BackupKeepHot,
		&inst.BackupOnRestart,
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
	if bepinexVersion.Valid {
		inst.BepInExVersion = &bepinexVersion.String
	}
	if gameBuildID.Valid {
		inst.GameBuildID = &gameBuildID.String
	}
	if cpuLimit.Valid {
		inst.CPULimit = &cpuLimit.Float64
	}
	return inst, nil
}

// InstanceByID reads one instance, or (nil, nil) when it does not exist, which a caller
// pairing this with an authorization decision answers as 404 (D2, ADR-038).
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
// Filtered in Go rather than by a dynamic `WHERE id IN (...)`, so every instances query is
// built from a fixed string. At this scale one static query plus an in-memory filter is
// enough.
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

// InstancePassword reads the encrypted envelope for GET /instances/{id}/password (11 §9). Its
// own query, never folded into instanceColumns, so the ciphertext is never in memory alongside
// a struct anything else marshals.
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

// AuditEntry is one row of the permanent record of who did what (09 §4). It never cascades:
// deleting an instance does not erase the trail of what was done to it.
type AuditEntry struct {
	UserID     string
	InstanceID string
	Action     string
	Detail     string
	IP         string
}

// WriteAuditLog records one entry.
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

// InstanceLaunch is the field set PATCH /instances/{id} accepts. instance.limits and
// instance.extra_args stay admin-only because they shape the container; the rest is the
// grantable instance.settings (09 §3.2, D15).
//
// crossplay_instance_id is deliberately absent: it is fixed at provision and immutable for the
// instance's life, so toggling crossplay changes the flag and never the id (A5, ADR-027).
type InstanceLaunch struct {
	ServerName string
	// Password is already the encrypted envelope — this package never sees plaintext (10 §3).
	Password   string
	Public     bool
	Crossplay  bool
	Preset     *string
	Modifiers  *string
	MemLimitMB int
	CPULimit   *float64
	ExtraArgs  *string
}

// UpdateInstanceLaunch applies patch and sets restart_required, since these properties take
// effect at launch (12 §2.5). One statement, so a patch cannot land half-applied.
func (db *DB) UpdateInstanceLaunch(ctx context.Context, id string, patch *InstanceLaunch) error {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE instances SET server_name = ?, password = ?, public = ?, crossplay = ?,
			preset = ?, modifiers = ?, mem_limit_mb = ?, cpu_limit = ?, extra_args = ?,
			restart_required = TRUE, updated_at = ?
		WHERE id = ?`,
		patch.ServerName, patch.Password, patch.Public, patch.Crossplay,
		patch.Preset, patch.Modifiers, patch.MemLimitMB, patch.CPULimit, patch.ExtraArgs,
		Now(), id)
	if err != nil {
		return fmt.Errorf("update launch settings for instance %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update launch settings for instance %s: %w", id, err)
	}
	if n == 0 {
		return ErrInstanceNotFound
	}
	return nil
}

// BackupPolicy is one instance's retention, in archives kept per class, plus whether a
// restart takes a cold archive on its way through `stopped` (02 §4.4 step 7).
type BackupPolicy struct {
	KeepCold  int
	KeepHot   int
	OnRestart bool
}

// UpdateInstanceBackupPolicy applies policy. Deliberately not part of UpdateInstanceLaunch:
// these take effect immediately and shape no container, so they must not set
// restart_required.
func (db *DB) UpdateInstanceBackupPolicy(ctx context.Context, id string, policy BackupPolicy) error {
	res, err := db.Writer.ExecContext(ctx, `
		UPDATE instances SET backup_keep_cold = ?, backup_keep_hot = ?, backup_on_restart = ?,
			updated_at = ?
		WHERE id = ?`,
		policy.KeepCold, policy.KeepHot, policy.OnRestart, Now(), id)
	if err != nil {
		return fmt.Errorf("update backup policy for instance %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("update backup policy for instance %s: %w", id, err)
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

// ErrInstanceNameTaken and ErrBasePortTaken report which of instances' two user-visible UNIQUE
// columns collided: a name the caller chose, or a base port the panel allocated and lost a race
// on.
var (
	ErrInstanceNameTaken = errors.New("instance name already taken")
	ErrBasePortTaken     = errors.New("base port already reserved")
)

// CreateInstance inserts a new instance row already `created`, reserving base_port and
// crossplay_instance_id in the same statement (A5, A6). A single INSERT is atomic on the one
// writer connection, so it needs no explicit transaction.
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

// TxUpdateInstanceState is UpdateInstanceState's compare-and-swap inside a caller's own
// transaction, so a job's OnClaim/OnFinish hook can land a state flip atomically with the
// lock.
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

// TxSetInstanceBuildID records the build an instance now runs, inside a caller's transaction
// so it commits with the job's own state flip (12 §6). Provisioning writes it through
// TxFinishProvisioning; a game update is the only other thing that changes it.
func TxSetInstanceBuildID(ctx context.Context, tx *sql.Tx, id, gameBuildID string) error {
	if _, err := tx.ExecContext(ctx,
		`UPDATE instances SET game_build_id = ?, updated_at = ? WHERE id = ?`,
		gameBuildID, Now(), id,
	); err != nil {
		return fmt.Errorf("record build %s for instance %s: %w", gameBuildID, id, err)
	}
	return nil
}

// TxFinishProvisioning is the provision job's OnFinish (12 §6): the terminal state flip and
// the container id it produced, both already in memory.
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

// TxFinishStart is a successful start or restart's OnFinish (12 §6): the terminal state flip
// plus clearing restart_required (ADR-012). A failed start leaves the flag alone, which is why
// this is separate from TxUpdateInstanceState.
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

// TxDeleteInstance is the delete job's OnFinish (12 §6): the row is removed outright, which is
// `deleting`'s only successor (12 §2.1). ON DELETE SET NULL then clears job_runs's reference to
// it, this job's own row included (12 §4.2).
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

// SetInstanceContainerID repoints a row at the container reconciliation found for it.
// Reconciliation joins on the io.valmin.instance.id label rather than this column, so a stale
// or lost container_id is recoverable and what the join found wins (08 §6.1).
func (db *DB) SetInstanceContainerID(ctx context.Context, id, containerID string) error {
	if _, err := db.Writer.ExecContext(ctx,
		`UPDATE instances SET container_id = ?, updated_at = ? WHERE id = ?`,
		containerID, Now(), id,
	); err != nil {
		return fmt.Errorf("set container id for instance %s: %w", id, err)
	}
	return nil
}

// UpdateInstanceState is the compare-and-swap 12 §1 needs for its two writers: the row only
// moves if it is still in from when the write lands.
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
