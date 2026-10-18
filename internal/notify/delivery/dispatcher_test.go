package delivery

import (
	"context"
	"io"
	"net/http"
	"net/netip"
	"path"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/notify"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/store/storetest"
)

// fakeDiscord answers like a Discord webhook: a post creates a message and answers with its id,
// and an edit succeeds for a message it holds and is 404 for any other.
type fakeDiscord struct {
	mu       sync.Mutex
	requests []string
	messages map[string]bool
}

func (f *fakeDiscord) sender() *notify.Sender {
	return &notify.Sender{
		Lookup: func(context.Context, string) ([]netip.Addr, error) {
			return []netip.Addr{netip.MustParseAddr("203.0.113.5")}, nil
		},
		Client: func(notify.Target) *http.Client { return &http.Client{Transport: f} },
	}
}

func (f *fakeDiscord) RoundTrip(r *http.Request) (*http.Response, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path+"?"+r.URL.RawQuery)
	status, body := http.StatusOK, ""
	switch {
	case r.Method == http.MethodPost:
		id := strconv.Itoa(1000 + len(f.messages))
		f.messages[id] = true
		body = `{"id":"` + id + `","channel_id":"5"}`
	case !f.messages[path.Base(r.URL.Path)]:
		status = http.StatusNotFound
	}
	return &http.Response{
		StatusCode: status, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}, Request: r,
	}, nil
}

// TestAResolutionEditsTheMessageOfTheAlertItResolves asserts a Discord delivery is posted with
// its message id recorded, a delivery that edits it patches that message, and an edit whose
// message is gone or unknown is posted as a new message instead.
func TestAResolutionEditsTheMessageOfTheAlertItResolves(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	hook := &store.Webhook{ID: store.NewID(), Name: "ops", Kind: notify.KindDiscord, URL: "sealed", Enabled: true}
	if err := db.CreateWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	discord := &fakeDiscord{messages: map[string]bool{}}
	h := &Dispatcher{DB: db, Sender: discord.sender}
	const raw = "https://discord.com/api/webhooks/1/tok"

	record := func(edits *store.Delivery) *store.Delivery {
		t.Helper()
		d := &store.Delivery{
			ID: store.NewID(), WebhookID: hook.ID, EventID: store.NewID(), EventKind: "alert_opened",
			Payload: `{"embeds":[]}`,
		}
		if edits != nil {
			d.EditsID = &edits.ID
		}
		if err := db.CreateDelivery(ctx, d); err != nil {
			t.Fatal(err)
		}
		return d
	}
	deliver := func(d *store.Delivery) int {
		t.Helper()
		attempts, err := h.deliver(ctx, d, notify.KindDiscord, raw)
		if err != nil {
			t.Fatalf("deliver: %v", err)
		}
		return attempts
	}

	opened := record(nil)
	deliver(opened)
	stored, err := db.DeliveryByID(ctx, opened.ID)
	if err != nil || stored.RemoteID == nil || *stored.RemoteID != "1000" {
		t.Fatalf("opened delivery = %+v, %v, want message 1000 recorded", stored, err)
	}

	deliver(record(opened))
	delete(discord.messages, "1000")
	if attempts := deliver(record(opened)); attempts != 2 {
		t.Errorf("attempts = %d, want the refused edit and the post", attempts)
	}
	deliver(record(record(nil)))

	want := []string{
		"POST /api/webhooks/1/tok?wait=true",
		"PATCH /api/webhooks/1/tok/messages/1000?",
		"PATCH /api/webhooks/1/tok/messages/1000?",
		"POST /api/webhooks/1/tok?wait=true",
		"POST /api/webhooks/1/tok?wait=true",
	}
	if !slices.Equal(discord.requests, want) {
		t.Errorf("requests = %q, want %q", discord.requests, want)
	}
}

// TestAPausedDestinationReceivesNothingQueued asserts a delivery written before its destination
// was paused fails without a request.
func TestAPausedDestinationReceivesNothingQueued(t *testing.T) {
	t.Parallel()
	ctx := t.Context()
	db := storetest.Open(t)
	hook := &store.Webhook{ID: store.NewID(), Name: "ops", Kind: notify.KindDiscord, URL: "sealed"}
	if err := db.CreateWebhook(ctx, hook); err != nil {
		t.Fatal(err)
	}
	d := &store.Delivery{
		ID: store.NewID(), WebhookID: hook.ID, EventID: store.NewID(), EventKind: "alert_opened",
		Payload: `{"embeds":[]}`,
	}
	if err := db.CreateDelivery(ctx, d); err != nil {
		t.Fatal(err)
	}
	discord := &fakeDiscord{messages: map[string]bool{}}
	h := &Dispatcher{DB: db, Sender: discord.sender}

	if out := h.RunDelivery(d.ID, hook.ID)(ctx, nil); out.Status != jobs.StatusFailed {
		t.Errorf("outcome = %+v, want failed", out)
	}
	stored, err := db.DeliveryByID(ctx, d.ID)
	if err != nil || stored.Status != store.DeliveryFailed {
		t.Errorf("delivery = %+v, %v, want failed", stored, err)
	}
	if len(discord.requests) != 0 {
		t.Errorf("requests = %q, want none", discord.requests)
	}
}
