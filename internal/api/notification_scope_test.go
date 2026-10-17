package api

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/valminhq/valmin/internal/alerts"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

// scopeWorld is two servers, inst-a and inst-b, with the member an operator holding
// notifications.manage on inst-a, and three webhooks: a shared one, inst-a's own and inst-b's.
func scopeWorld(t *testing.T) scopeFixture {
	t.Helper()
	rt, db, admin, member := provisionWorld(t)
	for i, id := range []string{"inst-a", "inst-b"} {
		seed(t, db, `INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, 'stopped', ?, ?, ?, ?, 'sealed', ?, ?, ?)`,
			id, id, "/srv/valmin/instances/"+id, 2456+i*5, id, id, "cp-"+id, store.Now(), store.Now())
	}
	seed(t, db, `INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES ('u-member', 'inst-a', 'operator', '["notifications.manage"]', ?)`, store.Now())
	shared := seedWebhook(t, db, "shared")
	own := seedWebhook(t, db, "a-own")
	other := seedWebhook(t, db, "b-own")
	seed(t, db, `UPDATE webhooks SET instance_id = 'inst-a' WHERE id = ?`, own)
	seed(t, db, `UPDATE webhooks SET instance_id = 'inst-b' WHERE id = ?`, other)
	return scopeFixture{rt, db, admin, member, shared, own, other}
}

// scopeFixture is what scopeWorld seeds.
type scopeFixture struct {
	rt                 *Server
	db                 *store.DB
	admin, member      *store.User
	shared, own, other string
}

func webhookNames(t *testing.T, rt *Server, u *store.User, path string) []string {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, path, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
	}
	var page Page[webhookView]
	decodeInto(t, rec, &page)
	names := make([]string, 0, len(page.Items))
	for _, w := range page.Items {
		names = append(names, w.Name)
	}
	slices.Sort(names)
	return names
}

// TestAServerSeesItsOwnAndTheSharedWebhooks asserts a server's list leaves out every other
// server's webhooks, while the admin list holds them all.
func TestAServerSeesItsOwnAndTheSharedWebhooks(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, admin, member := f.rt, f.admin, f.member

	if got, want := webhookNames(t, rt, member, "/api/v1/instances/inst-a/webhooks"),
		[]string{"a-own", "shared"}; !slices.Equal(got, want) {
		t.Errorf("server list = %v, want %v", got, want)
	}
	if got, want := webhookNames(t, rt, admin, webhooksPath),
		[]string{"a-own", "b-own", "shared"}; !slices.Equal(got, want) {
		t.Errorf("admin list = %v, want %v", got, want)
	}
}

// TestAServerChangesOnlyItsOwnWebhooks asserts a shared or another server's webhook reads as
// absent on a server's routes.
func TestAServerChangesOnlyItsOwnWebhooks(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, member, shared, own, other := f.rt, f.member, f.shared, f.own, f.other
	const base = "/api/v1/instances/inst-a/webhooks/"

	for _, tc := range []struct {
		name, method, path, body string
		want                     int
	}{
		{"rename own", http.MethodPatch, base + own, `{"name":"renamed"}`, http.StatusOK},
		{"pause shared", http.MethodPatch, base + shared, `{"enabled":false}`, http.StatusNotFound},
		{"pause other", http.MethodPatch, base + other, `{"enabled":false}`, http.StatusNotFound},
		{"test shared", http.MethodPost, base + shared + "/test", "", http.StatusNotFound},
		{"delete other", http.MethodDelete, base + other, "", http.StatusNotFound},
		{"delete shared", http.MethodDelete, base + shared, "", http.StatusNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if rec := as(rt, member, jsonReq(tc.method, tc.path, tc.body)); rec.Code != tc.want {
				t.Errorf("%s %s = %d %s, want %d", tc.method, tc.path, rec.Code, rec.Body, tc.want)
			}
		})
	}
}

// TestAWebhookAddedOnAServerBelongsToIt asserts ownership, the catch-all default, and that a
// name is unique per server rather than across the panel.
func TestAWebhookAddedOnAServerBelongsToIt(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, member := f.rt, f.member
	recordingReceiver(t, rt, http.StatusNoContent)
	const path = "/api/v1/instances/inst-a/webhooks"
	add := func(name string) *httptest.ResponseRecorder {
		return as(rt, member, jsonReq(http.MethodPost, path,
			`{"name":"`+name+`","kind":"discord","url":"https://example.com/api/webhooks/1/x"}`))
	}

	rec := add("shared")
	if rec.Code != http.StatusCreated {
		t.Fatalf("add a name the shared list uses: %d %s", rec.Code, rec.Body)
	}
	var view webhookView
	decodeInto(t, rec, &view)
	if view.InstanceID == nil || *view.InstanceID != "inst-a" || !view.CatchAll {
		t.Errorf("created = %+v, want inst-a's own with catch-all on", view)
	}
	if rec := add("a-own"); rec.Code != http.StatusConflict {
		t.Errorf("add a name inst-a already uses = %d %s, want 409", rec.Code, rec.Body)
	}
}

// TestARuleNamesOnlyWebhooksThatServeIt asserts a rule about a server may use the shared
// webhooks and that server's own, a panel-wide rule only the shared ones, and that a server's
// routes make rules about that server alone.
func TestARuleNamesOnlyWebhooksThatServeIt(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, admin, member, shared, own, other := f.rt, f.admin, f.member, f.shared, f.own, f.other
	const server = "/api/v1/instances/inst-a/alert-rules"

	for _, tc := range []struct {
		name string
		u    *store.User
		path string
		body string
		want int
	}{
		{"own on server", member, server, `{"condition_kind":"player_joined","webhook_ids":["` + own + `"]}`, http.StatusCreated},
		{"shared on server", member, server, `{"condition_kind":"player_joined","webhook_ids":["` + shared + `"]}`, http.StatusCreated},
		{"other on server", member, server, `{"condition_kind":"player_joined","webhook_ids":["` + other + `"]}`, http.StatusUnprocessableEntity},
		{"another server's rule", member, server, `{"condition_kind":"player_joined","instance_id":"inst-b"}`, http.StatusUnprocessableEntity},
		{"panel-wide rule", member, server, `{"condition_kind":"player_joined","instance_id":""}`, http.StatusUnprocessableEntity},
		{"host-wide kind", member, server, `{"condition_kind":"low_disk"}`, http.StatusUnprocessableEntity},
		{"server webhook on panel-wide", admin, alertRulesPath, `{"condition_kind":"crash_loop","webhook_ids":["` + own + `"]}`, http.StatusUnprocessableEntity},
		{"server webhook on its server", admin, alertRulesPath, `{"condition_kind":"crash_loop","instance_id":"inst-a","webhook_ids":["` + own + `"]}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := as(rt, tc.u, jsonReq(http.MethodPost, tc.path, tc.body))
			if rec.Code != tc.want {
				t.Fatalf("POST %s %s = %d %s, want %d", tc.path, tc.body, rec.Code, rec.Body, tc.want)
			}
			if tc.u == member && rec.Code == http.StatusCreated {
				var rule alertRuleView
				decodeInto(t, rec, &rule)
				if rule.InstanceID == nil || *rule.InstanceID != "inst-a" {
					t.Errorf("rule made on inst-a's routes is about %v", rule.InstanceID)
				}
			}
		})
	}
}

// TestAServerChangesOnlyItsOwnRules asserts a server lists its own and the panel-wide rules,
// never another server's, and changes only its own.
func TestAServerChangesOnlyItsOwnRules(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, db, member, shared := f.rt, f.db, f.member, f.shared
	panelWide := seedRule(t, db, alerts.KindCrashLoop, "{}", shared)
	otherServer := seedRule(t, db, alerts.KindCrashLoop, "{}")
	seed(t, db, `UPDATE alert_rules SET instance_id = 'inst-b' WHERE id = ?`, otherServer)
	const base = "/api/v1/instances/inst-a/alert-rules"
	created := as(rt, member, jsonReq(http.MethodPost, base, `{"condition_kind":"player_left"}`))
	var own alertRuleView
	decodeInto(t, created, &own)

	rec := as(rt, member, httptest.NewRequest(http.MethodGet, base, http.NoBody))
	var page Page[alertRuleView]
	decodeInto(t, rec, &page)
	listed := make([]string, 0, len(page.Items))
	for _, r := range page.Items {
		listed = append(listed, r.ID)
	}
	if !slices.Contains(listed, panelWide) || !slices.Contains(listed, own.ID) ||
		slices.Contains(listed, otherServer) {
		t.Errorf("listed %v, want the panel-wide %s and own %s, not %s", listed, panelWide, own.ID, otherServer)
	}

	for id, want := range map[string]int{
		own.ID: http.StatusOK, panelWide: http.StatusNotFound, otherServer: http.StatusNotFound,
	} {
		if rec := as(rt, member, jsonReq(http.MethodPatch, base+"/"+id, `{"enabled":false}`)); rec.Code != want {
			t.Errorf("PATCH rule %s = %d %s, want %d", id, rec.Code, rec.Body, want)
		}
	}
}

// TestAServerListsOnlyDeliveriesAboutIt asserts a server's delivery list leaves out every
// delivery about another server or none.
func TestAServerListsOnlyDeliveriesAboutIt(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, db, member, shared := f.rt, f.db, f.member, f.shared
	mine := seedDelivery(t, db, shared, store.DeliveryDelivered, nil)
	theirs := seedDelivery(t, db, shared, store.DeliveryDelivered, nil)
	seedDelivery(t, db, shared, store.DeliveryDelivered, nil)
	seed(t, db, `UPDATE webhook_deliveries SET instance_id = 'inst-a' WHERE id = ?`, mine)
	seed(t, db, `UPDATE webhook_deliveries SET instance_id = 'inst-b' WHERE id = ?`, theirs)

	rec := as(rt, member, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/webhooks/deliveries", http.NoBody))
	var page Page[deliveryView]
	decodeInto(t, rec, &page)
	if got := deliveryIDs(page.Items); !slices.Equal(got, []string{mine}) {
		t.Errorf("deliveries = %v, want only %s", got, mine)
	}
}

// TestARuleTestSendsItsMessageOverTheSampleValues asserts the test send is the rule's own
// message under the test kind, and that it only reaches a webhook the rule could use.
func TestARuleTestSendsItsMessageOverTheSampleValues(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, member, own, other := f.rt, f.member, f.own, f.other
	hits, sent := recordingReceiver(t, rt, http.StatusNoContent)
	const path = "/api/v1/instances/inst-a/alert-rules/test"
	body := func(webhookID string) string {
		return `{"condition_kind":"player_joined","params":{"template":"{player} came in"},` +
			`"webhook_id":"` + webhookID + `","fields":{"player":"Bob"}}`
	}

	if rec := as(rt, member, jsonReq(http.MethodPost, path, body(other))); rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("test on another server's webhook = %d %s, want 422", rec.Code, rec.Body)
	}
	rec := as(rt, member, jsonReq(http.MethodPost, path, body(own)))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test send: %d %s", rec.Code, rec.Body)
	}
	var submitted jobView
	decodeInto(t, rec, &submitted)
	if done := waitJob(t, rt, member, submitted.JobID); done.Status != "succeeded" {
		t.Fatalf("delivery %s: %+v", done.Status, done)
	}
	if hits.Load() != 1 || !bytes.Contains(*sent, []byte(`Bob came in`)) {
		t.Errorf("sent %d: %s, want the rule's message over the sample", hits.Load(), *sent)
	}
	if !bytes.Contains(*sent, []byte(notify.KindTest.String())) {
		t.Errorf("sent %s, want the test kind", *sent)
	}
}

// TestBuiltInAlertsReachOnlyCatchAllWebhooksThatServeTheServer asserts a failed backup on one
// server reaches the shared webhooks and that server's own, never another server's, and none
// whose catch-all is off.
func TestBuiltInAlertsReachOnlyCatchAllWebhooksThatServeTheServer(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, db, shared, own, other := f.rt, f.db, f.shared, f.own, f.other
	recordingReceiver(t, rt, http.StatusNoContent)
	quiet := seedWebhook(t, db, "no-catch-all")
	seed(t, db, `UPDATE webhooks SET catch_all = FALSE WHERE id = ?`, quiet)
	instanceID := "inst-b"

	finishJob(t, rt, db, &jobs.FinishedJob{
		ID: store.NewID(), Kind: jobs.KindBackup, InstanceID: &instanceID,
		InstanceName: "inst-b", Status: jobs.StatusFailed,
	})
	sent := deliveriesOfKind(t, db, notify.KindBackupFailed)
	got := make([]string, 0, len(sent))
	for _, d := range sent {
		got = append(got, d.WebhookID)
	}
	want := []string{shared, other}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Errorf("delivered to %v, want the shared %s and inst-b's %s; not inst-a's %s or %s",
			got, shared, other, own, quiet)
	}
}

// TestAServerRuleNeverWithholdsBuiltInAlertsFromASharedWebhook asserts that a rule about one
// server takes only that server's own webhooks out of the built-in alerts. A shared webhook it
// names still gets them, so a server's rule cannot keep its failures from the shared channel,
// even inside its quiet hours.
func TestAServerRuleNeverWithholdsBuiltInAlertsFromASharedWebhook(t *testing.T) {
	t.Parallel()
	f := scopeWorld(t)
	rt, db, shared, own := f.rt, f.db, f.shared, f.own
	recordingReceiver(t, rt, http.StatusNoContent)
	rule := seedRule(t, db, alerts.KindJobFailed, "", shared, own)
	seed(t, db, `UPDATE alert_rules SET instance_id = 'inst-a', quiet_start = 0,
		quiet_end = 1439, quiet_tz = 'UTC' WHERE id = ?`, rule)

	failJob(t, rt, db, jobs.KindBackup, "inst-a")

	assertDeliveries(t, db, "the shared webhook", shared,
		map[string]int{notify.KindBackupFailed.String(): 1})
	assertDeliveries(t, db, "inst-a's own webhook, which the rule owns", own, map[string]int{})
}
