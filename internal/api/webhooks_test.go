package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
)

const webhooksPath = "/api/v1/admin/webhooks"

// recordingReceiver replaces the panel's outbound client with one that answers from memory,
// and reports what it was asked to send. The address policy is left alone: the destination
// still has to pass it.
func recordingReceiver(t *testing.T, rt *Router, status int) (hits *atomic.Int32, sent *[]byte) {
	t.Helper()
	var requests atomic.Int32
	var body []byte
	rt.webhooks.Sender = &notify.Sender{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.5")}, nil
		},
		Client: func(notify.Target) *http.Client {
			return &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				requests.Add(1)
				body, _ = io.ReadAll(r.Body)
				return &http.Response{
					StatusCode: status,
					Body:       io.NopCloser(strings.NewReader("")),
					Header:     http.Header{},
					Request:    r,
				}, nil
			})}
		},
	}
	return &requests, &body
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func createWebhook(t *testing.T, rt *Router, u *store.User, body string) webhookView {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, webhooksPath, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := as(rt, u, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create webhook: %d %s", rec.Code, rec.Body)
	}
	var view webhookView
	decodeInto(t, rec, &view)
	return view
}

// TestADestinationURLNeverLeavesThePanel asserts 11 §9 on the one field that is a bearer
// credential: it is stored in the envelope of 10 §3.2 and appears in no response.
func TestADestinationURLNeverLeavesThePanel(t *testing.T) {
	rt, db, admin, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)
	const secret = "https://example.com/api/webhooks/1/s3cr3t"

	created := createWebhook(t, rt, admin,
		`{"name":"Discord","kind":"discord","url":"`+secret+`"}`)

	var stored string
	if err := db.Reader.QueryRowContext(t.Context(),
		`SELECT url FROM webhooks WHERE id = ?`, created.ID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(stored, "v1.") || strings.Contains(stored, "s3cr3t") {
		t.Errorf("stored url = %q, want the AEAD envelope", stored)
	}

	rec := as(rt, admin, httptest.NewRequest(http.MethodGet, webhooksPath, http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list: %d %s", rec.Code, rec.Body)
	}
	if strings.Contains(rec.Body.String(), "s3cr3t") || strings.Contains(rec.Body.String(), "url") {
		t.Errorf("list response carries the destination URL: %s", rec.Body)
	}
}

// TestADestinationTheAddressPolicyRefusesIsAFieldError asserts that the SSRF policy is applied
// where the operator can act on it (ADR-167): a private or non-HTTPS destination is a 422 on
// the url field, not a delivery that fails later.
func TestADestinationTheAddressPolicyRefusesIsAFieldError(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusNoContent)

	for _, url := range []string{
		"http://example.com/hook",
		"https://127.0.0.1/hook",
		"https://169.254.169.254/latest/meta-data/",
		"https://user:pw@example.com/hook",
		"https://example.com:8443/hook",
	} {
		req := httptest.NewRequest(http.MethodPost, webhooksPath,
			strings.NewReader(`{"name":"n-`+url+`","kind":"generic","url":"`+url+`"}`))
		req.Header.Set("Content-Type", "application/json")
		rec := as(rt, admin, req)
		if rec.Code != http.StatusUnprocessableEntity {
			t.Errorf("create with %s = %d, want 422 (%s)", url, rec.Code, rec.Body)
		}
	}
}

// TestATestSendDeliversThroughTheRealPath asserts that the test button exercises the delivery
// the panel would make, and that the delivery row records the outcome.
func TestATestSendDeliversThroughTheRealPath(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	hits, sent := recordingReceiver(t, rt, http.StatusNoContent)
	created := createWebhook(t, rt, admin,
		`{"name":"Discord","kind":"discord","url":"https://example.com/api/webhooks/1/t"}`)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, webhooksPath+"/"+created.ID+"/test", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test send: %d %s", rec.Code, rec.Body)
	}
	var submitted jobView
	decodeInto(t, rec, &submitted)
	if done := waitJob(t, rt, admin, submitted.JobID); done.Status != "succeeded" {
		t.Fatalf("delivery %s: %+v", done.Status, done)
	}

	if hits.Load() != 1 {
		t.Errorf("requests = %d, want 1", hits.Load())
	}
	if !bytes.Contains(*sent, []byte("Test notification")) {
		t.Errorf("sent body = %s, want the test event", *sent)
	}

	deliveries := listDeliveries(t, rt, admin)
	if len(deliveries) != 1 {
		t.Fatalf("deliveries = %d, want 1", len(deliveries))
	}
	d := deliveries[0]
	if d.Status != store.DeliveryDelivered || d.Attempts != 1 || d.WebhookID != created.ID {
		t.Errorf("delivery = %+v, want one delivered attempt against the destination", d)
	}
	if d.EventKind != notify.KindTest.String() {
		t.Errorf("event kind = %q, want %q", d.EventKind, notify.KindTest)
	}
}

// TestAnExhaustedDeliveryStaysInspectable asserts that a destination that refuses the send
// leaves a readable record rather than only a log line, and that the record does not quote the
// URL it failed to reach.
func TestAnExhaustedDeliveryStaysInspectable(t *testing.T) {
	rt, _, admin, _ := provisionWorld(t)
	recordingReceiver(t, rt, http.StatusUnauthorized)
	created := createWebhook(t, rt, admin,
		`{"name":"Gone","kind":"generic","url":"https://example.com/api/webhooks/1/s3cr3t"}`)

	rec := as(rt, admin, httptest.NewRequest(
		http.MethodPost, webhooksPath+"/"+created.ID+"/test", http.NoBody))
	if rec.Code != http.StatusAccepted {
		t.Fatalf("test send: %d %s", rec.Code, rec.Body)
	}
	var submitted jobView
	decodeInto(t, rec, &submitted)
	if done := waitJob(t, rt, admin, submitted.JobID); done.Status != "failed" {
		t.Fatalf("delivery %s, want failed", done.Status)
	}

	deliveries := listDeliveries(t, rt, admin)
	if len(deliveries) != 1 || deliveries[0].Status != store.DeliveryFailed {
		t.Fatalf("deliveries = %+v, want one failed row", deliveries)
	}
	if deliveries[0].LastError == nil {
		t.Fatal("a failed delivery records no reason")
	}
	if strings.Contains(*deliveries[0].LastError, "s3cr3t") {
		t.Errorf("recorded failure quotes the destination URL: %s", *deliveries[0].LastError)
	}
}

// TestWebhookAdministrationIsInvisibleToAMember asserts 09 §3.3's never-grantable rule with
// ADR-038's answer: configuring a destination is a request the panel will make on the
// operator's behalf, so a member cannot see that the group exists.
func TestWebhookAdministrationIsInvisibleToAMember(t *testing.T) {
	rt, _, _, member := provisionWorld(t)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, webhooksPath, http.NoBody),
		httptest.NewRequest(http.MethodPost, webhooksPath, strings.NewReader(`{}`)),
		httptest.NewRequest(http.MethodGet, webhooksPath+"/deliveries", http.NoBody),
	} {
		req.Header.Set("Content-Type", "application/json")
		if rec := as(rt, member, req); rec.Code != http.StatusNotFound {
			t.Errorf("%s %s = %d, want 404", req.Method, req.URL.Path, rec.Code)
		}
	}
}

func listDeliveries(t *testing.T, rt *Router, u *store.User) []deliveryView {
	t.Helper()
	rec := as(rt, u, httptest.NewRequest(http.MethodGet, webhooksPath+"/deliveries", http.NoBody))
	if rec.Code != http.StatusOK {
		t.Fatalf("list deliveries: %d %s", rec.Code, rec.Body)
	}
	var page Page[deliveryView]
	if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	return page.Items
}
