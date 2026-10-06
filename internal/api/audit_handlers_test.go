package api

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

type auditPage struct {
	Items []struct {
		ID         string  `json:"id"`
		UserID     *string `json:"user_id"`
		Actor      *string `json:"actor"`
		InstanceID *string `json:"instance_id"`
		Instance   *string `json:"instance"`
		Action     string  `json:"action"`
		Detail     *string `json:"detail"`
		IP         *string `json:"ip"`
		Outcome    *string `json:"outcome"`
		JobID      *string `json:"job_id"`
		JobError   *string `json:"job_error"`
		CreatedAt  string  `json:"created_at"`
	} `json:"items"`
	NextCursor *string `json:"next_cursor"`
}

func readAudit(t *testing.T, rt *Server, session *httptest.ResponseRecorder, query string) auditPage {
	t.Helper()
	rec := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, http.NoBody), session))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /audit%s = %d (%s)", query, rec.Code, rec.Body)
	}
	var page auditPage
	decodeInto(t, rec, &page)
	return page
}

// auditSeed is one audit_log row written directly, so a test can pin created_at, name an actor
// or instance that does not exist, and set the columns a handler never writes itself. Empty
// optional fields are stored as NULL; Detail and IP default to a harmless value.
type auditSeed struct {
	ID, UserID, InstanceID, Action, CreatedAt string
	Detail, IP                                string
	ActorName, InstanceName                   string
	JobID, Outcome                            string
}

func seedAudit(t *testing.T, db *store.DB, s *auditSeed) {
	t.Helper()
	if s.Detail == "" {
		s.Detail = "{}"
	}
	if s.IP == "" {
		s.IP = "127.0.0.1"
	}
	seed(t, db, `
		INSERT INTO audit_log (
			id, user_id, instance_id, action, detail, ip, created_at,
			actor_name, instance_name, job_id, outcome
		) VALUES (
			?, NULLIF(?, ''), NULLIF(?, ''), ?, ?, ?, ?,
			NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''), NULLIF(?, ''))`,
		s.ID, s.UserID, s.InstanceID, s.Action, s.Detail, s.IP, s.CreatedAt,
		s.ActorName, s.InstanceName, s.JobID, s.Outcome)
}

func seedAuditRow(t *testing.T, db *store.DB, id, userID, instanceID, action, createdAt string) {
	t.Helper()
	seedAudit(t, db, &auditSeed{
		ID: id, UserID: userID, InstanceID: instanceID, Action: action, CreatedAt: createdAt,
	})
}

// memberSession creates a member with no grants and returns their login.
func memberSession(t *testing.T, rt *Server, admin *httptest.ResponseRecorder) *httptest.ResponseRecorder {
	t.Helper()
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin),
	)
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", create.Code, create.Body)
	}
	return loginAs(t, rt, "bea", "another-password")
}

func getAs(rt *Server, session *httptest.ResponseRecorder, path string) *httptest.ResponseRecorder {
	return send(rt, authenticated(httptest.NewRequest(http.MethodGet, path, http.NoBody), session))
}

func auditItemIDs(page auditPage) []string {
	ids := make([]string, 0, len(page.Items))
	for _, item := range page.Items {
		ids = append(ids, item.ID)
	}
	return ids
}

func orNull(s *string) string {
	if s == nil {
		return "<null>"
	}
	return *s
}

func TestAuditIsInvisibleToAMember(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	member := memberSession(t, rt, admin)

	for _, query := range []string{"", "?action=users.create", "?user_id=whoever"} {
		rec := send(
			rt,
			authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, http.NoBody), member),
		)
		if rec.Code != http.StatusNotFound {
			t.Errorf("member GET /audit%s = %d, want 404", query, rec.Code)
		}
	}
}

// TestAuditPagesTiedTimestampsWithoutDuplicates is ADR-035's reason for the compound key:
// rows written in the same burst share created_at, and a page boundary that only ordered by
// time would repeat or skip one.
func TestAuditPagesTiedTimestampsWithoutDuplicates(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	const tied = "2026-01-02T03:04:05.000000000Z"
	for _, id := range []string{"a1", "a2", "a3", "a4", "a5"} {
		seedAuditRow(t, db, id, "", "", "tied.event", tied)
	}

	seen := map[string]bool{}
	query := "?limit=2&action=tied.event"
	for pages := 0; ; pages++ {
		if pages > 5 {
			t.Fatal("pagination did not terminate")
		}
		page := readAudit(t, rt, admin, query)
		for _, item := range page.Items {
			if seen[item.ID] {
				t.Fatalf("row %s appeared on two pages", item.ID)
			}
			seen[item.ID] = true
		}
		if page.NextCursor == nil {
			break
		}
		query = "?limit=2&action=tied.event&cursor=" + *page.NextCursor
	}
	if len(seen) != 5 {
		t.Errorf("paged over %d rows, want 5", len(seen))
	}
}

func TestAuditRejectsMalformedCursorsAndUnknownFilters(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)

	for name, query := range map[string]string{
		"malformed cursor": "?cursor=not-base64!!",
		"unknown filter":   "?detail=password",
		"bad limit":        "?limit=zero",
	} {
		rec := send(
			rt,
			authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit"+query, http.NoBody), admin),
		)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s: %d (%s), want 400", name, rec.Code, rec.Body)
		}
		var envelope struct {
			Error struct {
				Code string `json:"code"`
			} `json:"error"`
		}
		decodeInto(t, rec, &envelope)
		if envelope.Error.Code != "invalid_parameter" {
			t.Errorf("%s: code %q, want invalid_parameter", name, envelope.Error.Code)
		}
	}
}

// TestAuditOutlivesItsSubjects is the point of the table: deleting the user or the instance
// an entry describes leaves the entry readable, with the ids intact and the names null.
func TestAuditOutlivesItsSubjects(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin),
	)
	var created struct {
		ID string `json:"id"`
	}
	decodeInto(t, create, &created)

	seedAuditRow(t, db, "gone", created.ID, "no-such-instance", "instances.delete", store.Now())

	del := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodDelete, "/api/v1/users/"+created.ID, http.NoBody), admin),
	)
	if del.Code != http.StatusNoContent {
		t.Fatalf("delete = %d (%s)", del.Code, del.Body)
	}

	page := readAudit(t, rt, admin, "?action=instances.delete")
	if len(page.Items) != 1 {
		t.Fatalf("audit rows for a deleted user = %d, want 1", len(page.Items))
	}
	row := page.Items[0]
	if row.UserID == nil || *row.UserID != created.ID {
		t.Errorf("user_id = %v, want the deleted user's id", row.UserID)
	}
	if row.Actor != nil {
		t.Errorf("actor = %v, want null once the account is gone", *row.Actor)
	}
	if row.InstanceID == nil || *row.InstanceID != "no-such-instance" {
		t.Errorf("instance_id = %v, want the unresolvable id", row.InstanceID)
	}
	if row.Instance != nil {
		t.Errorf("instance = %v, want null", *row.Instance)
	}
}

// TestAuditNeverCarriesACredential guards 11 §8.3: the audit detail is written from the
// request, and a canary password or invite token must not survive into the trail.
func TestAuditNeverCarriesACredential(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	const canary = "canary-Ac1d-Rain-9182"

	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": canary, "role": "member",
		})), admin),
	)
	if create.Code != http.StatusCreated {
		t.Fatalf("create = %d (%s)", create.Code, create.Body)
	}
	issued := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/invites", jsonBody(t, map[string]any{
			"instance_id": nil, "grant_role": nil, "grant_perms": []string{},
		})), admin),
	)
	if issued.Code != http.StatusCreated && issued.Code != http.StatusOK {
		t.Fatalf("issue invite = %d (%s)", issued.Code, issued.Body)
	}
	var invite struct {
		Token string `json:"token"`
	}
	decodeInto(t, issued, &invite)

	rec := send(rt, authenticated(httptest.NewRequest(http.MethodGet, "/api/v1/audit", http.NoBody), admin))
	if rec.Code != http.StatusOK {
		t.Fatalf("list = %d (%s)", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if strings.Contains(body, canary) {
		t.Error("the audit trail carries a password")
	}
	if invite.Token != "" && strings.Contains(body, invite.Token) {
		t.Error("the audit trail carries an invite token")
	}
	if !strings.Contains(body, "users.create") {
		t.Fatalf("no users.create row in %s", body)
	}
}

func TestAuditBoundsBySinceAndUntil(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	for i, id := range []string{"s1", "s2", "s3"} {
		seedAuditRow(t, db, id, "", "", "bounded.event",
			fmt.Sprintf("2026-02-01T00:00:0%d.000000000Z", i))
	}

	for _, tc := range []struct {
		name  string
		query url.Values
		want  []string
	}{
		{"no bounds", url.Values{}, []string{"s3", "s2", "s1"}},
		{"since is inclusive", url.Values{"since": {"2026-02-01T00:00:01Z"}}, []string{"s3", "s2"}},
		{"until is exclusive", url.Values{"until": {"2026-02-01T00:00:02Z"}}, []string{"s2", "s1"}},
		{
			"both bounds",
			url.Values{"since": {"2026-02-01T00:00:01Z"}, "until": {"2026-02-01T00:00:02Z"}},
			[]string{"s2"},
		},
		{"an offset names the same instant", url.Values{"since": {"2026-02-01T02:00:01+02:00"}}, []string{"s3", "s2"}},
		{"a window with nothing in it", url.Values{"since": {"2027-01-01T00:00:00Z"}}, []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.query.Set("action", "bounded.event")
			page := readAudit(t, rt, admin, "?"+tc.query.Encode())
			if got := auditItemIDs(page); !slices.Equal(got, tc.want) {
				t.Errorf("ids = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestAuditRejectsAnInvalidTimeBound(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)

	for _, path := range []string{"/api/v1/audit", "/api/v1/audit/export"} {
		for _, query := range []string{
			"since=yesterday",
			"until=2026-13-01T00:00:00Z",
			"since=2026-02-01",
			"until=1700000000",
		} {
			rec := getAs(rt, admin, path+"?"+query)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("GET %s?%s = %d (%s), want 400", path, query, rec.Code, rec.Body)
				continue
			}
			if code := errCode(t, rec); code != "invalid_parameter" {
				t.Errorf("GET %s?%s: code %q, want invalid_parameter", path, query, code)
			}
		}
	}
}

func TestAuditOutcomeComesFromTheJobWhenThereIsOne(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	for _, j := range []struct{ id, status, err string }{
		{"j-ok", "succeeded", ""},
		{"j-fail", "failed", "world file missing"},
		{"j-cancel", "cancelled", ""},
		{"j-run", "running", ""},
		{"j-queued", "queued", ""},
	} {
		seedJob(t, db, j.id, "start", j.status, nil)
		if j.err != "" {
			seed(t, db, `UPDATE job_runs SET error = ? WHERE id = ?`, j.err, j.id)
		}
	}

	rows := []struct {
		id, jobID, stored string
		wantOutcome       string
		wantJobError      string
	}{
		{"a-ok", "j-ok", "requested", "succeeded", "<null>"},
		{"a-fail", "j-fail", "requested", "failed", "world file missing"},
		{"a-cancel", "j-cancel", "requested", "cancelled", "<null>"},
		{"a-run", "j-run", "requested", "requested", "<null>"},
		{"a-queued", "j-queued", "requested", "requested", "<null>"},
		{"a-swept-requested", "j-swept", "requested", "requested", "<null>"},
		{"a-swept-succeeded", "j-swept", "succeeded", "succeeded", "<null>"},
		{"a-direct-failed", "", "failed", "failed", "<null>"},
		{"a-direct-unrecorded", "", "", "<null>", "<null>"},
	}
	for i, r := range rows {
		seedAudit(t, db, &auditSeed{
			ID: r.id, Action: "outcome.event", JobID: r.jobID, Outcome: r.stored,
			CreatedAt: fmt.Sprintf("2026-02-01T00:00:%02d.000000000Z", i),
		})
	}

	page := readAudit(t, rt, admin, "?action=outcome.event&limit=100")
	if len(page.Items) != len(rows) {
		t.Fatalf("items = %d, want %d", len(page.Items), len(rows))
	}
	got := map[string]int{}
	for i, item := range page.Items {
		got[item.ID] = i
	}
	for _, r := range rows {
		item := page.Items[got[r.id]]
		if orNull(item.Outcome) != r.wantOutcome || orNull(item.JobError) != r.wantJobError {
			t.Errorf("%s: outcome = %s, job_error = %s; want %s, %s",
				r.id, orNull(item.Outcome), orNull(item.JobError), r.wantOutcome, r.wantJobError)
		}
		wantJob := r.jobID
		if wantJob == "" {
			wantJob = "<null>"
		}
		if orNull(item.JobID) != wantJob {
			t.Errorf("%s: job_id = %s, want %s", r.id, orNull(item.JobID), wantJob)
		}
	}
}

func TestAuditShowsTheNamesRecordedAtWriteTime(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	create := send(
		rt,
		authenticated(httptest.NewRequest(http.MethodPost, "/api/v1/users", jsonBody(t, map[string]string{
			"username": "bea", "password": "another-password", "role": "member",
		})), admin),
	)
	var bea struct {
		ID string `json:"id"`
	}
	decodeInto(t, create, &bea)
	seedTestInstance(t, db, "inst-x", 2456)
	if err := db.WriteAuditLog(t.Context(), &store.AuditEntry{
		UserID: bea.ID, InstanceID: "inst-x", Action: "history.event",
	}); err != nil {
		t.Fatal(err)
	}

	for _, step := range []struct {
		name   string
		change func()
	}{
		{"as written", func() {}},
		{"after a rename", func() {
			seed(t, db, `UPDATE users SET username = 'bea-renamed' WHERE id = ?`, bea.ID)
			seed(t, db, `UPDATE instances SET name = 'inst-renamed' WHERE id = 'inst-x'`)
		}},
		{"after both are deleted", func() {
			seed(t, db, `DELETE FROM users WHERE id = ?`, bea.ID)
			seed(t, db, `DELETE FROM instances WHERE id = 'inst-x'`)
		}},
	} {
		step.change()
		page := readAudit(t, rt, admin, "?action=history.event")
		if len(page.Items) != 1 {
			t.Fatalf("%s: %d rows, want 1", step.name, len(page.Items))
		}
		item := page.Items[0]
		if orNull(item.Actor) != "bea" || orNull(item.Instance) != "inst-x" {
			t.Errorf("%s: actor = %s, instance = %s; want bea, inst-x",
				step.name, orNull(item.Actor), orNull(item.Instance))
		}
		if orNull(item.UserID) != bea.ID || orNull(item.InstanceID) != "inst-x" {
			t.Errorf("%s: ids = %s, %s; want the original ids", step.name, orNull(item.UserID), orNull(item.InstanceID))
		}
	}
}

func TestAuditFiltersListsWhatTheTrailContains(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	seed(t, db, `DELETE FROM audit_log`)

	type subject struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	type filters struct {
		Actions   []string  `json:"actions"`
		Actors    []subject `json:"actors"`
		Instances []subject `json:"instances"`
	}

	empty := getAs(rt, admin, "/api/v1/audit/filters")
	if empty.Code != http.StatusOK {
		t.Fatalf("filters = %d (%s)", empty.Code, empty.Body)
	}
	var raw map[string]json.RawMessage
	decodeInto(t, empty, &raw)
	for _, key := range []string{"actions", "actors", "instances"} {
		if string(raw[key]) != "[]" {
			t.Errorf("%s = %s on an empty trail, want []", key, raw[key])
		}
	}

	seedAudit(t, db, &auditSeed{
		ID: "f1", UserID: "u-gone", ActorName: "gone-user", InstanceID: "i-gone", InstanceName: "Old Server",
		Action: "instances.delete", CreatedAt: "2026-02-01T00:00:00.000000000Z",
	})
	seedAudit(t, db, &auditSeed{
		ID: "f2", UserID: "u-x", ActorName: "ada", Action: "users.create", CreatedAt: "2026-02-01T00:00:01.000000000Z",
	})
	seedAudit(t, db, &auditSeed{ID: "f3", Action: "auth.login", CreatedAt: "2026-02-01T00:00:02.000000000Z"})

	rec := getAs(rt, admin, "/api/v1/audit/filters")
	if rec.Code != http.StatusOK {
		t.Fatalf("filters = %d (%s)", rec.Code, rec.Body)
	}
	var got filters
	decodeInto(t, rec, &got)
	want := filters{
		Actions:   []string{"auth.login", "instances.delete", "users.create"},
		Actors:    []subject{{"u-x", "ada"}, {"u-gone", "gone-user"}},
		Instances: []subject{{"i-gone", "Old Server"}},
	}
	if !slices.Equal(got.Actions, want.Actions) || !slices.Equal(got.Actors, want.Actors) ||
		!slices.Equal(got.Instances, want.Instances) {
		t.Errorf("filters = %+v, want %+v", got, want)
	}
}

func TestAuditFiltersAndExportAreInvisibleToAMember(t *testing.T) {
	rt, _, admin := bootstrappedRouter(t)
	member := memberSession(t, rt, admin)

	for _, path := range []string{
		"/api/v1/audit/filters",
		"/api/v1/audit/export",
		"/api/v1/audit/export?action=users.create",
		"/api/v1/audit/export?since=not-a-time",
	} {
		if rec := getAs(rt, member, path); rec.Code != http.StatusNotFound {
			t.Errorf("member GET %s = %d, want 404", path, rec.Code)
		}
	}
}

func readCSV(t *testing.T, rec *httptest.ResponseRecorder) [][]string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("export = %d (%s)", rec.Code, rec.Body)
	}
	records, err := csv.NewReader(rec.Body).ReadAll()
	if err != nil {
		t.Fatalf("export is not valid CSV: %v", err)
	}
	return records
}

func TestAuditExportIsCSVNewestFirstAndHonoursFilters(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	seedJob(t, db, "j-1", "restore", "failed", nil)
	seedAudit(t, db, &auditSeed{
		ID: "x1", UserID: "u1", ActorName: "ada", InstanceID: "i1", InstanceName: "Alpha",
		Action: "export.event", Outcome: "succeeded", IP: "203.0.113.1", Detail: "d1",
		CreatedAt: "2026-04-01T00:00:00.000000000Z",
	})
	seedAudit(t, db, &auditSeed{
		ID: "x2", UserID: "u2", ActorName: "bea", Action: "export.event", Outcome: "requested",
		JobID: "j-1", Detail: "d2", CreatedAt: "2026-04-01T00:00:01.000000000Z",
	})
	seedAudit(t, db, &auditSeed{
		ID: "x3", UserID: "u1", ActorName: "ada", InstanceID: "i1", InstanceName: "Alpha",
		Action: "other.event", Detail: "d3", CreatedAt: "2026-04-01T00:00:02.000000000Z",
	})

	rec := getAs(rt, admin, "/api/v1/audit/export?action=export.event")
	if got := rec.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/csv", got)
	}
	if got := rec.Header().Get("Content-Disposition"); got != `attachment; filename="audit-log.csv"` {
		t.Errorf("Content-Disposition = %q, want an attachment named audit-log.csv", got)
	}
	records := readCSV(t, rec)
	want := [][]string{
		{"time", "actor", "user_id", "server", "instance_id", "action", "outcome", "job_id", "ip", "detail"},
		{"2026-04-01T00:00:01.000000000Z", "bea", "u2", "", "", "export.event", "failed", "j-1", "127.0.0.1", "d2"},
		{
			"2026-04-01T00:00:00.000000000Z",
			"ada",
			"u1",
			"Alpha",
			"i1",
			"export.event",
			"succeeded",
			"",
			"203.0.113.1",
			"d1",
		},
	}
	if len(records) != len(want) {
		t.Fatalf("export has %d records, want %d: %v", len(records), len(want), records)
	}
	for i := range want {
		if !slices.Equal(records[i], want[i]) {
			t.Errorf("record %d = %q, want %q", i, records[i], want[i])
		}
	}

	for _, tc := range []struct {
		name  string
		query url.Values
		want  []string
	}{
		{"by instance", url.Values{"instance_id": {"i1"}}, []string{"d3", "d1"}},
		{"by user", url.Values{"user_id": {"u2"}}, []string{"d2"}},
		{"since", url.Values{"action": {"export.event"}, "since": {"2026-04-01T00:00:01Z"}}, []string{"d2"}},
		{"until", url.Values{"action": {"export.event"}, "until": {"2026-04-01T00:00:01Z"}}, []string{"d1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			records := readCSV(t, getAs(rt, admin, "/api/v1/audit/export?"+tc.query.Encode()))
			var details []string
			for _, r := range records[1:] {
				details = append(details, r[9])
			}
			if !slices.Equal(details, tc.want) {
				t.Errorf("details = %v, want %v", details, tc.want)
			}
		})
	}
}

func TestAuditExportNeutralisesSpreadsheetFormulas(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	seedAudit(t, db, &auditSeed{
		ID: "danger", ActorName: `=HYPERLINK("http://evil")`, InstanceName: "-2+3", IP: "@host",
		Action: "csv.event", Detail: "+1", CreatedAt: "2026-04-01T00:00:01.000000000Z",
	})
	seedAudit(t, db, &auditSeed{
		ID: "safe", ActorName: "ada", InstanceName: "Alpha", IP: "203.0.113.1",
		Action: "csv.event", Detail: `{"note":"=fine"}`, CreatedAt: "2026-04-01T00:00:00.000000000Z",
	})

	records := readCSV(t, getAs(rt, admin, "/api/v1/audit/export?action=csv.event"))
	if len(records) != 3 {
		t.Fatalf("export has %d records, want 3", len(records))
	}
	const actor, server, ip, detail = 1, 3, 8, 9
	for _, tc := range []struct {
		record, column int
		want           string
	}{
		{1, actor, `'=HYPERLINK("http://evil")`},
		{1, server, "'-2+3"},
		{1, ip, "'@host"},
		{1, detail, "'+1"},
		{2, actor, "ada"},
		{2, server, "Alpha"},
		{2, ip, "203.0.113.1"},
		{2, detail, `{"note":"=fine"}`},
	} {
		if got := records[tc.record][tc.column]; got != tc.want {
			t.Errorf("record %d column %s = %q, want %q", tc.record, records[0][tc.column], got, tc.want)
		}
	}
}

// TestAuditExportSpansEveryBatchInOrder seeds more rows than two export batches, in tie groups
// one row wider than a batch, so every batch boundary falls inside a run of equal timestamps.
func TestAuditExportSpansEveryBatchInOrder(t *testing.T) {
	rt, db, admin := bootstrappedRouter(t)
	const total = 2*exportBatch + 100
	seed(t, db, fmt.Sprintf(`
		WITH RECURSIVE n(i) AS (SELECT 1 UNION ALL SELECT i + 1 FROM n WHERE i < %d)
		INSERT INTO audit_log (id, action, detail, created_at)
		SELECT printf('bulk-%%05d', i), 'bulk.event', printf('%%05d', i),
			strftime('%%Y-%%m-%%dT%%H:%%M:%%S', '2026-05-01 00:00:00', '+' || ((i - 1) / %d) || ' seconds')
				|| '.000000000Z'
		FROM n`, total, exportBatch+1))

	records := readCSV(t, getAs(rt, admin, "/api/v1/audit/export?action=bulk.event"))
	if len(records) != total+1 {
		t.Fatalf("export has %d records, want the header and %d rows", len(records), total)
	}
	for k, record := range records[1:] {
		if want := fmt.Sprintf("%05d", total-k); record[9] != want {
			t.Fatalf("row %d carries detail %s, want %s: a batch repeated or skipped a row", k, record[9], want)
		}
	}
}
