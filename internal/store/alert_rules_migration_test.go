package store

import (
	"maps"
	"slices"
	"testing"
)

// TestCatchAllBecomesRules asserts that each channel posting the built-in alerts is named by an
// ordinary rule of the same scope for each of them, that a channel a rule of the kind already
// names keeps only that rule, and that the flag is gone.
func TestCatchAllBecomesRules(t *testing.T) {
	t.Parallel()
	db := migrateTo(t, 48)
	server := seedInstance(t, db, NewID(), 2456)

	webhook := func(name string, instanceID *string, catchAll bool) string {
		id := NewID()
		exec(t, db.Writer, `INSERT INTO webhooks
			(id, name, kind, url, enabled, instance_id, catch_all, created_at, updated_at)
			VALUES (?, ?, 'discord', 'sealed', TRUE, ?, ?, ?, ?)`,
			id, name, instanceID, catchAll, Now(), Now())
		return id
	}
	a := webhook("a", nil, true)
	b := webhook("b", nil, true)
	webhook("c", nil, false)
	s := webhook("s", &server, true)

	styled := NewID()
	exec(t, db.Writer, `INSERT INTO alert_rules (id, condition_kind, created_at, updated_at)
		VALUES (?, 'instance_down', ?, ?)`, styled, Now(), Now())
	exec(t, db.Writer, `INSERT INTO alert_rule_destinations (rule_id, webhook_id) VALUES (?, ?)`, styled, a)

	applyVersion(t, db, 49)

	rows, err := db.Reader.QueryContext(t.Context(), `
		SELECT r.id, r.condition_kind, COALESCE(r.instance_id, ''), d.webhook_id
		FROM alert_rules r JOIN alert_rule_destinations d ON d.rule_id = r.id
		WHERE r.id <> ? ORDER BY d.webhook_id`, styled)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	got := map[string][]string{}
	ids := map[string]bool{}
	for rows.Next() {
		var id, kind, scope, webhookID string
		if err := rows.Scan(&id, &kind, &scope, &webhookID); err != nil {
			t.Fatal(err)
		}
		if len(id) != 36 {
			t.Errorf("rule id %q is not UUID-shaped", id)
		}
		ids[id] = true
		key := kind + "@" + scope
		got[key] = append(got[key], webhookID)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}

	want := map[string][]string{
		"instance_down@":          {b},
		"backup_failed@":          {a, b},
		"new_game_build@":         {a, b},
		"instance_down@" + server: {s},
		"backup_failed@" + server: {s},
	}
	for _, named := range want {
		slices.Sort(named)
	}
	if !maps.EqualFunc(got, want, slices.Equal[[]string]) {
		t.Errorf("rules after migration = %v, want %v", got, want)
	}
	if len(ids) != len(want) {
		t.Errorf("%d rules created, want %d", len(ids), len(want))
	}
	if _, err := db.Reader.ExecContext(t.Context(), `SELECT catch_all FROM webhooks`); err == nil {
		t.Error("webhooks.catch_all survived the migration")
	}
}
