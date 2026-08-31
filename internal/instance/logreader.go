package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// The ring's two bounds, from 04 §4 and 14 §4.2.
//
// `↯` Lines *and* bytes, whichever binds first. A line count alone is not a memory bound
// when one mod can log a megabyte.
const (
	RingLines = 1000
	RingBytes = 1 << 20
)

// The startup segment's bounds. 14 §4.2 pins "container start through the chainloader",
// which on a modded server ends at a line and on a vanilla one never arrives — so the
// segment is sealed by whichever comes first: the readiness line, or these caps.
const (
	StartupLines = 500
	StartupBytes = 256 << 10
)

// subscriberQueue is one console subscriber's buffer. The reader never blocks on a
// subscriber (C21): a full queue drops, and the subscriber notices because Entry.Seq skips.
// The drop *policy* — the gap message, the close-on-lossless — is the hub's, not the
// reader's (ADR-039, 14 §5).
const subscriberQueue = 256

// reprimeTail is how much history a re-opened stream asks for. The client clears its view on
// the stream.reset that precedes it, so this is context rather than continuity.
const reprimeTail = 100

// readerRetryDelay paces re-opening a stream that ended while the container is still up.
const readerRetryDelay = 2 * time.Second

// ErrStreamReset reports that the log stream restarted while a caller was waiting on a line.
//
// `↯` This is C20's fail-closed rule and it is the reason jobs read Await rather than
// subscribing to the console. 12 §3.4 requires the anchored save-complete line before a
// backup archives anything; if the stream hiccuped, the panel does not know whether the line
// was written while it was not looking. No line, no archive.
var ErrStreamReset = errors.New("the log stream restarted while waiting for a log line")

// Entry is one message on an instance's console topic.
type Entry struct {
	// Seq is per instance and monotonic, and does not reset when the stream does. It is what
	// lets a client reconcile replay against live messages, and a subscriber detect its own
	// dropped messages, without comparing content (14 §4.2).
	Seq uint64
	// Reset marks a stream.reset: the reader restarted, and the client clears its view
	// rather than splicing. Line is zero on such an entry.
	Reset bool
	Line
}

// Ring is one instance's recent console history plus its pinned startup segment. It outlives
// the reader: 14 §8 keeps the buffer when an instance stops, because the console of a stopped
// server is the most useful moment it has.
type Ring struct {
	mu      sync.Mutex
	seq     uint64
	recent  []Entry
	bytes   int
	startup []Entry
	sBytes  int
	sealed  bool
}

// Append records a line and returns the entry it became.
func (r *Ring) Append(l Line) Entry {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.seq++
	e := Entry{Seq: r.seq, Line: l}
	n := len(l.Text)

	r.recent = append(r.recent, e)
	r.bytes += n
	if drop := r.overflow(); drop > 0 {
		for _, dropped := range r.recent[:drop] {
			r.bytes -= len(dropped.Text)
		}
		copy(r.recent, r.recent[drop:])
		r.recent = r.recent[:len(r.recent)-drop]
	}

	// `↯` The startup segment is kept separately, so it survives the rotation above. On a
	// busy server those lines are gone within minutes, and they are the ones that explain a
	// failed boot — an operator opening the console at lunchtime to ask "did my mods load"
	// would otherwise get no answer at all (G8, 14 §4.2).
	if !r.sealed {
		r.startup = append(r.startup, e)
		r.sBytes += n
		if len(r.startup) >= StartupLines || r.sBytes >= StartupBytes {
			r.sealed = true
		}
	}
	return e
}

// overflow reports how many of the oldest entries must go for the ring to be within both
// bounds. The newest entry is never dropped, whatever its size.
func (r *Ring) overflow() int {
	drop, size := 0, r.bytes
	for len(r.recent)-drop > 1 && (len(r.recent)-drop > RingLines || size > RingBytes) {
		size -= len(r.recent[drop].Text)
		drop++
	}
	return drop
}

// Seal ends the startup segment. The reader calls it on the readiness line; Ring itself
// calls it on the caps.
func (r *Ring) Seal() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.sealed = true
}

// Arm starts a new startup segment, discarding the previous one. A new container is a new
// boot; a re-opened stream against the same container is not, which is why this is Open's
// job and not the read loop's.
func (r *Ring) Arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startup, r.sBytes, r.sealed = nil, 0, false
}

// Replay returns the pinned startup segment and the recent history, oldest first. The two
// overlap until the ring has rotated past the startup lines; the client discards what it has
// already rendered by Seq, which is what Seq is for.
func (r *Ring) Replay() (startup, recent []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.startup...), append([]Entry(nil), r.recent...)
}

// Since returns the entries newer than seq, oldest first. An entry that has already rotated
// out of the ring is gone: a caller that has fallen more than RingLines behind cannot learn
// what it missed, which is why Await treats a miss as a timeout rather than a "no".
func (r *Ring) Since(seq uint64) []Entry {
	r.mu.Lock()
	defer r.mu.Unlock()
	for i := range r.recent {
		if r.recent[i].Seq > seq {
			return append([]Entry(nil), r.recent[i:]...)
		}
	}
	return nil
}

// Seq returns the last sequence number issued.
func (r *Ring) Seq() uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.seq
}

// Reader is one instance's log reader: one goroutine per running container (02 §4.5 — never
// one Docker stream per browser tab), demuxing frames into lines, matching the pattern set
// once per line, filling the ring and fanning out to subscribers.
type Reader struct {
	Ring     *Ring
	patterns PatternSet

	mu     sync.Mutex
	subs   map[chan Entry]struct{}
	waits  map[*wait]struct{}
	stop   context.CancelFunc
	done   chan struct{}
	source string
}

// wait is one Await call.
type wait struct {
	kind  EventKind
	event chan LogEvent
	reset chan struct{}
}

func newReader() *Reader {
	return &Reader{
		Ring:     &Ring{},
		patterns: DefaultPatterns,
		subs:     make(map[chan Entry]struct{}),
		waits:    make(map[*wait]struct{}),
	}
}

// Subscribe returns a channel of this instance's console entries and a cancel func that must
// be called to stop receiving. Replay what the ring already holds before consuming the
// channel: entries are only sent live.
func (r *Reader) Subscribe() (entries <-chan Entry, cancel func()) {
	ch := make(chan Entry, subscriberQueue)

	r.mu.Lock()
	r.subs[ch] = struct{}{}
	r.mu.Unlock()

	var once sync.Once
	return ch, func() {
		once.Do(func() {
			r.mu.Lock()
			delete(r.subs, ch)
			r.mu.Unlock()
			close(ch)
		})
	}
}

// Await blocks until a line of the given kind is read after sequence number since, and fails
// closed on a stream restart.
//
// `↯` It is the channel 14 §4.2 requires jobs to use instead of the hub. The hub is lossy by
// design; a job that waited on it would archive a world because a console subscriber's queue
// happened to have room.
//
// `↯` since is a parameter and not an internal detail, because the alternative loses lines.
// A caller stops a server and *then* waits for the save to finish; on a fast stop the line
// has already been read by the time the wait is registered, and a wait that only sees the
// future would block until its own timeout on a save that completed perfectly. Capture
// Ring.Seq() before triggering the thing being waited for, and pass it here.
func (r *Reader) Await(ctx context.Context, kind EventKind, since uint64) (LogEvent, error) {
	w := &wait{kind: kind, event: make(chan LogEvent, 1), reset: make(chan struct{})}

	// Register before scanning, never after: a line arriving between the two would otherwise
	// fall through the gap and be seen by neither.
	r.mu.Lock()
	r.waits[w] = struct{}{}
	r.mu.Unlock()

	defer func() {
		r.mu.Lock()
		delete(r.waits, w)
		r.mu.Unlock()
	}()

	for _, e := range r.Ring.Since(since) {
		if ev, ok := r.patterns.Match(e.Text); ok && ev.Kind == kind {
			return ev, nil
		}
	}

	select {
	case ev := <-w.event:
		return ev, nil
	case <-w.reset:
		return LogEvent{}, ErrStreamReset
	case <-ctx.Done():
		return LogEvent{}, fmt.Errorf("await a %s log line: %w", kind, ctx.Err())
	}
}

// append is the read loop's per-line work: ring, patterns, fan-out.
func (r *Reader) append(l Line) {
	e := r.Ring.Append(l)
	if ev, ok := r.patterns.Match(l.Text); ok {
		r.deliver(ev)
		if ev.Kind == EventReady {
			r.Ring.Seal()
		}
	}
	r.publish(e)
}

func (r *Reader) deliver(ev LogEvent) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for w := range r.waits {
		if w.kind != ev.Kind {
			continue
		}
		select {
		case w.event <- ev:
		default:
		}
	}
}

// publish never blocks (C21). One sleeping laptop must not freeze every console, so a
// subscriber that has stopped reading loses messages and finds out from the gap in Seq.
func (r *Reader) publish(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.subs {
		select {
		case ch <- e:
		default:
		}
	}
}

// reset publishes a stream.reset and fails every outstanding wait (C20).
func (r *Reader) reset() {
	r.mu.Lock()
	for w := range r.waits {
		close(w.reset)
		delete(r.waits, w)
	}
	r.mu.Unlock()
	r.publish(Entry{Seq: r.Ring.Seq(), Reset: true})
}

// run reads containerID's stream until ctx is cancelled or the container is gone.
func (r *Reader) run(ctx context.Context, rt runtime.Runtime, instanceID, containerID string, done chan struct{}) {
	defer func() {
		r.mu.Lock()
		if r.source == containerID {
			r.source = ""
		}
		r.mu.Unlock()
		close(done)
	}()

	for first := true; ctx.Err() == nil; first = false {
		tail := 0
		if !first {
			r.reset()
			tail = reprimeTail
		}
		if err := r.read(ctx, rt, containerID, tail); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "log stream ended, will re-open",
				slog.String("instance_id", instanceID),
				slog.String("container_id", containerID), slog.Any("error", err))
		}
		// A stream that ends because the server exited is not a stream to re-open. The
		// supervisor will call Close on its next pass regardless; this stops the reader from
		// emitting a reset every couple of seconds in the meantime.
		if c, err := rt.Inspect(ctx, containerID); err == nil && !c.Running {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(readerRetryDelay):
		}
	}
}

func (r *Reader) read(ctx context.Context, rt runtime.Runtime, containerID string, tail int) error {
	rc, err := rt.Logs(ctx, containerID, runtime.LogOptions{Follow: true, Timestamps: true, Tail: tail})
	if err != nil {
		return fmt.Errorf("open log stream of container %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()
	return DemuxLines(rc, r.append)
}

// Streams is the registry of per-instance sources — one log reader and one stats sampler
// each. 14 §8 owns the lifecycle and gives both the same one: they start when a container
// runs and stop when it does not, which is why they share a registry rather than having two
// that answer the same question on two timers.
type Streams struct {
	rt runtime.Runtime

	mu       sync.Mutex
	readers  map[string]*Reader
	samplers map[string]*Sampler
}

func NewStreams(rt runtime.Runtime) *Streams {
	return &Streams{
		rt:       rt,
		readers:  make(map[string]*Reader),
		samplers: make(map[string]*Sampler),
	}
}

// Reader returns instanceID's log reader, or nil if the panel has never read that instance's
// log. The ring it holds outlives the container, so this keeps answering after a stop.
func (l *Streams) Reader(instanceID string) *Reader {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.readers[instanceID]
}

// Sampler returns instanceID's stats sampler, or nil if it has never run.
func (l *Streams) Sampler(instanceID string) *Sampler {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.samplers[instanceID]
}

// Attach returns instanceID's reader and sampler, creating them if the panel has not read
// that instance yet, and starting neither.
//
// `↯` It exists so a subscriber can arrive before the container does. A console opened on a
// stopped server holds the same objects a later Open attaches to a container, so the boot it
// was opened to watch is not missed — which is what would happen if a subscription resolved
// to whichever Reader happened to exist at subscribe time.
func (l *Streams) Attach(instanceID string) (*Reader, *Sampler) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.readers[instanceID]
	if r == nil {
		r = newReader()
		l.readers[instanceID] = r
	}
	sampler := l.samplers[instanceID]
	if sampler == nil {
		sampler = newSampler()
		l.samplers[instanceID] = sampler
	}
	return r, sampler
}

// Open starts reading containerID's log and sampling its stats for instanceID, and is a
// no-op for whichever of the two is already running against that container. A different
// container id restarts both and arms a fresh startup segment.
func (l *Streams) Open(instanceID, containerID string) *Reader {
	r, sampler := l.Attach(instanceID)

	sampler.start(l.rt, instanceID, containerID)

	if r.reading() == containerID {
		return r
	}
	r.halt()
	r.Ring.Arm()

	// The reader is process-scoped, not request-scoped: it must outlive the reconcile pass
	// that noticed the container, and halt is the only thing that ends it.
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	r.mu.Lock()
	r.stop, r.done, r.source = cancel, done, containerID
	r.mu.Unlock()

	go r.run(ctx, l.rt, instanceID, containerID, done)
	return r
}

// Close stops reading instanceID's log and sampling its stats.
//
// `↯` 14 §8: the sampler stops and the ring buffer stays. A stopped server has no resource
// usage worth graphing, but its console is the most useful moment it has — it is where the
// reason it stopped is written.
func (l *Streams) Close(instanceID string) {
	l.mu.Lock()
	r, sampler := l.readers[instanceID], l.samplers[instanceID]
	l.mu.Unlock()

	if r != nil {
		r.halt()
	}
	if sampler != nil {
		sampler.halt()
	}
}

// Shutdown stops every source. The rings stay, but nothing outlives the process anyway —
// 14 §8 says buffers are empty after a daemon restart and stream.reset covers it.
func (l *Streams) Shutdown() {
	l.mu.Lock()
	ids := make([]string, 0, len(l.readers))
	for id := range l.readers {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	for _, id := range ids {
		l.Close(id)
	}
}

func (r *Reader) reading() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.source
}

// halt stops the read loop and waits for it, so a re-open cannot leave two goroutines
// appending to one ring.
func (r *Reader) halt() {
	r.mu.Lock()
	stop, done := r.stop, r.done
	r.stop, r.done = nil, nil
	r.mu.Unlock()

	if stop == nil {
		return
	}
	stop()
	<-done
}
