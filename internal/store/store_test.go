package store

import (
	"database/sql"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// open returns a migrated database in a temp directory.
func open(t *testing.T) *DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "panel.db")
	db, err := Open(t.Context(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return db
}

func exec(t *testing.T, db *sql.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("exec %q: %v", query, err)
	}
}

// seedUser inserts a user and returns its id.
func seedUser(t *testing.T, db *DB, id string) string {
	t.Helper()
	exec(t, db.Writer,
		`INSERT INTO users (id, username, password_hash, role, created_at) VALUES (?, ?, ?, ?, ?)`,
		id, "user-"+id, "argon2id$stub", "admin", time.Now().UTC())
	return id
}

// seedInstance inserts an instance and returns its id.
func seedInstance(t *testing.T, db *DB, id string, basePort int) string {
	t.Helper()
	exec(t, db.Writer, `
		INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, 'stopped', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "inst-"+id, "/srv/valmin/instances/"+id, basePort,
		"Server "+id, "World"+id, "v1.k.n.ct", "cp-"+id,
		time.Now().UTC(), time.Now().UTC())
	return id
}

// TestPragmasAssertedOnEveryConnection covers 10 §4.3. SQLite disables foreign_keys by
// default, which makes every ON DELETE CASCADE inert and silent.
func TestPragmasAssertedOnEveryConnection(t *testing.T) {
	db := open(t)

	for name, pool := range map[string]*sql.DB{"writer": db.Writer, "reader": db.Reader} {
		var got string
		if err := pool.QueryRowContext(t.Context(), "PRAGMA foreign_keys").Scan(&got); err != nil {
			t.Fatalf("%s: read pragma: %v", name, err)
		}
		if got != "1" {
			t.Errorf("%s: PRAGMA foreign_keys = %q, want 1", name, got)
		}
	}
}

// TestAssertPragmasCatchesAnUnappliedPragma is the negative test for the assertion
// itself. Without it, a pragma spelled the other driver's way would pass silently, which
// is the failure mode 10 §4.3 predicts.
func TestAssertPragmasCatchesAnUnappliedPragma(t *testing.T) {
	dsn := "file:" + filepath.Join(t.TempDir(), "bare.db")

	bare, err := sql.Open("sqlite", dsn) // deliberately no _pragma parameters
	if err != nil {
		t.Fatalf("sql.Open: %v", err)
	}
	defer bare.Close()
	bare.SetMaxOpenConns(1)

	err = assertPragmas(t.Context(), bare, 1)
	if err == nil {
		t.Fatal("assertPragmas passed on a connection with no pragmas applied")
	}
	if !strings.Contains(err.Error(), "foreign_keys") {
		t.Errorf("error = %v, want it to name foreign_keys", err)
	}
}

// TestForeignKeysAreEnforced proves the cascade is live, not merely declared.
func TestForeignKeysAreEnforced(t *testing.T) {
	db := open(t)
	userID := seedUser(t, db, "u1")

	exec(t, db.Writer, `
		INSERT INTO sessions (
			id, token_hash, user_id, created_at, last_seen_at,
			idle_expires_at, absolute_expires_at
		) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"s1", "sha256:stub", userID,
		time.Now().UTC(), time.Now().UTC(), time.Now().UTC(), time.Now().UTC())

	exec(t, db.Writer, `DELETE FROM users WHERE id = ?`, userID)

	var n int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM sessions WHERE user_id = ?`, userID).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("%d sessions survived the user delete; the cascade is inert", n)
	}
}

// TestReferencesAreRejected proves a bad reference fails rather than creating an orphan.
func TestReferencesAreRejected(t *testing.T) {
	db := open(t)
	_, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO sessions (
			id, token_hash, user_id, created_at, last_seen_at,
			idle_expires_at, absolute_expires_at
		) VALUES ('s1', 'h', 'no-such-user', ?, ?, ?, ?)`,
		time.Now().UTC(), time.Now().UTC(), time.Now().UTC(), time.Now().UTC())
	if err == nil {
		t.Error("inserted a session for a nonexistent user")
	}
}

// TestJobRunsInstanceIDDoesNotCascade covers INVARIANTS C15 and 12 §4.2. The delete job
// is itself a row carrying that instance's id: cascade, and the job deletes itself at the
// moment it succeeds, leaving a delete that appears to have failed.
func TestJobRunsInstanceIDDoesNotCascade(t *testing.T) {
	db := open(t)
	instanceID := seedInstance(t, db, "i1", 2456)

	exec(t, db.Writer, `
		INSERT INTO job_runs (id, kind, status, lock_key, instance_id, instance_name, created_at)
		VALUES (?, 'delete', 'running', ?, ?, ?, ?)`,
		"j1", "instance:"+instanceID, instanceID, "inst-i1", time.Now().UTC())

	exec(t, db.Writer, `DELETE FROM instances WHERE id = ?`, instanceID)

	var (
		gotInstance sql.NullString
		gotName     sql.NullString
	)
	err := db.Reader.QueryRowContext(t.Context(),
		`SELECT instance_id, instance_name FROM job_runs WHERE id = 'j1'`).
		Scan(&gotInstance, &gotName)
	if errors.Is(err, sql.ErrNoRows) {
		t.Fatal("the job row was deleted with its instance; instance_id must be SET NULL, not CASCADE")
	}
	if err != nil {
		t.Fatal(err)
	}
	if gotInstance.Valid {
		t.Errorf("instance_id = %q, want NULL", gotInstance.String)
	}
	if gotName.String != "inst-i1" {
		t.Errorf("instance_name = %q, want it denormalised so history stays readable", gotName.String)
	}
}

// TestChecksumMismatchIsAStartupError covers ADR-024: someone edited applied history.
func TestChecksumMismatchIsAStartupError(t *testing.T) {
	db := open(t)

	exec(t, db.Writer, `UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 1`)

	err := Migrate(t.Context(), db.Writer)
	if !errors.Is(err, ErrChecksumMismatch) {
		t.Fatalf("Migrate error = %v, want ErrChecksumMismatch", err)
	}
}

// TestMigrateIsIdempotent covers a normal restart: the panel migrates on every start.
func TestMigrateIsIdempotent(t *testing.T) {
	db := open(t)
	if err := Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}

	var n int
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Errorf("schema_migrations has %d rows after two runs, want 1", n)
	}
}

// TestMigrationsArePortable covers 10 §4.3's portable-subset rule. Postgres parity is a
// discipline kept from M1, not a driver shipped at M1.
func TestMigrationsArePortable(t *testing.T) {
	migrations, err := Migrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(migrations) == 0 {
		t.Fatal("no migrations were embedded")
	}

	banned := []struct{ token, why string }{
		{"RETURNING", "not in the portable subset (10 §4.3)"},
		{"->>", "dialect-specific JSON operator; JSON columns are filtered in Go"},
		{"AUTOINCREMENT", "ids are TEXT UUIDv7 (06 §4)"},
		{"INTEGER PRIMARY KEY AUTOINCREMENT", "ids are TEXT UUIDv7 (06 §4)"},
	}

	for _, m := range migrations {
		upper := strings.ToUpper(stripSQLComments(m.SQL))
		for _, b := range banned {
			if strings.Contains(upper, strings.ToUpper(b.token)) {
				t.Errorf("migration %d (%s) contains %q: %s", m.Version, m.Name, b.token, b.why)
			}
		}
		// A partial index is CREATE INDEX ... WHERE, which Postgres supports but the
		// portable subset excludes.
		for _, line := range strings.Split(upper, "\n") {
			if strings.HasPrefix(strings.TrimSpace(line), "CREATE INDEX") &&
				strings.Contains(line, " WHERE ") {
				t.Errorf("migration %d has a partial index: %s", m.Version, strings.TrimSpace(line))
			}
		}
	}
}

// stripSQLComments removes -- comments so the portability check reads statements rather
// than prose. Without it, a migration documenting "no RETURNING" trips its own check.
func stripSQLComments(sqlText string) string {
	lines := strings.Split(sqlText, "\n")
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		if before, _, found := strings.Cut(line, "--"); found {
			out = append(out, before)
			continue
		}
		out = append(out, line)
	}
	return strings.Join(out, "\n")
}

// TestStripSQLComments guards the helper above: a checker that silently strips too much
// would pass a migration that genuinely uses a banned construct.
func TestStripSQLComments(t *testing.T) {
	got := stripSQLComments("SELECT 1; -- no RETURNING here\nINSERT INTO t VALUES (2);")
	if strings.Contains(got, "RETURNING") {
		t.Errorf("comment survived stripping: %q", got)
	}
	if !strings.Contains(got, "INSERT INTO t") {
		t.Errorf("statement was stripped: %q", got)
	}
}

// TestStateEnumMatchesTheStateMachine covers 12 §2.1: the enum is short by two in 04 §2,
// and provisioning and deleting are exactly the ambiguity 08 §6.1 needs not to have.
func TestStateEnumMatchesTheStateMachine(t *testing.T) {
	db := open(t)

	states := []string{
		"created", "provisioning", "stopped", "starting", "running", "stopping",
		"backing_up", "restoring", "updating", "deleting", "error",
	}
	for i, state := range states {
		id := "s" + state
		seedInstance(t, db, id, 3000+i*5)
		exec(t, db.Writer, `UPDATE instances SET state = ? WHERE id = ?`, state, id)
	}

	if _, err := db.Writer.ExecContext(t.Context(),
		`UPDATE instances SET state = 'banana' WHERE id = 'screated'`); err == nil {
		t.Error("an unknown state was accepted; the CHECK constraint is missing")
	}
}

// TestAutostartColumnIsGone covers ADR-033. Three behaviours were tangled in it and
// Docker's restart policy already covers reboot.
func TestAutostartColumnIsGone(t *testing.T) {
	db := open(t)
	if _, err := db.Reader.ExecContext(t.Context(), `SELECT autostart FROM instances`); err == nil {
		t.Error("instances.autostart exists; ADR-033 deletes it")
	}
}

// TestReaderPoolRejectsWrites keeps the single-writer discipline of 10 §4.3 enforced by
// the database rather than by convention.
func TestReaderPoolRejectsWrites(t *testing.T) {
	db := open(t)
	_, err := db.Reader.ExecContext(t.Context(),
		`INSERT INTO kv (key, value, updated_at) VALUES ('k', '1', ?)`, time.Now().UTC())
	if err == nil {
		t.Error("the reader pool accepted a write; query_only is not applied")
	}
}

// TestBackupTriggerAllowsPreImport covers the WP-18 world_import snapshot, which reuses
// the shared archive primitive rather than a second snapshot path.
func TestBackupTriggerAllowsPreImport(t *testing.T) {
	db := open(t)
	instanceID := seedInstance(t, db, "i1", 2456)

	exec(t, db.Writer, `
		INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES ('b1', ?, '/srv/x.tar.zst', 1, 'abc', 'World', 'pre_import', TRUE, ?)`,
		instanceID, time.Now().UTC())

	if _, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO backups (id, instance_id, path, size_bytes, sha256, world_name, trigger, consistent, created_at)
		VALUES ('b2', ?, '/srv/y.tar.zst', 1, 'abc', 'World', 'nonsense', TRUE, ?)`,
		instanceID, time.Now().UTC()); err == nil {
		t.Error("an unknown backup trigger was accepted")
	}
}

// TestBasePortIsUniquePerInstance covers INVARIANTS A6: the reservation must be durable
// before a 1 GB download starts.
func TestBasePortIsUniquePerInstance(t *testing.T) {
	db := open(t)
	seedInstance(t, db, "i1", 2456)

	if _, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES ('i2', 'inst-i2', 'created', '/srv/x', 2456, 'S', 'W', 'p', 'cp-i2', ?, ?)`,
		time.Now().UTC(), time.Now().UTC()); err == nil {
		t.Error("two instances took the same base_port")
	}
}

// TestJobLockIsTheDeduplicationMechanism covers 12 §4.3 and INVARIANTS C16: acquire is an
// INSERT, and a primary-key conflict is the "already running" answer.
func TestJobLockIsTheDeduplicationMechanism(t *testing.T) {
	db := open(t)

	exec(t, db.Writer,
		`INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES ('instance:i1', 'j1', ?)`,
		time.Now().UTC())

	if _, err := db.Writer.ExecContext(t.Context(),
		`INSERT INTO job_locks (lock_key, job_id, acquired_at) VALUES ('instance:i1', 'j2', ?)`,
		time.Now().UTC()); err == nil {
		t.Error("a second job took a held lock; double-clicking Start would produce two jobs")
	}
}
