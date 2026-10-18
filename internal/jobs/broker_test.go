package jobs

import "testing"

// TestATerminalEventIsDeliveredToAFullSubscriber asserts that a subscriber whose buffer is full
// of progress events still receives the job's terminal event, last.
func TestATerminalEventIsDeliveredToAFullSubscriber(t *testing.T) {
	b := newBroker()
	events, cancel := b.Subscribe("job-1")
	defer cancel()

	for range 200 {
		b.publish("job-1", Event{JobID: "job-1", LogLine: "line"})
	}
	b.publish("job-1", Event{JobID: "job-1", Status: StatusFailed})

	var last Event
	for len(events) > 0 {
		last = <-events
	}
	if last.Status != StatusFailed {
		t.Errorf("last event = %+v, want the failed status", last)
	}
}
