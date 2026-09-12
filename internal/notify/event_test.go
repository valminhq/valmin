package notify

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func testEvent() *Event {
	return &Event{
		ID:           "01920000-0000-7000-8000-000000000001",
		Kind:         KindInstanceDown,
		OccurredAt:   time.Date(2026, 9, 12, 2, 0, 0, 0, time.UTC),
		InstanceID:   "01920000-0000-7000-8000-000000000002",
		InstanceName: `Bob's "Valheim" server`,
		Detail:       map[string]string{"Exit code": "137"},
	}
}

// TestARenderedPayloadCannotBeReshapedByItsValues asserts 05 M6's structural rule: the body is
// built and marshalled, never assembled as text, so a server name carrying a quote is a string
// value rather than a way to add fields.
func TestARenderedPayloadCannotBeReshapedByItsValues(t *testing.T) {
	for _, kind := range DestinationKinds {
		body, err := Render(kind, testEvent())
		if err != nil {
			t.Fatalf("Render(%s): %v", kind, err)
		}
		var decoded any
		if err := json.Unmarshal(body.Bytes, &decoded); err != nil {
			t.Fatalf("%s payload does not decode: %v", kind, err)
		}
		if !strings.Contains(string(body.Bytes), `Bob's \"Valheim\" server`) {
			t.Errorf("%s payload = %s, want the name escaped rather than interpolated", kind, body.Bytes)
		}
	}
}

// TestTheGenericPayloadCarriesTheStableEventIdentity asserts what a generic receiver switches
// on: a schema version, the event id that is stable across retries, the kind and the instance.
func TestTheGenericPayloadCarriesTheStableEventIdentity(t *testing.T) {
	body, err := Render(KindGeneric, testEvent())
	if err != nil {
		t.Fatal(err)
	}
	// Decoded into a shadow of the envelope: nothing in the panel reads a payload back, so
	// Kind is a wire name on the way out only.
	var got struct {
		Version    int               `json:"version"`
		ID         string            `json:"id"`
		Kind       string            `json:"kind"`
		OccurredAt string            `json:"occurred_at"`
		Instance   *genericInstance  `json:"instance"`
		Detail     map[string]string `json:"detail"`
	}
	if err := json.Unmarshal(body.Bytes, &got); err != nil {
		t.Fatal(err)
	}
	if got.Version != GenericVersion {
		t.Errorf("version = %d, want %d", got.Version, GenericVersion)
	}
	if got.ID != testEvent().ID || got.Kind != KindInstanceDown.String() {
		t.Errorf("identity = %s/%s, want the event's own", got.ID, got.Kind)
	}
	if got.OccurredAt != "2026-09-12T02:00:00Z" {
		t.Errorf("occurred_at = %q, want RFC3339 UTC", got.OccurredAt)
	}
	if got.Instance == nil || got.Instance.ID != testEvent().InstanceID {
		t.Errorf("instance = %+v, want the event's", got.Instance)
	}
	if got.Detail["Exit code"] != "137" {
		t.Errorf("detail = %v, want the event's", got.Detail)
	}
}

// TestDetailIsBoundedOnRender asserts that a caller cannot build a body the panel will not
// send: too many fields are dropped and a long value is truncated.
func TestDetailIsBoundedOnRender(t *testing.T) {
	e := testEvent()
	e.Detail = map[string]string{"Long": strings.Repeat("x", MaxDetailValue*2)}
	for i := range MaxDetailFields * 2 {
		e.Detail[string(rune('a'+i))] = "v"
	}
	fields := e.bounded()
	if len(fields) != MaxDetailFields {
		t.Errorf("fields = %d, want %d", len(fields), MaxDetailFields)
	}
	e.Detail = map[string]string{"Long": strings.Repeat("x", MaxDetailValue*2)}
	if v := e.bounded()[0].Value; len([]rune(v)) != MaxDetailValue+1 {
		t.Errorf("value length = %d, want it truncated to %d", len([]rune(v)), MaxDetailValue)
	}
}
