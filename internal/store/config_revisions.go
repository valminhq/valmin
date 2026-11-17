package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"time"
)

// ConfigSource names what produced the bytes a config revision holds.
type ConfigSource string

const (
	// ConfigFound is a file's first recorded version, as the panel first found it.
	ConfigFound ConfigSource = "found"
	// ConfigOutside is a version written outside the panel: by the game, a plugin or by hand.
	ConfigOutside ConfigSource = "outside"
	// ConfigEdit is a version saved from the panel's config editor.
	ConfigEdit ConfigSource = "edit"
	// ConfigSetup is a version written by a setup restore or a definition import.
	ConfigSetup ConfigSource = "setup"
	// ConfigMod is a version a mod install, update or downgrade wrote.
	ConfigMod ConfigSource = "mod"
)

// MaxConfigRevisionBytes bounds the content one revision holds.
const MaxConfigRevisionBytes = 16 << 20

// ErrConfigTooLarge is a config file over MaxConfigRevisionBytes, whose versions are not kept.
var ErrConfigTooLarge = fmt.Errorf("config file is larger than %d MiB", MaxConfigRevisionBytes>>20)

// configRevisionsKept is how many revisions of one file are kept besides its baseline.
const configRevisionsKept = 20

// ConfigRevision is one recorded version of a config file. File is the name the config
// endpoints address it by. UserName is read back with the revision and never written. Mod, FromVersion and ToVersion describe a ConfigMod revision;
// ReplacedEdits marks one that replaced bytes differing from what the mod last shipped.
// Content is empty on a listing.
type ConfigRevision struct {
	ID            string
	InstanceID    string
	File          string
	CreatedAt     time.Time
	Source        ConfigSource
	UserID        string
	UserName      string
	Mod           string
	FromVersion   string
	ToVersion     string
	ReplacedEdits bool
	Baseline      bool
	SHA256        string
	Size          int
	Content       []byte
}

// RecordConfigRevisions records each revision in one transaction. See TxRecordConfigRevisions.
func (db *DB) RecordConfigRevisions(ctx context.Context, revs []ConfigRevision) error {
	if len(revs) == 0 {
		return nil
	}
	return db.inTx(ctx, "record config revisions", func(tx *sql.Tx) error {
		return TxRecordConfigRevisions(ctx, tx, revs)
	})
}

// TxRecordConfigRevisions records each revision inside tx, in order. A revision whose bytes
// match the file's newest one is skipped, and one over MaxConfigRevisionBytes fails with
// ErrConfigTooLarge. A file's first
// revision becomes its baseline, and a ConfigMod revision takes the baseline over. Each file
// keeps its newest configRevisionsKept revisions plus its baseline. A zero CreatedAt means now.
func TxRecordConfigRevisions(ctx context.Context, tx *sql.Tx, revs []ConfigRevision) error {
	for i := range revs {
		if err := recordConfigRevision(ctx, tx, &revs[i]); err != nil {
			return fmt.Errorf("record a revision of %s: %w", revs[i].File, err)
		}
	}
	return nil
}

func recordConfigRevision(ctx context.Context, tx *sql.Tx, r *ConfigRevision) error {
	if len(r.Content) > MaxConfigRevisionBytes {
		return ErrConfigTooLarge
	}
	sum := sha256.Sum256(r.Content)
	digest := hex.EncodeToString(sum[:])
	var newest string
	err := tx.QueryRowContext(ctx, `
		SELECT sha256 FROM config_revisions WHERE instance_id = ? AND file = ?
		ORDER BY created_at DESC, id DESC LIMIT 1`, r.InstanceID, r.File).Scan(&newest)
	first := errors.Is(err, sql.ErrNoRows)
	switch {
	case err != nil && !first:
		return fmt.Errorf("read the newest revision: %w", err)
	case newest == digest:
		return nil
	}
	baseline := first || r.Source == ConfigMod
	if baseline && !first {
		if _, err := tx.ExecContext(ctx, `
			UPDATE config_revisions SET baseline = FALSE WHERE instance_id = ? AND file = ?`,
			r.InstanceID, r.File); err != nil {
			return fmt.Errorf("move the baseline: %w", err)
		}
	}
	content := r.Content
	if content == nil {
		content = []byte{}
	}
	created := r.CreatedAt
	if created.IsZero() {
		created = time.Now()
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO config_revisions (
			id, instance_id, file, created_at, source, user_id, mod, from_version, to_version,
			replaced_edits, baseline, sha256, content
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		NewID(), r.InstanceID, r.File, FormatTime(created), string(r.Source), nullIfEmpty(r.UserID),
		r.Mod, r.FromVersion, r.ToVersion, r.ReplacedEdits, baseline, digest, content); err != nil {
		return fmt.Errorf("insert: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM config_revisions WHERE instance_id = ? AND file = ? AND NOT baseline AND id NOT IN (
			SELECT id FROM config_revisions WHERE instance_id = ? AND file = ?
			ORDER BY created_at DESC, id DESC LIMIT ?)`,
		r.InstanceID, r.File, r.InstanceID, r.File, configRevisionsKept); err != nil {
		return fmt.Errorf("prune: %w", err)
	}
	return nil
}

// configRevisionColumns is every column but the content, in scanConfigRevision's order.
const configRevisionColumns = `r.id, r.instance_id, r.file, r.created_at, r.source,
	COALESCE(r.user_id, ''), COALESCE(u.username, ''), r.mod, r.from_version, r.to_version,
	r.replaced_edits, r.baseline, r.sha256, length(r.content)`

// configRevisionsFrom is the FROM clause configRevisionColumns reads.
const configRevisionsFrom = ` FROM config_revisions r LEFT JOIN users u ON u.id = r.user_id `

// ConfigRevisions lists a file's revisions, newest first, without their content.
func (db *DB) ConfigRevisions(ctx context.Context, instanceID, file string) ([]ConfigRevision, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+configRevisionColumns+configRevisionsFrom+`
		WHERE r.instance_id = ? AND r.file = ? ORDER BY r.created_at DESC, r.id DESC`, instanceID, file)
	if err != nil {
		return nil, fmt.Errorf("list revisions of %s: %w", file, err)
	}
	defer func() { _ = rows.Close() }()
	var out []ConfigRevision
	for rows.Next() {
		r, err := scanConfigRevision(rows, false)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list revisions of %s: %w", file, err)
	}
	return out, nil
}

// LatestConfigRevisions maps each file of an instance to its newest revision, without content.
func (db *DB) LatestConfigRevisions(ctx context.Context, instanceID string) (map[string]ConfigRevision, error) {
	rows, err := db.Reader.QueryContext(ctx, `SELECT `+configRevisionColumns+configRevisionsFrom+`
		WHERE r.instance_id = ? AND r.id = (
			SELECT n.id FROM config_revisions n WHERE n.instance_id = r.instance_id AND n.file = r.file
			ORDER BY n.created_at DESC, n.id DESC LIMIT 1)`, instanceID)
	if err != nil {
		return nil, fmt.Errorf("read the newest config revisions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]ConfigRevision{}
	for rows.Next() {
		r, err := scanConfigRevision(rows, false)
		if err != nil {
			return nil, err
		}
		out[r.File] = r
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read the newest config revisions: %w", err)
	}
	return out, nil
}

// configRevisionWithContent selects configRevisionColumns and the content, for a WHERE clause
// to follow.
const configRevisionWithContent = `SELECT ` + configRevisionColumns + `, r.content` + configRevisionsFrom

// ConfigRevision returns one revision of a file with its content, or nil when the file has no
// revision with that id.
func (db *DB) ConfigRevision(ctx context.Context, instanceID, file, id string) (*ConfigRevision, error) {
	const q = configRevisionWithContent + `WHERE r.instance_id = ? AND r.file = ? AND r.id = ?`
	return db.oneConfigRevision(ctx, q, instanceID, file, id)
}

// ConfigBaseline returns a file's baseline revision with its content, or nil when it has none.
func (db *DB) ConfigBaseline(ctx context.Context, instanceID, file string) (*ConfigRevision, error) {
	const q = configRevisionWithContent + `WHERE r.instance_id = ? AND r.file = ? AND r.baseline
		ORDER BY r.created_at DESC, r.id DESC LIMIT 1`
	return db.oneConfigRevision(ctx, q, instanceID, file)
}

// oneConfigRevision runs query, which selects configRevisionWithContent, and decodes its row.
func (db *DB) oneConfigRevision(ctx context.Context, query string, args ...any) (*ConfigRevision, error) {
	row := db.Reader.QueryRowContext(ctx, query, args...)
	r, err := scanConfigRevision(row, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// TxCopyConfigRevisions copies every revision of one instance to another inside tx, except the
// file named except.
func TxCopyConfigRevisions(ctx context.Context, tx *sql.Tx, from, to, except string) error {
	ids, err := configRevisionIDs(ctx, tx, from, except)
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO config_revisions (
				id, instance_id, file, created_at, source, user_id, mod, from_version, to_version,
				replaced_edits, baseline, sha256, content
			) SELECT ?, ?, file, created_at, source, user_id, mod, from_version, to_version,
				replaced_edits, baseline, sha256, content
			FROM config_revisions WHERE id = ?`, NewID(), to, id); err != nil {
			return fmt.Errorf("copy config revision %s: %w", id, err)
		}
	}
	return nil
}

// configRevisionIDs lists the ids of an instance's revisions, except those of the file named
// except.
func configRevisionIDs(ctx context.Context, tx *sql.Tx, instanceID, except string) ([]string, error) {
	rows, err := tx.QueryContext(ctx, `SELECT id FROM config_revisions WHERE instance_id = ? AND file <> ?`,
		instanceID, except)
	if err != nil {
		return nil, fmt.Errorf("list config revisions to copy: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list config revisions to copy: %w", err)
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list config revisions to copy: %w", err)
	}
	return ids, nil
}

// scanConfigRevision decodes configRevisionColumns, followed by the content when withContent
// is set.
func scanConfigRevision(s scanner, withContent bool) (ConfigRevision, error) {
	var r ConfigRevision
	var created, source string
	dest := []any{
		&r.ID, &r.InstanceID, &r.File, &created, &source, &r.UserID, &r.UserName,
		&r.Mod, &r.FromVersion, &r.ToVersion, &r.ReplacedEdits, &r.Baseline, &r.SHA256, &r.Size,
	}
	if withContent {
		dest = append(dest, &r.Content)
	}
	if err := s.Scan(dest...); err != nil {
		return ConfigRevision{}, err
	}
	r.Source = ConfigSource(source)
	t, err := ParseTime(created)
	if err != nil {
		return ConfigRevision{}, fmt.Errorf("config revision created_at: %w", err)
	}
	r.CreatedAt = t
	return r, nil
}
