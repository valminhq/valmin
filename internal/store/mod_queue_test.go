package store

import "testing"

// TestQueueModInstallsIsAllOrNothing asserts a batch is queued in one transaction, rolled back
// whole when one entry is refused, and removed by name.
func TestQueueModInstallsIsAllOrNothing(t *testing.T) {
	db := open(t)
	ctx := t.Context()
	id := seedInstance(t, db, NewID(), 2456)
	names := func() []string {
		t.Helper()
		queued, err := db.QueuedModInstalls(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		out := make([]string, 0, len(queued))
		for _, q := range queued {
			out = append(out, q.FullName+"@"+q.Version)
		}
		return out
	}

	if err := db.QueueModInstalls(ctx, []QueuedModInstall{
		{InstanceID: id, FullName: "Ns-A", Version: "1.0.0"},
		{InstanceID: id, FullName: "Ns-B", Version: "1.0.0"},
		{InstanceID: id, FullName: "Ns-C", Version: "1.0.0"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := db.QueueModInstalls(ctx, []QueuedModInstall{
		{InstanceID: id, FullName: "Ns-A", Version: "2.0.0"},
		{InstanceID: "no-such-instance", FullName: "Ns-D", Version: "1.0.0"},
	}); err == nil {
		t.Fatal("a batch naming an unknown instance was accepted")
	}
	if got := names(); len(got) != 3 || got[0] != "Ns-A@1.0.0" {
		t.Fatalf("queue after a refused batch = %v, want the first batch unchanged", got)
	}

	if err := db.UnqueueModInstalls(ctx, id, []string{"Ns-A", "Ns-C", "Ns-Missing"}); err != nil {
		t.Fatal(err)
	}
	if got := names(); len(got) != 1 || got[0] != "Ns-B@1.0.0" {
		t.Errorf("queue after unqueueing two = %v, want Ns-B alone", got)
	}
}
