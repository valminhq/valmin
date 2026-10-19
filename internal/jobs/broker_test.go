package jobs

import "testing"

// TestEverySubscriberSeesEveryEventInOrder asserts a burst of progress events and the terminal
// event that follows reach a subscriber whole and in order, and that nothing arrives after
// cancel.
func TestEverySubscriberSeesEveryEventInOrder(t *testing.T) {
	b := newBroker()
	var got []Event
	cancel := b.Subscribe("job-1", func(ev Event) { got = append(got, ev) })

	for range 200 {
		b.publish("job-1", Event{JobID: "job-1", LogLine: "line"})
	}
	b.publish("job-1", Event{JobID: "job-1", Status: StatusFailed})
	b.publish("job-2", Event{JobID: "job-2", Status: StatusFailed})
	cancel()
	b.publish("job-1", Event{JobID: "job-1", LogLine: "after"})

	if len(got) != 201 || got[200].Status != StatusFailed {
		t.Fatalf("received %d events ending in %+v, want 201 ending in the failed status", len(got), got[len(got)-1])
	}
}
