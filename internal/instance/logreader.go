package instance

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync"
	"time"

	"github.com/valminhq/valmin/internal/runtime"
)

// The ring's two bounds, whichever binds first (04 §4, 14 §4.2). A line count alone is not a
// memory bound when one mod can log a megabyte.
const (
	RingLines = 1000
	RingBytes = 1 << 20
)

// The startup segment's bounds. 14 §4.2 pins "container start through the chainloader", which
// a vanilla server never logs, so the segment is sealed by the readiness line or these caps,
// whichever comes first.
const (
	StartupLines = 500
	StartupBytes = 256 << 10
)

// reprimeTail is how much history a re-opened stream asks for. The client clears its view on
// the stream.reset that precedes it, so this is context rather than continuity.
const reprimeTail = 100

// readerRetryDelay paces re-opening a stream that ended while the container is still up.
const readerRetryDelay = 2 * time.Second

// Entry is one message on an instance's console topic.
type Entry struct {
	// Seq is per instance and monotonic, and does not reset when the stream does, so a client
	// can reconcile replay against live messages and spot its own drops (14 §4.2).
	Seq uint64
	// Reset marks a stream.reset: the reader restarted, and the client clears its view
	// rather than splicing. Line is zero on such an entry.
	Reset bool
	Line
}

// Ring is one instance's recent console history plus its pinned startup segment. It outlives
// the reader, so the console of a stopped server stays readable (14 §8).
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
		// Re-slicing rather than shifting keeps an append amortized O(1); append copies the
		// live entries only when it outgrows the backing array.
		clear(r.recent[:drop])
		r.recent = r.recent[drop:]
	}

	// Kept separately so the boot lines survive the rotation above (G8, 14 §4.2).
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

// Arm starts a new startup segment, discarding the previous one. Called by Open, not by the
// read loop: a new container is a new boot, a re-opened stream against the same one is not.
func (r *Ring) Arm() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.startup, r.sBytes, r.sealed = nil, 0, false
}

// Replay returns the pinned startup segment and the recent history, oldest first. The two
// overlap until the ring rotates past the startup lines, and the client dedupes by Seq.
func (r *Ring) Replay() (startup, recent []Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Entry(nil), r.startup...), append([]Entry(nil), r.recent...)
}

// Since returns the entries newer than seq, oldest first. An entry already rotated out of the
// ring is gone, so a caller further behind than RingLines cannot learn what it missed.
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

// Reader is one instance's log reader: one goroutine per running container, never one Docker
// stream per browser tab (02 §4.5). It demuxes frames into lines, matches the pattern set once
// per line, fills the ring and fans out to subscribers.
type Reader struct {
	Ring     *Ring
	patterns PatternSet
	players  playerCount
	// onPlayers is called whenever the derived count changes, including when it becomes
	// unknown. Set once at Attach; nil for a reader nobody records.
	onPlayers func(PlayerObservation)
	// onIdentity is called for every line that names an account. Set once at Attach; nil for
	// a reader nobody records.
	onIdentity func(PlayerIdentity)
	// onJoinCode is called whenever the latched join code changes, including when it is
	// cleared. Set once at Attach; nil for a reader nobody announces.
	onJoinCode func(code string)
	// onEye is called for every ValminEye event newer than the stream's open. Set once at
	// Attach; nil for a reader nobody announces.
	onEye func(EyeEvent)
	// openedAt and restate belong to the run goroutine. Lines older than openedAt are a
	// re-read of history already recorded; restate makes the first newer count line recorded
	// even when its value did not change, so the history ends on a known count again.
	openedAt time.Time
	restate  bool
	// bootAt is the start of the boot being read, owned by the run goroutine. A line stamped
	// before it belongs to an earlier boot of the same container and is not latched. readBoot is
	// the boot whose lines the ring already holds.
	bootAt, readBoot time.Time
	// sockets maps each connecting socket's remote id to the platform id it presented, also
	// owned by the run goroutine.
	sockets map[string]string
	// names ties spawned characters to the accounts that were waiting, also owned by the run
	// goroutine.
	names nameMatcher

	loop loop

	mu       sync.Mutex
	subs     subscribers[Entry]
	joinCode string
	disk     *DiskThresholds
	boot     *bootLatch
	watchers map[chan struct{}]struct{}
}

// DiskThresholds is what the server said about its own disk floors. The panel reads them
// rather than hardcoding them because the server computes them at runtime, so a future build
// can move them under a panel that would otherwise go on alarming at the old numbers
// (03 §3.4).
type DiskThresholds struct {
	// AvailableBytes is the space the server saw, from inside its own container.
	AvailableBytes uint64 `json:"available_bytes"`
	// BlockedBelowBytes is the floor under which it stops persisting the world. It does not
	// crash and it does not log an error; the loss is found when someone reconnects.
	BlockedBelowBytes uint64 `json:"blocked_below_bytes"`
	// WarnBelowBytes is the floor under which it warns.
	WarnBelowBytes uint64 `json:"warn_below_bytes"`
}

func newReader(patterns PatternSet) *Reader {
	return &Reader{
		Ring:     &Ring{},
		patterns: patterns,
		boot:     newBootLatch(),
	}
}

// latch is the record of the boot being read.
func (r *Reader) latch() *bootLatch {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.boot
}

// syncBoot records that the stream is reading the boot that began at at. When the container has
// started again since the last call, the old record is ended and a new one starts, so a waiter
// never mistakes the earlier boot's lines for this one's.
func (r *Reader) syncBoot(at time.Time) {
	r.mu.Lock()
	old := r.boot
	if began := old.bootStart(); !began.IsZero() && !began.Equal(at) {
		r.boot = newBootLatch()
	}
	r.boot.setBoot(at)
	current := r.boot
	r.mu.Unlock()
	if old != current {
		old.end()
	}
}

// newBoot starts the record of a new boot.
func (r *Reader) newBoot() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.boot = newBootLatch()
}

// Subscribe calls fn with each console entry from now on, until cancel returns. Entries are
// only sent live, so a caller replays what the ring already holds itself. fn runs on the read
// loop and must not block.
func (r *Reader) Subscribe(fn func(Entry)) (cancel func()) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id := r.subs.add(fn)
	return func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.subs.remove(id)
	}
}

// append is the read loop's per-line work: ring, patterns, fan-out.
func (r *Reader) append(l Line) {
	e := r.Ring.Append(l)
	if ev, ok := r.patterns.Match(l.Text); ok {
		r.matched(ev, l.TS)
	}
	r.publish(e)
}

// matched is what one recognised line costs: the latched values, and whatever
// the line says about who is connected. ts is Docker's receive time, never the reader's
// clock — after a re-open the two differ by the length of the gap (14 §4.1).
func (r *Reader) matched(ev LogEvent, ts time.Time) {
	if ts.IsZero() || !ts.Before(r.bootAt) {
		r.latch().note(ev.Kind)
	}
	if ev.Kind == EventReady {
		r.Ring.Seal()
	}
	if ev.Kind == EventCrossplaySession {
		r.setJoinCode(ev.Groups[1])
	}
	if ev.Kind == EventDiskThresholds {
		r.setDisk(ev.Groups)
	}
	if id, ok := identityOf(ev, ts); ok {
		r.identified(id)
	}
	if id, ok := r.refusal(ev, ts); ok {
		r.identified(id)
	}
	players, changed := r.players.apply(ev)
	if !ts.IsZero() && ts.Before(r.openedAt) {
		return
	}
	r.matchNames(ev, ts)
	r.announceEye(ev, ts)
	if !changed && r.restate && (ev.Kind == EventPlayerCount || ev.Kind == EventConnections) {
		players, changed = r.players.current(), true
	}
	if changed {
		r.restate = false
		r.observed(PlayerObservation{TS: ts, Players: players})
	}
}

// matchNames feeds a live line to the name matcher and reports the guesses it makes. A re-read
// line never reaches it, because a guess adds to a count instead of restating a fact.
func (r *Reader) matchNames(ev LogEvent, ts time.Time) {
	if ev.Kind == EventPlatformID {
		r.names.connected(ev.Groups[1], ev.Groups[2], ts)
	}
	if ev.Kind == EventWrongPassword {
		r.names.refused(ev.Groups[1])
	}
	if ev.Kind != EventCharacter {
		return
	}
	for _, id := range r.names.spawn(ev.Groups[1], ev.Groups[2], ts) {
		r.identified(id)
	}
}

// announceEye reports the account a ValminEye join line names and hands the decoded event to
// onEye.
func (r *Reader) announceEye(ev LogEvent, ts time.Time) {
	if ev.Kind != EventEye {
		return
	}
	e, ok := parseEye(ev.Groups[1], ts)
	if !ok {
		return
	}
	if id, ok := e.identity(); ok {
		r.identified(id)
	}
	if r.onEye != nil {
		r.onEye(e)
	}
}

// opened marks the start of a log stream read: what comes before at is a re-read. The name
// matcher forgets who was waiting, because the lines it was waiting on may have been written
// while the stream was closed, and keeps the connection numbers it has seen.
func (r *Reader) opened(at time.Time) {
	r.openedAt, r.restate = at, true
	r.names = nameMatcher{spawned: r.names.spawned, seenOrder: r.names.seenOrder}
}

// Players is the count derived from this container's log, or nil for "not known".
func (r *Reader) Players() *int { return r.players.current() }

// maxSockets bounds the remote ids a reader remembers.
const maxSockets = 256

// refusal attributes a wrong-password line to the account its socket presented, matched by the
// remote id both lines carry. A refusal from a socket that presented no account is dropped
// rather than guessed.
func (r *Reader) refusal(ev LogEvent, ts time.Time) (PlayerIdentity, bool) {
	if ev.Kind == EventPlatformID {
		// ponytail: forgets every socket at the cap, losing at most a refusal still in flight;
		// evict oldest if a server ever sees that many accounts between restarts.
		if r.sockets == nil || len(r.sockets) >= maxSockets {
			r.sockets = make(map[string]string)
		}
		r.sockets[ev.Groups[1]] = ev.Groups[2]
		return PlayerIdentity{}, false
	}
	if ev.Kind != EventWrongPassword {
		return PlayerIdentity{}, false
	}
	platformID, ok := r.sockets[ev.Groups[1]]
	if !ok {
		return PlayerIdentity{}, false
	}
	return PlayerIdentity{TS: ts, PlatformID: platformID, WrongPassword: true}, true
}

func (r *Reader) identified(id PlayerIdentity) {
	r.mu.Lock()
	announce := r.onIdentity
	r.mu.Unlock()
	if announce != nil {
		announce(id)
	}
}

func (r *Reader) observed(obs PlayerObservation) {
	if r.onPlayers != nil {
		r.onPlayers(obs)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for ch := range r.watchers {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// WatchPlayers returns a channel that receives after each change to the player count, including
// when it becomes unknown, and the function that ends the watch.
func (r *Reader) WatchPlayers() (changed <-chan struct{}, stop func()) {
	ch := make(chan struct{}, 1)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.watchers == nil {
		r.watchers = make(map[chan struct{}]struct{})
	}
	r.watchers[ch] = struct{}{}
	return ch, func() {
		r.mu.Lock()
		defer r.mu.Unlock()
		delete(r.watchers, ch)
	}
}

// JoinCode is the crossplay join code this container's session last logged. It is "" until
// the session is active, which means "not yet" rather than "there is none" (Q25).
func (r *Reader) JoinCode() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.joinCode
}

func (r *Reader) setJoinCode(code string) {
	r.mu.Lock()
	changed, announce := r.joinCode != code, r.onJoinCode
	r.joinCode = code
	r.mu.Unlock()

	// Outside the lock: the announcement reaches the hub, and a subscriber's queue is not
	// somewhere to hold a reader's mutex.
	if changed && announce != nil {
		announce(code)
	}
}

// Disk is what this container's server last said about its own disk floors, or nil for "it has
// not said". A server prints the line on shutdown, so a running instance that has not stopped
// since the panel attached reports nothing and the caller falls back to its own floor.
func (r *Reader) Disk() *DiskThresholds {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.disk == nil {
		return nil
	}
	d := *r.disk
	return &d
}

func (r *Reader) setDisk(groups []string) {
	available, err := strconv.ParseUint(groups[1], 10, 64)
	if err != nil {
		return
	}
	blocked, err := strconv.ParseUint(groups[2], 10, 64)
	if err != nil {
		return
	}
	warn, err := strconv.ParseUint(groups[3], 10, 64)
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.disk = &DiskThresholds{AvailableBytes: available, BlockedBelowBytes: blocked, WarnBelowBytes: warn}
}

func (r *Reader) publish(e Entry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.subs.send(e)
}

// reset publishes a stream.reset. The player count goes with it: a stream that restarted saw
// none of what happened while it was closed.
func (r *Reader) reset() {
	if r.players.invalidate() {
		r.observed(PlayerObservation{TS: time.Now()})
	}
	r.publish(Entry{Seq: r.Ring.Seq(), Reset: true})
}

// run reads containerID's stream until ctx is cancelled or the container is gone.
func (r *Reader) run(ctx context.Context, rt runtime.Runtime, instanceID, containerID string) {
	defer func() { r.latch().end() }()
	for ctx.Err() == nil {
		c, err := rt.Inspect(ctx, containerID)
		if errors.Is(err, runtime.ErrNotFound) {
			return
		}
		if err != nil {
			if !pause(ctx, readerRetryDelay) {
				return
			}
			continue
		}
		r.bootAt = bootOf(&c)
		r.syncBoot(r.bootAt)
		opts := runtime.LogOptions{Follow: true, Timestamps: true}
		if r.bootAt.Equal(r.readBoot) {
			// This boot's lines are already in the ring, so the stream only re-primes.
			r.reset()
			opts.Tail = reprimeTail
		} else {
			// The container's log keeps every earlier session; this one starts at its boot.
			opts.Since = r.bootAt
		}
		r.readBoot = r.bootAt
		r.opened(time.Now())
		if err := r.read(ctx, rt, containerID, opts); err != nil && ctx.Err() == nil {
			slog.WarnContext(ctx, "log stream ended, will re-open",
				slog.String("instance_id", instanceID),
				slog.String("container_id", containerID), slog.Any("error", err))
		}
		// A stream that ended because the server exited is not one to re-open; the supervisor
		// calls Close on its next pass.
		if containerGone(ctx, rt, containerID) || !pause(ctx, readerRetryDelay) {
			return
		}
	}
}

// pause waits d and reports false if ctx ended first.
func pause(ctx context.Context, d time.Duration) bool {
	select {
	case <-ctx.Done():
		return false
	case <-time.After(d):
		return true
	}
}

// containerGone reports whether containerID has stopped or no longer exists, which ends a
// reader's or a sampler's loop. Any other inspection failure is transient.
func containerGone(ctx context.Context, rt runtime.Runtime, containerID string) bool {
	c, err := rt.Inspect(ctx, containerID)
	if errors.Is(err, runtime.ErrNotFound) {
		return true
	}
	return err == nil && !c.Running
}

func (r *Reader) read(ctx context.Context, rt runtime.Runtime, containerID string, opts runtime.LogOptions) error {
	rc, err := rt.Logs(ctx, containerID, opts)
	if err != nil {
		return fmt.Errorf("open log stream of container %s: %w", containerID, err)
	}
	defer func() { _ = rc.Close() }()
	return DemuxLines(rc, r.append)
}

// Streams is the registry of per-instance sources: one log reader and one stats sampler each.
// Both share one lifecycle, starting when a container runs and stopping when it does not
// (14 §8).
type Streams struct {
	rt       runtime.Runtime
	patterns PatternSet
	hooks    StreamHooks

	// openMu serializes Open and Close, so a reader is never started and stopped at once.
	openMu   sync.Mutex
	mu       sync.Mutex
	readers  map[string]*Reader
	samplers map[string]*Sampler
	shutdown bool
}

// StreamHooks are what an instance's log tells the rest of the panel. Each may be nil, and
// none may block: they run on the read loop.
type StreamHooks struct {
	// OnPlayers records a change in an instance's derived player count.
	OnPlayers func(instanceID string, obs PlayerObservation)
	// OnIdentity records an account an instance's log named.
	OnIdentity func(instanceID string, id PlayerIdentity)
	// OnJoinCode announces an instance's crossplay join code, and "" for the sessions that
	// no longer have one.
	OnJoinCode func(instanceID, code string)
	// OnEye announces a ValminEye event an instance's log carried.
	OnEye func(instanceID string, ev EyeEvent)
}

// NewStreams returns the registry of rt's instance logs, matched with patterns.
func NewStreams(rt runtime.Runtime, patterns PatternSet, hooks StreamHooks) *Streams {
	return &Streams{
		rt:       rt,
		patterns: patterns,
		hooks:    hooks,
		readers:  make(map[string]*Reader),
		samplers: make(map[string]*Sampler),
	}
}

// Patterns is the set every log of these streams is matched with.
func (l *Streams) Patterns() PatternSet { return l.patterns }

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

// Attach returns instanceID's reader and sampler, creating them if needed and starting
// neither, so a subscriber can arrive before the container does. A console opened on a stopped
// server holds the same objects a later Open attaches, and so does not miss the boot.
func (l *Streams) Attach(instanceID string) (*Reader, *Sampler) {
	l.mu.Lock()
	defer l.mu.Unlock()
	r := l.readers[instanceID]
	if r == nil {
		r = newReader(l.patterns)
		h := l.hooks
		if h.OnPlayers != nil {
			r.onPlayers = func(obs PlayerObservation) { h.OnPlayers(instanceID, obs) }
		}
		if h.OnIdentity != nil {
			r.onIdentity = func(id PlayerIdentity) { h.OnIdentity(instanceID, id) }
		}
		if h.OnJoinCode != nil {
			r.onJoinCode = func(code string) { h.OnJoinCode(instanceID, code) }
		}
		if h.OnEye != nil {
			r.onEye = func(ev EyeEvent) { h.OnEye(instanceID, ev) }
		}
		l.readers[instanceID] = r
	}
	sampler := l.samplers[instanceID]
	if sampler == nil {
		sampler = newSampler()
		sampler.players = r.Players
		l.samplers[instanceID] = sampler
	}
	return r, sampler
}

// Open starts reading containerID's log and sampling its stats for instanceID, and is a
// no-op for whichever of the two is already running against that container. A different
// container id restarts both and arms a fresh startup segment. After Shutdown it starts
// nothing.
func (l *Streams) Open(instanceID, containerID string) *Reader {
	l.openMu.Lock()
	defer l.openMu.Unlock()
	return l.start(instanceID, containerID, false)
}

// reopen starts the reader of instanceID again against containerID, even if it already runs
// against it.
func (l *Streams) reopen(instanceID, containerID string) *Reader {
	l.openMu.Lock()
	defer l.openMu.Unlock()
	return l.start(instanceID, containerID, true)
}

// start is Open and reopen with openMu held.
func (l *Streams) start(instanceID, containerID string, again bool) *Reader {
	r, sampler := l.Attach(instanceID)
	if l.isShutdown() {
		return r
	}

	sampler.start(l.rt, instanceID, containerID)

	if !again && r.loop.running() == containerID {
		return r
	}
	r.halt()
	r.Ring.Arm()

	// A new container is a new session, and its predecessor's count is not its own. The disk
	// floors are kept, unlike the join code halt has already cleared: they are a property of
	// the build and the filesystem, not of the session, and the line that carries them is
	// printed on shutdown.
	if r.players.invalidate() {
		r.observed(PlayerObservation{TS: time.Now()})
	}

	r.newBoot()
	r.loop.start(containerID, func(ctx context.Context) { r.run(ctx, l.rt, instanceID, containerID) })
	return r
}

// isShutdown reports whether Shutdown has been called.
func (l *Streams) isShutdown() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.shutdown
}

// Close stops reading instanceID's log and sampling its stats. The ring buffer stays, so the
// console that explains why the server stopped is still readable (14 §8).
func (l *Streams) Close(instanceID string) {
	l.openMu.Lock()
	defer l.openMu.Unlock()
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

// Remove stops instanceID's sources and forgets them, ring included: for an instance that no
// longer exists.
func (l *Streams) Remove(instanceID string) {
	l.Close(instanceID)
	l.mu.Lock()
	delete(l.readers, instanceID)
	delete(l.samplers, instanceID)
	l.mu.Unlock()
}

// Instances lists the instances that have a reader or a sampler.
func (l *Streams) Instances() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	ids := make([]string, 0, len(l.readers))
	for id := range l.readers {
		ids = append(ids, id)
	}
	for id := range l.samplers {
		if _, ok := l.readers[id]; !ok {
			ids = append(ids, id)
		}
	}
	return ids
}

// Shutdown stops every source. Nothing outlives the process: buffers are empty after a daemon
// restart and stream.reset covers it (14 §8).
func (l *Streams) Shutdown() {
	l.mu.Lock()
	l.shutdown = true
	ids := make([]string, 0, len(l.readers))
	for id := range l.readers {
		ids = append(ids, id)
	}
	l.mu.Unlock()
	for _, id := range ids {
		l.Close(id)
	}
}

// halt stops the read loop and waits for it, so a re-open cannot leave two goroutines
// appending to one ring.
func (r *Reader) halt() {
	// A code names a live crossplay session, so one nobody is reading a log for is an
	// invitation to nothing. Cleared here rather than at the next open: a stopped server
	// would otherwise keep showing the code of the session it no longer runs (Q25).
	r.setJoinCode("")
	r.loop.halt()
}

// loop runs one goroutine against one container at a time. The reader and the sampler each
// hold one, so both end the same way when their container stops.
type loop struct {
	mu     sync.Mutex
	stop   context.CancelFunc
	done   chan struct{}
	source string
}

// running returns the container the goroutine runs against, or "" when none runs.
func (l *loop) running() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.source
}

// start halts any running goroutine and runs run against containerID in a new one. Its
// context is process-scoped, not request-scoped: only halt ends it.
func (l *loop) start(containerID string, run func(ctx context.Context)) {
	l.halt()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	l.mu.Lock()
	l.stop, l.done, l.source = cancel, done, containerID
	l.mu.Unlock()

	go func() {
		defer func() {
			l.mu.Lock()
			if l.source == containerID {
				l.source = ""
			}
			l.mu.Unlock()
			close(done)
		}()
		run(ctx)
	}()
}

// halt stops the goroutine and waits for it, so a restart cannot leave two running.
func (l *loop) halt() {
	l.mu.Lock()
	stop, done := l.stop, l.done
	l.stop, l.done = nil, nil
	l.mu.Unlock()

	if stop == nil {
		return
	}
	stop()
	<-done
}
