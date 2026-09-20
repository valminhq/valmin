package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// AlertCondition is one row of alert_conditions: something believed true of an instance, or of
// the host when InstanceID is nil. An open row has no ResolvedAt.
type AlertCondition struct {
	ID          string
	InstanceID  *string
	Kind        string
	Detail      map[string]string
	FirstSeenAt time.Time
	LastSeenAt  time.Time
	ResolvedAt  *time.Time
}

// ObservedCondition is what a scan believes right now. It restates the evaluator's condition
// because the evaluator imports this package.
type ObservedCondition struct {
	Kind       string
	InstanceID *string
	Detail     map[string]string
}

// ConditionDiff is what one reconciliation changed — the two edges rules dispatch on. A
// condition that was open and still is appears in neither.
type ConditionDiff struct {
	Opened   []AlertCondition
	Resolved []AlertCondition
}

const conditionColumns = `id, instance_id, kind, detail, first_seen_at, last_seen_at, resolved_at`

// conditionKey identifies a condition across scans, mirroring the unique index's COALESCE over
// a null instance.
func conditionKey(kind string, instanceID *string) string {
	return kind + "\x00" + deref(instanceID)
}

func scanCondition(s scanner) (AlertCondition, error) {
	var c AlertCondition
	var instanceID, resolvedAt sql.NullString
	var detail string
	var firstSeen, lastSeen string
	if err := s.Scan(&c.ID, &instanceID, &c.Kind, &detail, &firstSeen, &lastSeen, &resolvedAt); err != nil {
		return AlertCondition{}, fmt.Errorf("scan alert condition: %w", err)
	}
	if instanceID.Valid {
		c.InstanceID = &instanceID.String
	}
	if detail != "" {
		if err := json.Unmarshal([]byte(detail), &c.Detail); err != nil {
			return AlertCondition{}, fmt.Errorf("parse alert condition detail: %w", err)
		}
	}
	for _, f := range []struct {
		raw string
		dst *time.Time
	}{{firstSeen, &c.FirstSeenAt}, {lastSeen, &c.LastSeenAt}} {
		t, err := ParseTime(f.raw)
		if err != nil {
			return AlertCondition{}, fmt.Errorf("parse alert condition timestamp: %w", err)
		}
		*f.dst = t
	}
	if resolvedAt.Valid {
		t, err := ParseTime(resolvedAt.String)
		if err != nil {
			return AlertCondition{}, fmt.Errorf("parse alert condition resolved_at: %w", err)
		}
		c.ResolvedAt = &t
	}
	return c, nil
}

// OpenConditions returns every condition currently believed true, newest first. The caller
// filters it to the instances its user may see.
func (db *DB) OpenConditions(ctx context.Context) ([]AlertCondition, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM alert_conditions WHERE resolved_at IS NULL
		 ORDER BY first_seen_at DESC, id DESC`, conditionColumns))
	if err != nil {
		return nil, fmt.Errorf("read open conditions: %w", err)
	}
	return collectConditions(rows)
}

func collectConditions(rows *sql.Rows) ([]AlertCondition, error) {
	defer func() { _ = rows.Close() }()
	out := []AlertCondition{}
	for rows.Next() {
		c, err := scanCondition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read conditions: %w", err)
	}
	return out, nil
}

// ReconcileConditions makes the stored set match what a scan observed, in one transaction, and
// reports the edges. The scan does its filesystem reads before calling this (C1, 10 §4.3).
func (db *DB) ReconcileConditions(
	ctx context.Context, observed []ObservedCondition, now time.Time,
) (ConditionDiff, error) {
	var diff ConditionDiff
	err := db.inTx(ctx, "reconcile conditions", func(tx *sql.Tx) error {
		open, err := openConditionsTx(ctx, tx)
		if err != nil {
			return err
		}
		byKey := make(map[string]*AlertCondition, len(open))
		for i := range open {
			byKey[conditionKey(open[i].Kind, open[i].InstanceID)] = &open[i]
		}

		seen := make(map[string]bool, len(observed))
		for _, o := range observed {
			key := conditionKey(o.Kind, o.InstanceID)
			// A doubled observation must open one row; a second would violate the unique index.
			if seen[key] {
				continue
			}
			seen[key] = true

			opened, err := upsertCondition(ctx, tx, o, byKey[key], now)
			if err != nil {
				return err
			}
			if opened != nil {
				diff.Opened = append(diff.Opened, *opened)
			}
		}

		diff.Resolved, err = resolveMissing(ctx, tx, open, seen, now)
		return err
	})
	if err != nil {
		return ConditionDiff{}, err
	}
	return diff, nil
}

func openConditionsTx(ctx context.Context, tx *sql.Tx) ([]AlertCondition, error) {
	rows, err := tx.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM alert_conditions WHERE resolved_at IS NULL`, conditionColumns))
	if err != nil {
		return nil, fmt.Errorf("read open conditions: %w", err)
	}
	return collectConditions(rows)
}

// upsertCondition touches an already-open condition or opens a new one. existing is nil when
// none is open; the returned row is non-nil only when one opened.
func upsertCondition(
	ctx context.Context, tx *sql.Tx, o ObservedCondition,
	existing *AlertCondition, now time.Time,
) (*AlertCondition, error) {
	detail, err := json.Marshal(o.Detail)
	if err != nil {
		return nil, fmt.Errorf("encode condition detail: %w", err)
	}
	stamp := FormatTime(now)
	if existing != nil {
		if _, err := tx.ExecContext(ctx,
			`UPDATE alert_conditions SET last_seen_at = ?, detail = ? WHERE id = ?`,
			stamp, string(detail), existing.ID); err != nil {
			return nil, fmt.Errorf("touch condition %s: %w", existing.ID, err)
		}
		return nil, nil
	}

	c := AlertCondition{
		ID: NewID(), InstanceID: o.InstanceID, Kind: o.Kind, Detail: o.Detail,
		FirstSeenAt: now, LastSeenAt: now,
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO alert_conditions (id, instance_id, kind, detail, first_seen_at, last_seen_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		c.ID, c.InstanceID, c.Kind, string(detail), stamp, stamp); err != nil {
		return nil, fmt.Errorf("open condition %s: %w", c.Kind, err)
	}
	return &c, nil
}

// resolveMissing closes every open condition the scan no longer observed.
func resolveMissing(
	ctx context.Context, tx *sql.Tx, open []AlertCondition, seen map[string]bool, now time.Time,
) ([]AlertCondition, error) {
	var resolved []AlertCondition
	stamp := FormatTime(now)
	for _, c := range open {
		if seen[conditionKey(c.Kind, c.InstanceID)] {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE alert_conditions SET resolved_at = ? WHERE id = ?`, stamp, c.ID); err != nil {
			return nil, fmt.Errorf("resolve condition %s: %w", c.ID, err)
		}
		c.ResolvedAt = &now
		resolved = append(resolved, c)
	}
	return resolved, nil
}

// Alert edges.
const (
	EdgeOpened   = "opened"
	EdgeResolved = "resolved"
)

// MarkNotified claims the right to announce one edge of one condition to one rule, reporting
// false when it was already claimed. The primary key is the deduplication.
func (db *DB) MarkNotified(ctx context.Context, conditionID, ruleID, edge string, now time.Time) (bool, error) {
	res, err := db.Writer.ExecContext(ctx, `
		INSERT OR IGNORE INTO alert_notifications (condition_id, rule_id, edge, notified_at)
		VALUES (?, ?, ?, ?)`, conditionID, ruleID, edge, FormatTime(now))
	if err != nil {
		return false, fmt.Errorf("claim notification for condition %s: %w", conditionID, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim notification for condition %s: %w", conditionID, err)
	}
	return n > 0, nil
}

// AlertRule is one row of alert_rules with its destinations. A nil InstanceID covers every
// instance, including ones created later.
type AlertRule struct {
	ID            string
	InstanceID    *string
	ConditionKind string
	Params        string
	// QuietStart and QuietEnd are minutes from local midnight in QuietTZ, all nil together when
	// the rule has no quiet hours. A window past midnight has a start above its end.
	QuietStart *int
	QuietEnd   *int
	QuietTZ    *string
	Enabled    bool
	CreatedBy  *string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	WebhookIDs []string
}

const ruleColumns = `id, instance_id, condition_kind, params, quiet_start, quiet_end, quiet_tz,
	enabled, created_by, created_at, updated_at`

func scanRule(s scanner) (AlertRule, error) {
	var r AlertRule
	var instanceID, quietTZ, createdBy sql.NullString
	var quietStart, quietEnd sql.NullInt64
	var createdAt, updatedAt string
	if err := s.Scan(&r.ID, &instanceID, &r.ConditionKind, &r.Params, &quietStart, &quietEnd,
		&quietTZ, &r.Enabled, &createdBy, &createdAt, &updatedAt); err != nil {
		return AlertRule{}, fmt.Errorf("scan alert rule: %w", err)
	}
	for _, f := range []struct {
		ns  sql.NullString
		dst **string
	}{{instanceID, &r.InstanceID}, {quietTZ, &r.QuietTZ}, {createdBy, &r.CreatedBy}} {
		if f.ns.Valid {
			v := f.ns.String
			*f.dst = &v
		}
	}
	for _, f := range []struct {
		ni  sql.NullInt64
		dst **int
	}{{quietStart, &r.QuietStart}, {quietEnd, &r.QuietEnd}} {
		if f.ni.Valid {
			v := int(f.ni.Int64)
			*f.dst = &v
		}
	}
	for _, f := range []struct {
		raw string
		dst *time.Time
	}{{createdAt, &r.CreatedAt}, {updatedAt, &r.UpdatedAt}} {
		t, err := ParseTime(f.raw)
		if err != nil {
			return AlertRule{}, fmt.Errorf("parse alert rule timestamp: %w", err)
		}
		*f.dst = t
	}
	return r, nil
}

// ListAlertRules returns every rule with its destinations. No cursor: rules are a handful of
// operator-written rows, not a growing log (11 §4).
func (db *DB) ListAlertRules(ctx context.Context) ([]AlertRule, error) {
	rows, err := db.Reader.QueryContext(ctx, fmt.Sprintf(
		`SELECT %s FROM alert_rules ORDER BY condition_kind, instance_id, id`, ruleColumns))
	if err != nil {
		return nil, fmt.Errorf("list alert rules: %w", err)
	}
	out, err := collectRules(rows)
	if err != nil {
		return nil, err
	}
	return out, db.attachDestinations(ctx, out)
}

func collectRules(rows *sql.Rows) ([]AlertRule, error) {
	defer func() { _ = rows.Close() }()
	out := []AlertRule{}
	for rows.Next() {
		r, err := scanRule(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read alert rules: %w", err)
	}
	return out, nil
}

// attachDestinations fills WebhookIDs for every rule in one query.
func (db *DB) attachDestinations(ctx context.Context, rules []AlertRule) error {
	if len(rules) == 0 {
		return nil
	}
	rows, err := db.Reader.QueryContext(ctx,
		`SELECT rule_id, webhook_id FROM alert_rule_destinations ORDER BY rule_id, webhook_id`)
	if err != nil {
		return fmt.Errorf("read alert rule destinations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	byRule := map[string][]string{}
	for rows.Next() {
		var ruleID, webhookID string
		if err := rows.Scan(&ruleID, &webhookID); err != nil {
			return fmt.Errorf("scan alert rule destination: %w", err)
		}
		byRule[ruleID] = append(byRule[ruleID], webhookID)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read alert rule destinations: %w", err)
	}
	for i := range rules {
		rules[i].WebhookIDs = byRule[rules[i].ID]
	}
	return nil
}

// AlertRuleByID reads one rule with its destinations. A missing row is (nil, nil).
func (db *DB) AlertRuleByID(ctx context.Context, id string) (*AlertRule, error) {
	row := db.Reader.QueryRowContext(ctx, fmt.Sprintf(
		`SELECT %s FROM alert_rules WHERE id = ?`, ruleColumns), id)
	r, err := scanRule(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read alert rule %s: %w", id, err)
	}
	one := []AlertRule{r}
	if err := db.attachDestinations(ctx, one); err != nil {
		return nil, err
	}
	return &one[0], nil
}

// SaveAlertRule inserts or replaces a rule and its destinations in one transaction, so a rule
// is never briefly live with the wrong destinations.
func (db *DB) SaveAlertRule(ctx context.Context, r *AlertRule) error {
	return db.inTx(ctx, "save alert rule", func(tx *sql.Tx) error {
		now := Now()
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO alert_rules (
				id, instance_id, condition_kind, params, quiet_start, quiet_end, quiet_tz,
				enabled, created_by, created_at, updated_at
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (id) DO UPDATE SET
				instance_id = excluded.instance_id,
				condition_kind = excluded.condition_kind,
				params = excluded.params,
				quiet_start = excluded.quiet_start,
				quiet_end = excluded.quiet_end,
				quiet_tz = excluded.quiet_tz,
				enabled = excluded.enabled,
				updated_at = excluded.updated_at`,
			r.ID, r.InstanceID, r.ConditionKind, r.Params, r.QuietStart, r.QuietEnd, r.QuietTZ,
			r.Enabled, r.CreatedBy, now, now); err != nil {
			return fmt.Errorf("save alert rule %s: %w", r.ID, err)
		}
		if _, err := tx.ExecContext(ctx,
			`DELETE FROM alert_rule_destinations WHERE rule_id = ?`, r.ID); err != nil {
			return fmt.Errorf("clear destinations of rule %s: %w", r.ID, err)
		}
		// The foreign key is integrity, not validation: the handler answers 422 for an unknown
		// destination before reaching here.
		for _, webhookID := range r.WebhookIDs {
			if _, err := tx.ExecContext(ctx,
				`INSERT INTO alert_rule_destinations (rule_id, webhook_id) VALUES (?, ?)`,
				r.ID, webhookID); err != nil {
				return fmt.Errorf("add destination %s to rule %s: %w", webhookID, r.ID, err)
			}
		}
		return nil
	})
}

// DeleteAlertRule removes a rule. Its destinations and its notification history go with it.
func (db *DB) DeleteAlertRule(ctx context.Context, id string) error {
	if _, err := db.Writer.ExecContext(ctx, `DELETE FROM alert_rules WHERE id = ?`, id); err != nil {
		return fmt.Errorf("delete alert rule %s: %w", id, err)
	}
	return nil
}

// RecordIncident notes a server going down on its own, written where the observer detects it:
// an instance that crashes and restarts between two scans is never observed down.
func (db *DB) RecordIncident(ctx context.Context, instanceID, reason string, now time.Time) error {
	if _, err := db.Writer.ExecContext(ctx, `
		INSERT INTO instance_incidents (id, instance_id, reason, occurred_at) VALUES (?, ?, ?, ?)`,
		NewID(), instanceID, reason, FormatTime(now)); err != nil {
		return fmt.Errorf("record incident for instance %s: %w", instanceID, err)
	}
	return nil
}

// RecentIncidents returns when each instance went down on its own since the given time, keyed
// by instance id, newest first.
func (db *DB) RecentIncidents(ctx context.Context, since time.Time) (map[string][]time.Time, error) {
	rows, err := db.Reader.QueryContext(ctx,
		`SELECT instance_id, occurred_at FROM instance_incidents
		 WHERE occurred_at > ? ORDER BY instance_id, occurred_at DESC`, FormatTime(since))
	if err != nil {
		return nil, fmt.Errorf("read recent incidents: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := map[string][]time.Time{}
	for rows.Next() {
		var instanceID, at string
		if err := rows.Scan(&instanceID, &at); err != nil {
			return nil, fmt.Errorf("scan incident: %w", err)
		}
		t, err := ParseTime(at)
		if err != nil {
			return nil, fmt.Errorf("parse incident timestamp: %w", err)
		}
		out[instanceID] = append(out[instanceID], t)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("read recent incidents: %w", err)
	}
	return out, nil
}

// SweepIncidents drops incident rows past retention. They are read over a window of minutes, so
// anything older is growth with no reader.
func (db *DB) SweepIncidents(ctx context.Context, before time.Time) (int64, error) {
	res, err := db.Writer.ExecContext(ctx,
		`DELETE FROM instance_incidents WHERE occurred_at < ?`, FormatTime(before))
	if err != nil {
		return 0, fmt.Errorf("sweep incidents: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("sweep incidents: %w", err)
	}
	return n, nil
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
