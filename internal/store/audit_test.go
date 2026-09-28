package store

import (
	"database/sql"
	"slices"
	"testing"
	"time"
)

var auditBase = time.Date(2026, 3, 1, 10, 0, 0, 0, time.UTC)

type auditColumns struct {
	UserID, InstanceID, IP, ActorName, InstanceName, JobID, Outcome string
}

// readAuditColumns reads one row's raw columns, spelling SQL NULL as "NULL".
func readAuditColumns(t *testing.T, db *DB, id string) auditColumns {
	t.Helper()
	var user, instance, ip, actor, instanceName, job, outcome sql.NullString
	if err := db.Reader.QueryRowContext(t.Context(), `
		SELECT user_id, instance_id, ip, actor_name, instance_name, job_id, outcome
		FROM audit_log WHERE id = ?`, id,
	).Scan(&user, &instance, &ip, &actor, &instanceName, &job, &outcome); err != nil {
		t.Fatalf("read audit row %s: %v", id, err)
	}
	val := func(ns sql.NullString) string {
		if !ns.Valid {
			return "NULL"
		}
		return ns.String
	}
	return auditColumns{
		UserID: val(user), InstanceID: val(instance), IP: val(ip), ActorName: val(actor),
		InstanceName: val(instanceName), JobID: val(job), Outcome: val(outcome),
	}
}

func writeAuditAt(t *testing.T, db *DB, e *AuditEntry, at time.Time) {
	t.Helper()
	if err := writeAuditLog(t.Context(), db.Writer, e, at); err != nil {
		t.Fatal(err)
	}
}

func auditIDs(rows []AuditRecord) []string {
	ids := make([]string, 0, len(rows))
	for i := range rows {
		ids = append(ids, rows[i].ID)
	}
	return ids
}

func TestWriteAuditLogRecordsNamesAtWriteTime(t *testing.T) {
	for _, tc := range []struct {
		name         string
		entry        AuditEntry
		wantActor    string
		wantInstance string
	}{
		{
			"names are looked up from the live rows",
			AuditEntry{UserID: "u1", InstanceID: "i1"},
			"user-u1", "inst-i1",
		},
		{
			"explicit names win over the live rows",
			AuditEntry{UserID: "u1", InstanceID: "i1", ActorName: "Ada", InstanceName: "Old Server"},
			"Ada", "Old Server",
		},
		{
			"explicit names cover subjects that are already gone",
			AuditEntry{UserID: "nobody", InstanceID: "nowhere", ActorName: "ghost", InstanceName: "lost"},
			"ghost", "lost",
		},
		{
			"unknown subjects record no name",
			AuditEntry{UserID: "nobody", InstanceID: "nowhere"},
			"NULL", "NULL",
		},
		{"an entry without subjects records no name", AuditEntry{}, "NULL", "NULL"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := open(t)
			seedUser(t, db, "u1")
			seedInstance(t, db, "i1", 2456)

			entry := tc.entry
			entry.ID, entry.Action = "e1", "test.action"
			if err := db.WriteAuditLog(t.Context(), &entry); err != nil {
				t.Fatal(err)
			}

			got := readAuditColumns(t, db, "e1")
			if got.ActorName != tc.wantActor || got.InstanceName != tc.wantInstance {
				t.Errorf("actor_name = %q, instance_name = %q; want %q, %q",
					got.ActorName, got.InstanceName, tc.wantActor, tc.wantInstance)
			}
		})
	}
}

func TestAuditNamesSurviveRenamingAndDeletingTheSubjects(t *testing.T) {
	db := open(t)
	seedUser(t, db, "u1")
	seedInstance(t, db, "i1", 2456)
	if err := db.WriteAuditLog(t.Context(), &AuditEntry{
		ID: "e1", UserID: "u1", InstanceID: "i1", Action: "instances.start",
	}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name   string
		change func()
	}{
		{"as written", func() {}},
		{"after a rename", func() {
			exec(t, db.Writer, `UPDATE users SET username = 'renamed' WHERE id = 'u1'`)
			exec(t, db.Writer, `UPDATE instances SET name = 'renamed' WHERE id = 'i1'`)
		}},
		{"after both are deleted", func() {
			exec(t, db.Writer, `DELETE FROM users WHERE id = 'u1'`)
			exec(t, db.Writer, `DELETE FROM instances WHERE id = 'i1'`)
		}},
	} {
		step.change()
		rows, err := db.ListAuditLog(t.Context(), &AuditFilter{}, "", "", 10)
		if err != nil {
			t.Fatal(err)
		}
		if len(rows) != 1 {
			t.Fatalf("%s: %d rows, want 1", step.name, len(rows))
		}
		rec := rows[0]
		if rec.Actor == nil || *rec.Actor != "user-u1" || rec.Instance == nil || *rec.Instance != "inst-i1" {
			t.Errorf("%s: actor = %v, instance = %v; want the names recorded at write time",
				step.name, rec.Actor, rec.Instance)
		}
		if rec.UserID == nil || *rec.UserID != "u1" || rec.InstanceID == nil || *rec.InstanceID != "i1" {
			t.Errorf("%s: ids = %v, %v; want u1, i1", step.name, rec.UserID, rec.InstanceID)
		}
	}
}

func TestListAuditLogFallsBackToLiveNamesForRowsWithoutOne(t *testing.T) {
	db := open(t)
	seedUser(t, db, "u1")
	seedInstance(t, db, "i1", 2456)
	exec(t, db.Writer, `
		INSERT INTO audit_log (id, user_id, instance_id, action, created_at)
		VALUES ('legacy', 'u1', 'i1', 'old.event', ?)`, Now())

	rows, err := db.ListAuditLog(t.Context(), &AuditFilter{}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].Actor == nil || *rows[0].Actor != "user-u1" ||
		rows[0].Instance == nil || *rows[0].Instance != "inst-i1" {
		t.Errorf("rows = %+v, want the current names for a row that recorded none", rows)
	}
}

func TestWriteAuditLogStoresEmptyFieldsAsNull(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry AuditEntry
		want  auditColumns
	}{
		{
			"empty user, instance, ip and job",
			AuditEntry{ID: "e1", Action: "test.action"},
			auditColumns{
				UserID: "NULL", InstanceID: "NULL", IP: "NULL", ActorName: "NULL",
				InstanceName: "NULL", JobID: "NULL", Outcome: "succeeded",
			},
		},
		{
			"every field set",
			AuditEntry{
				ID: "e1", UserID: "u1", InstanceID: "i1", Action: "test.action",
				IP: "203.0.113.9", JobID: "job-1",
			},
			auditColumns{
				UserID: "u1", InstanceID: "i1", IP: "203.0.113.9", ActorName: "user-u1",
				InstanceName: "inst-i1", JobID: "job-1", Outcome: "requested",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := open(t)
			seedUser(t, db, "u1")
			seedInstance(t, db, "i1", 2456)
			entry := tc.entry
			if err := db.WriteAuditLog(t.Context(), &entry); err != nil {
				t.Fatal(err)
			}
			if got := readAuditColumns(t, db, "e1"); got != tc.want {
				t.Errorf("row = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestWriteAuditLogUsesTheCallersIDOrMintsOne(t *testing.T) {
	db := open(t)
	for _, id := range []string{"chosen", ""} {
		if err := db.WriteAuditLog(t.Context(), &AuditEntry{ID: id, Action: "test.action"}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ListAuditLog(t.Context(), &AuditFilter{}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	ids := auditIDs(rows)
	if len(ids) != 2 || !slices.Contains(ids, "chosen") {
		t.Fatalf("ids = %v, want the chosen id and a minted one", ids)
	}
	for _, id := range ids {
		if id == "" {
			t.Error("an entry was stored without an id")
		}
	}
}

func TestWriteAuditLogOutcomeDefaults(t *testing.T) {
	for _, tc := range []struct {
		name  string
		entry AuditEntry
		want  string
	}{
		{"a direct action succeeded", AuditEntry{}, AuditSucceeded},
		{"a job-backed action is requested", AuditEntry{JobID: "job-1"}, AuditRequested},
		{"an explicit outcome is kept", AuditEntry{Outcome: AuditFailed}, AuditFailed},
		{"an explicit outcome wins over the job default", AuditEntry{JobID: "job-1", Outcome: AuditSucceeded}, AuditSucceeded},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := open(t)
			entry := tc.entry
			entry.ID, entry.Action = "e1", "test.action"
			if err := db.WriteAuditLog(t.Context(), &entry); err != nil {
				t.Fatal(err)
			}
			if got := readAuditColumns(t, db, "e1").Outcome; got != tc.want {
				t.Errorf("outcome = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestSetAuditOutcomeUpdatesOnlyThatEntry(t *testing.T) {
	db := open(t)
	for _, id := range []string{"sent", "other"} {
		if err := db.WriteAuditLog(t.Context(), &AuditEntry{
			ID: id, Action: "instances.commands.send", Outcome: AuditRequested,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := db.SetAuditOutcome(t.Context(), "sent", AuditFailed); err != nil {
		t.Fatal(err)
	}

	for id, want := range map[string]string{"sent": AuditFailed, "other": AuditRequested} {
		if got := readAuditColumns(t, db, id).Outcome; got != want {
			t.Errorf("%s outcome = %q, want %q", id, got, want)
		}
	}
}

func TestListAuditLogBoundsBySinceAndUntil(t *testing.T) {
	db := open(t)
	// r0..r3 are one second apart, r0 at auditBase.
	for i, id := range []string{"r0", "r1", "r2", "r3"} {
		writeAuditAt(t, db, &AuditEntry{ID: id, Action: "test.action"}, auditBase.Add(time.Duration(i)*time.Second))
	}
	at := func(seconds int) time.Time { return auditBase.Add(time.Duration(seconds) * time.Second) }

	for _, tc := range []struct {
		name   string
		filter AuditFilter
		want   []string
	}{
		{"no bounds", AuditFilter{}, []string{"r3", "r2", "r1", "r0"}},
		{"since is inclusive", AuditFilter{Since: at(1)}, []string{"r3", "r2", "r1"}},
		{"until is exclusive", AuditFilter{Until: at(3)}, []string{"r2", "r1", "r0"}},
		{"both bounds", AuditFilter{Since: at(1), Until: at(3)}, []string{"r2", "r1"}},
		{"since a nanosecond late drops the row on the bound", AuditFilter{Since: at(1).Add(time.Nanosecond)}, []string{"r3", "r2"}},
		{"until a nanosecond late keeps the row on the bound", AuditFilter{Until: at(3).Add(time.Nanosecond)}, []string{"r3", "r2", "r1", "r0"}},
		{"since equal to until is empty", AuditFilter{Since: at(2), Until: at(2)}, []string{}},
		{"since after every row", AuditFilter{Since: at(10)}, []string{}},
		{"a zone offset is the same instant", AuditFilter{Since: at(1).In(time.FixedZone("plus2", 2*3600))}, []string{"r3", "r2", "r1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rows, err := db.ListAuditLog(t.Context(), &tc.filter, "", "", 10)
			if err != nil {
				t.Fatal(err)
			}
			if got := auditIDs(rows); !slices.Equal(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestListAuditLogPagesTiedTimestampsWithinBounds walks the whole result by keyset cursor:
// rows sharing a second are ordered by id, and a page boundary inside a tie neither repeats
// nor skips a row.
func TestListAuditLogPagesTiedTimestampsWithinBounds(t *testing.T) {
	db := open(t)
	first, second := auditBase, auditBase.Add(time.Second)
	for _, id := range []string{"a", "b", "c"} {
		writeAuditAt(t, db, &AuditEntry{ID: id, Action: "test.action"}, first)
	}
	for _, id := range []string{"d", "e"} {
		writeAuditAt(t, db, &AuditEntry{ID: id, Action: "test.action"}, second)
	}
	writeAuditAt(t, db, &AuditEntry{ID: "z", Action: "test.action"}, second.Add(time.Second))

	for _, tc := range []struct {
		name   string
		filter AuditFilter
		want   []string
	}{
		{"unbounded", AuditFilter{}, []string{"z", "e", "d", "c", "b", "a"}},
		{"since the tie", AuditFilter{Since: first, Until: second.Add(time.Second)}, []string{"e", "d", "c", "b", "a"}},
		{"until the second tie", AuditFilter{Until: second}, []string{"c", "b", "a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []string
			cursorAt, cursorID := "", ""
			for pages := 0; ; pages++ {
				if pages > len(tc.want) {
					t.Fatal("pagination did not terminate")
				}
				rows, err := db.ListAuditLog(t.Context(), &tc.filter, cursorAt, cursorID, 2)
				if err != nil {
					t.Fatal(err)
				}
				if len(rows) == 0 {
					break
				}
				got = append(got, auditIDs(rows)...)
				last := rows[len(rows)-1]
				cursorAt, cursorID = FormatTime(last.CreatedAt), last.ID
			}
			if !slices.Equal(got, tc.want) {
				t.Errorf("paged ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestListAuditLogReportsTheJobBehindAnEntry(t *testing.T) {
	db := open(t)
	until := time.Now().Add(time.Minute)

	failed := newJob(NewID(), "start", "instance:failed")
	running := newJob(NewID(), "stop", "instance:running")
	for _, j := range []*Job{failed, running} {
		if err := db.ClaimJob(t.Context(), j, "panel:boot-a", until, nil); err != nil {
			t.Fatal(err)
		}
	}
	message := "world file missing"
	if err := db.FinishJob(
		t.Context(), failed.ID, "failed", 0, nil, &message, nil, nil, time.Now(), nil,
	); err != nil {
		t.Fatal(err)
	}

	for _, e := range []AuditEntry{
		{ID: "e-failed", Action: "instances.start", JobID: failed.ID},
		{ID: "e-running", Action: "instances.stop", JobID: running.ID},
		{ID: "e-swept", Action: "instances.restart", JobID: "swept-job"},
		{ID: "e-direct", Action: "instances.settings.update"},
	} {
		if err := db.WriteAuditLog(t.Context(), &e); err != nil {
			t.Fatal(err)
		}
	}

	rows, err := db.ListAuditLog(t.Context(), &AuditFilter{}, "", "", 10)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]AuditRecord{}
	for _, rec := range rows {
		byID[rec.ID] = rec
	}

	for _, tc := range []struct {
		id         string
		wantJob    string
		wantStatus string
		wantError  string
		wantStored string
	}{
		{"e-failed", failed.ID, "failed", message, AuditRequested},
		{"e-running", running.ID, "running", "", AuditRequested},
		{"e-swept", "swept-job", "", "", AuditRequested},
		{"e-direct", "", "", "", AuditSucceeded},
	} {
		rec := byID[tc.id]
		text := func(s *string) string {
			if s == nil {
				return ""
			}
			return *s
		}
		if text(rec.JobID) != tc.wantJob || text(rec.JobStatus) != tc.wantStatus ||
			text(rec.JobError) != tc.wantError || text(rec.Outcome) != tc.wantStored {
			t.Errorf("%s: job = %q status = %q error = %q outcome = %q; want %q %q %q %q", tc.id,
				text(rec.JobID), text(rec.JobStatus), text(rec.JobError), text(rec.Outcome),
				tc.wantJob, tc.wantStatus, tc.wantError, tc.wantStored)
		}
	}
	if byID["e-direct"].JobID != nil || byID["e-swept"].JobStatus != nil {
		t.Error("a direct entry must have no job id, and a swept job must have no status")
	}
}

func TestAuditFacetsListEverySubjectWithItsLastRecordedName(t *testing.T) {
	db := open(t)
	for _, id := range []string{"u1", "u2", "u3"} {
		seedUser(t, db, id)
	}
	for i, id := range []string{"i1", "i2", "i3"} {
		seedInstance(t, db, id, 2456+i*5)
	}
	step := func(seconds int) time.Time { return auditBase.Add(time.Duration(seconds) * time.Second) }

	// u1 and i1 are renamed between two entries, then renamed again with no entry: the last
	// recorded name is the one that counts.
	writeAuditAt(t, db, &AuditEntry{UserID: "u1", InstanceID: "i1", Action: "instances.start"}, step(0))
	exec(t, db.Writer, `UPDATE users SET username = 'ada' WHERE id = 'u1'`)
	exec(t, db.Writer, `UPDATE instances SET name = 'alpha' WHERE id = 'i1'`)
	writeAuditAt(t, db, &AuditEntry{UserID: "u1", InstanceID: "i1", Action: "instances.stop"}, step(1))
	exec(t, db.Writer, `UPDATE users SET username = 'ada-live' WHERE id = 'u1'`)
	exec(t, db.Writer, `UPDATE instances SET name = 'alpha-live' WHERE id = 'i1'`)

	// u2 and i2 are deleted; a later entry that could not look up a name keeps the earlier one.
	writeAuditAt(t, db, &AuditEntry{UserID: "u2", InstanceID: "i2", Action: "instances.delete"}, step(2))
	exec(t, db.Writer, `DELETE FROM users WHERE id = 'u2'`)
	exec(t, db.Writer, `DELETE FROM instances WHERE id = 'i2'`)
	writeAuditAt(t, db, &AuditEntry{UserID: "u2", InstanceID: "i2", Action: "jobs.cancel"}, step(3))

	writeAuditAt(t, db, &AuditEntry{Action: "users.login"}, step(4))

	// Rows that recorded no name: one whose subjects still exist, one whose subjects are gone.
	exec(t, db.Writer, `
		INSERT INTO audit_log (id, user_id, instance_id, action, created_at) VALUES
			('live', 'u3', 'i3', 'legacy.event', ?),
			('orphan', 'gone-user', 'gone-instance', 'legacy.event', ?)`,
		FormatTime(step(5)), FormatTime(step(6)))

	got, err := db.AuditFacets(t.Context())
	if err != nil {
		t.Fatal(err)
	}

	wantActions := []string{
		"instances.delete", "instances.start", "instances.stop", "jobs.cancel", "legacy.event", "users.login",
	}
	if !slices.Equal(got.Actions, wantActions) {
		t.Errorf("actions = %v, want %v", got.Actions, wantActions)
	}
	wantActors := []AuditSubject{
		{"gone-user", ""}, {"u1", "ada"}, {"u2", "user-u2"}, {"u3", "user-u3"},
	}
	if !slices.Equal(got.Actors, wantActors) {
		t.Errorf("actors = %v, want %v", got.Actors, wantActors)
	}
	wantInstances := []AuditSubject{
		{"gone-instance", ""}, {"i1", "alpha"}, {"i2", "inst-i2"}, {"i3", "inst-i3"},
	}
	if !slices.Equal(got.Instances, wantInstances) {
		t.Errorf("instances = %v, want %v", got.Instances, wantInstances)
	}
}
