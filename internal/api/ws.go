package api

import (
	"context"
	"fmt"
	"sync"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/ws"
)

// sourceQueue is the adapter's own buffer between a source and the hub. It is small on
// purpose: the hub has the real queue and the real drop policy (ADR-039), and a deep buffer
// here would only delay the moment the client learns it is behind.
const sourceQueue = 64

// sockets wires the hub to the things it fans out.
//
// This adapter is the whole of ADR-042 in practice: every conversion from a Valheim log
// line, a cgroup sample or a job row into a wire message happens here, in the package that
// already knows what those are. internal/ws imports neither internal/instance nor
// internal/jobs, and a test asserts it.
type sockets struct {
	engine  *jobs.Engine
	streams *instance.Streams
}

// console replays the ring buffer and then follows the reader (14 §4.2).
//
// The subscription is taken *before* the replay is read, so nothing appended in between is
// lost; anything that lands in both is filtered by sequence number, which is exactly what
// seq is for.
func (s *sockets) console(instanceID string) (replay []ws.Message, live <-chan ws.Message, cancel func()) {
	reader, _ := s.streams.Attach(instanceID)
	entries, stop := reader.Subscribe()

	startup, recent := reader.Ring.Replay()
	var last uint64
	replay = make([]ws.Message, 0, len(startup)+len(recent))
	for _, e := range append(startup, recent...) {
		if e.Seq <= last {
			continue
		}
		replay = append(replay, consoleMessage(instanceID, e))
		last = e.Seq
	}

	live, cancel = pump(entries, stop, func(e instance.Entry) (ws.Message, bool) {
		switch {
		case e.Reset:
			// The stream restarted. The client clears its view rather than splicing, and
			// the sequence numbers deliberately do not restart with it (14 §4.2).
			return ws.Reset(ws.ConsoleTopic(instanceID)), true
		case e.Seq <= last:
			return ws.Message{}, false
		default:
			return consoleMessage(instanceID, e), true
		}
	}, true)
	return replay, live, cancel
}

func consoleMessage(instanceID string, e instance.Entry) ws.Message {
	return ws.Message{Seq: e.Seq, Payload: ws.ConsoleMsg{
		Type: "console", Instance: instanceID, Seq: e.Seq,
		TS: e.TS, Stream: e.Stream, Line: e.Text,
	}}
}

// stats follows the sampler. There is no replay: a resource graph starts when you open it,
// and the first sample carries cpu_pct null because it has no predecessor (E10).
func (s *sockets) stats(instanceID string) (replay []ws.Message, live <-chan ws.Message, cancel func()) {
	_, sampler := s.streams.Attach(instanceID)
	samples, stop := sampler.Subscribe()

	live, cancel = pump(samples, stop, func(sm instance.Sample) (ws.Message, bool) {
		return ws.Message{Payload: ws.StatsMsg{
			Type: "stats", Instance: instanceID, TS: sm.TS,
			CPUPct: sm.CPUPct, MemBytes: sm.MemBytes, MemLimit: sm.MemLimit,
			MemPct: sm.MemPct, Players: sm.Players,
		}}, true
	}, true)
	return nil, live, cancel
}

// job follows one job's progress and log lines (12 §7). The kind is read once here rather
// than carried on every event: it cannot change for the life of the job.
func (s *sockets) job(jobID string) (replay []ws.Message, live <-chan ws.Message, cancel func()) {
	events, stop := s.engine.Subscribe(jobID)

	kind := ""
	if j, err := s.engine.Get(context.Background(), jobID); err == nil && j != nil {
		kind = j.Kind
	}

	live, cancel = pump(events, stop, func(ev jobs.Event) (ws.Message, bool) {
		return ws.Message{Payload: ws.JobMsg{
			Type: "job", ID: ev.JobID, Kind: kind, Status: ev.Status,
			Progress: ev.Progress, Message: ev.Message, Log: ev.LogLine,
		}}, true
	}, false)
	return nil, live, cancel
}

// pump converts one source's channel into the hub's and hands back a cancel that ends both.
//
// drop is the topic's class: a lossy source that outruns the hub loses its oldest here as
// well as there, and the client sees one discontinuity either way. A lossless one blocks
// instead, because the hub is the only thing allowed to decide that a state or job message
// cannot be delivered — and its answer is to close the connection, not to skip a message.
func pump[T any](
	in <-chan T, stop func(), convert func(T) (ws.Message, bool), drop bool,
) (live <-chan ws.Message, cancel func()) {
	out := make(chan ws.Message, sourceQueue)
	done := make(chan struct{})
	go func() {
		defer close(out)
		for v := range in {
			m, ok := convert(v)
			if !ok {
				continue
			}
			if drop {
				select {
				case out <- m:
				case <-done:
					return
				default:
				}
				continue
			}
			select {
			case out <- m:
			case <-done:
				return
			}
		}
	}()

	var once sync.Once
	return out, func() {
		once.Do(func() { close(done) })
		stop()
	}
}

// resolver answers the hub's two existence questions (14 §2.2) against the database.
type resolver struct{ db *store.DB }

func (r resolver) InstanceExists(ctx context.Context, instanceID string) (bool, error) {
	inst, err := r.db.InstanceByID(ctx, instanceID)
	if err != nil {
		return false, fmt.Errorf("look up instance %s for a subscription: %w", instanceID, err)
	}
	return inst != nil, nil
}

// JobInstance resolves a job to its instance. A found job with no instance is a global one,
// which the hub treats as admin-only — and 12 §4.2 puts a job whose instance was deleted in
// exactly that category, rather than cascading the history away (C15).
func (r resolver) JobInstance(ctx context.Context, jobID string) (instanceID string, found bool, err error) {
	j, err := r.db.JobByID(ctx, jobID)
	if err != nil {
		return "", false, fmt.Errorf("look up job %s for a subscription: %w", jobID, err)
	}
	if j == nil {
		return "", false, nil
	}
	if j.InstanceID == nil {
		return "", true, nil
	}
	return *j.InstanceID, true, nil
}

// announceState is the state publisher of 14 §4.4, registered on the job engine and called
// by the observer. It re-reads the row rather than being told what was written: the two
// writers of instances.state (12 §1) both land their change in a transaction that has since
// committed, so the row is the truth and a passed-in value is a second copy of it that can
// disagree.
func announceState(db *store.DB, hub *ws.Hub) func(ctx context.Context, instanceID string) {
	return func(ctx context.Context, instanceID string) {
		inst, err := db.InstanceByID(ctx, instanceID)
		if err != nil {
			return
		}
		if inst == nil {
			// The row is gone, so the delete job committed. 14 §6 drops the instance's
			// topics and closes nothing: the user may still hold others.
			hub.InstanceDeleted(instanceID)
			return
		}
		hub.PublishState(inst.ID, inst.State, inst.RestartRequired)
	}
}
