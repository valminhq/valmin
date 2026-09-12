package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Webhook is one notification destination. URL holds the AEAD envelope, never the plain
// address: it is a bearer credential, so nothing outside a delivery ever decrypts it and no
// response, job payload or log line carries it (10 §3, 11 §9).
type Webhook struct {
	ID        string
	Name      string
	Kind      string
	URL       string
	Enabled   bool
	CreatedBy *string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ErrWebhookNameTaken reports a destination name collision.
var ErrWebhookNameTaken = errors.New("webhook name already exists")

const webhookColumns = `id, name, kind, url, enabled, created_by, created_at, updated_at`

// CreateWebhook inserts a destination whose URL the caller has already sealed.
func (db *DB) CreateWebhook(ctx context.Context, w *Webhook) error {
	now := Now()
	_, err := db.Writer.ExecContext(ctx, `
		INSERT INTO webhooks (`+webhookColumns+`) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		w.ID, w.Name, w.Kind, w.URL, w.Enabled, w.CreatedBy, now, now)
	if isUniqueViolation(err) {
		return ErrWebhookNameTaken
	}
	if err != nil {
		return fmt.Errorf("create webhook %s: %w", w.ID, err)
	}
	return nil
}

// UpdateWebhook applies the fields a request actually sent. A nil field is untouched, so a
// rename cannot silently clear the stored URL.
func (db *DB) UpdateWebhook(ctx context.Context, id string, name, url *string, enabled *bool) error {
	_, err := db.Writer.ExecContext(ctx, `
		UPDATE webhooks SET
			name = COALESCE(?, name),
			url = COALESCE(?, url),
			enabled = COALESCE(?, enabled),
			updated_at = ?
		WHERE id = ?`, name, url, enabled, Now(), id)
	if isUniqueViolation(err) {
		return ErrWebhookNameTaken
	}
	if err != nil {
		return fmt.Errorf("update webhook %s: %w", id, err)
	}
	return nil
}

// DeleteWebhook removes a destination. Its deliveries go with it.
func (db *DB) DeleteWebhook(ctx context.Context, id string) error {
	if _, err := db.Writer.ExecContext(ctx, `DELETE FROM webhooks WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete webhook %s: %w", id, err)
	}
	return nil
}

// WebhookByID reads one destination, nil when there is none.
func (db *DB) WebhookByID(ctx context.Context, id string) (*Webhook, error) {
	w, err := scanWebhook(db.Reader.QueryRowContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &w, nil
}

// ListWebhooks returns every destination, oldest first.
func (db *DB) ListWebhooks(ctx context.Context) ([]Webhook, error) {
	rows, err := db.Reader.QueryContext(ctx,
		`SELECT `+webhookColumns+` FROM webhooks ORDER BY created_at, id`)
	if err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Webhook
	for rows.Next() {
		w, err := scanWebhook(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list webhooks: %w", err)
	}
	return out, nil
}

// EnabledWebhooks returns the destinations an event fans out to.
func (db *DB) EnabledWebhooks(ctx context.Context) ([]Webhook, error) {
	all, err := db.ListWebhooks(ctx)
	if err != nil {
		return nil, err
	}
	var out []Webhook
	for i := range all {
		if all[i].Enabled {
			out = append(out, all[i])
		}
	}
	return out, nil
}

func scanWebhook(s scanner) (Webhook, error) {
	var w Webhook
	var createdBy sql.NullString
	var created, updated string
	if err := s.Scan(
		&w.ID, &w.Name, &w.Kind, &w.URL, &w.Enabled, &createdBy, &created, &updated,
	); err != nil {
		return Webhook{}, fmt.Errorf("scan webhook row: %w", err)
	}
	if createdBy.Valid {
		w.CreatedBy = &createdBy.String
	}
	var err error
	if w.CreatedAt, err = ParseTime(created); err != nil {
		return Webhook{}, fmt.Errorf("parse webhook created_at: %w", err)
	}
	if w.UpdatedAt, err = ParseTime(updated); err != nil {
		return Webhook{}, fmt.Errorf("parse webhook updated_at: %w", err)
	}
	return w, nil
}

// Delivery statuses.
const (
	DeliveryPending   = "pending"
	DeliveryDelivered = "delivered"
	DeliveryFailed    = "failed"
)

// Delivery is one destination's copy of one event. The row is the delivery intent: it is
// written with the change that caused the event, before any network I/O, so a failed or
// exhausted send stays inspectable rather than existing only as a log line.
type Delivery struct {
	ID         string
	WebhookID  string
	EventID    string
	EventKind  string
	InstanceID *string
	Payload    string
	Status     string
	Attempts   int
	LastError  *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
}

const deliveryColumns = `id, webhook_id, event_id, event_kind, instance_id, payload, status,
	attempts, last_error, created_at, updated_at`

// CreateDelivery records a delivery intent outside a transaction.
func (db *DB) CreateDelivery(ctx context.Context, d *Delivery) error {
	return createDelivery(ctx, db.Writer, d)
}

// TxCreateDelivery records a delivery intent in the transaction that commits the event's own
// change, so a crash cannot leave a state change whose notification was never owed (C1: the
// send itself happens later, in a job, outside every transaction).
func TxCreateDelivery(ctx context.Context, tx *sql.Tx, d *Delivery) error {
	return createDelivery(ctx, tx, d)
}

func createDelivery(ctx context.Context, execer execer, d *Delivery) error {
	now := Now()
	_, err := execer.ExecContext(ctx, `
		INSERT INTO webhook_deliveries (`+deliveryColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		d.ID, d.WebhookID, d.EventID, d.EventKind, d.InstanceID, d.Payload,
		DeliveryPending, 0, nil, now, now)
	if err != nil {
		return fmt.Errorf("record delivery %s: %w", d.ID, err)
	}
	return nil
}

// FinishDelivery records the terminal outcome of one delivery. lastError is the sanitized
// failure: it never carries the destination URL (11 §9).
func (db *DB) FinishDelivery(ctx context.Context, id, status string, attempts int, lastError string) error {
	var failure any
	if lastError != "" {
		failure = lastError
	}
	if _, err := db.Writer.ExecContext(ctx, `
		UPDATE webhook_deliveries
		SET status = ?, attempts = ?, last_error = ?, updated_at = ?
		WHERE id = ?`, status, attempts, failure, Now(), id); err != nil {
		return fmt.Errorf("finish delivery %s: %w", id, err)
	}
	return nil
}

// ListPendingDeliveries returns delivery intents that have not reached a terminal status,
// oldest first. A row is written with the change that caused its event and sent afterwards, so
// this is what the dispatcher reads — including, after a crash, a row whose send never ran.
func (db *DB) ListPendingDeliveries(ctx context.Context, limit int) ([]Delivery, error) {
	rows, err := db.Reader.QueryContext(ctx, `
		SELECT `+deliveryColumns+` FROM webhook_deliveries
		WHERE status = ? ORDER BY created_at, id LIMIT ?`, DeliveryPending, limit)
	if err != nil {
		return nil, fmt.Errorf("list pending deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list pending deliveries: %w", err)
	}
	return out, nil
}

// DeliveryByID reads one delivery, nil when there is none.
func (db *DB) DeliveryByID(ctx context.Context, id string) (*Delivery, error) {
	d, err := scanDelivery(db.Reader.QueryRowContext(ctx,
		`SELECT `+deliveryColumns+` FROM webhook_deliveries WHERE id = ?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

// ListDeliveries returns the most recent deliveries first, one page at a time. after is the
// (created_at, id) of the last row of the previous page, empty for the first.
func (db *DB) ListDeliveries(ctx context.Context, afterTime, afterID string, limit int) ([]Delivery, error) {
	query := `SELECT ` + deliveryColumns + ` FROM webhook_deliveries`
	args := []any{}
	if afterTime != "" {
		query += ` WHERE (created_at, id) < (?, ?)`
		args = append(args, afterTime, afterID)
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Reader.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []Delivery
	for rows.Next() {
		d, err := scanDelivery(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list deliveries: %w", err)
	}
	return out, nil
}

func scanDelivery(s scanner) (Delivery, error) {
	var d Delivery
	var instanceID, lastError sql.NullString
	var created, updated string
	if err := s.Scan(
		&d.ID, &d.WebhookID, &d.EventID, &d.EventKind, &instanceID, &d.Payload, &d.Status,
		&d.Attempts, &lastError, &created, &updated,
	); err != nil {
		return Delivery{}, fmt.Errorf("scan delivery row: %w", err)
	}
	if instanceID.Valid {
		d.InstanceID = &instanceID.String
	}
	if lastError.Valid {
		d.LastError = &lastError.String
	}
	var err error
	if d.CreatedAt, err = ParseTime(created); err != nil {
		return Delivery{}, fmt.Errorf("parse delivery created_at: %w", err)
	}
	if d.UpdatedAt, err = ParseTime(updated); err != nil {
		return Delivery{}, fmt.Errorf("parse delivery updated_at: %w", err)
	}
	return d, nil
}
