package instance

import (
	"bufio"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// capturedSession is one real client on a real modded crossplay server, `docker logs` verbatim
// with four values substituted. It is the only measured player traffic the panel has, and every
// assertion below cites a clock time inside it rather than a line the test invented.
const capturedSession = "testdata/crossplay-session.log"

func captured(t *testing.T) []string {
	t.Helper()
	f, err := os.Open(capturedSession)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()

	var lines []string
	scan := bufio.NewScanner(f)
	scan.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for scan.Scan() {
		lines = append(lines, scan.Text())
	}
	if err := scan.Err(); err != nil {
		t.Fatal(err)
	}
	return lines
}

// replay folds the whole capture through a fresh tracker and returns every value it published.
func replay(t *testing.T, lines []string) []*int {
	t.Helper()
	var (
		p   playerCount
		out []*int
	)
	for _, raw := range lines {
		ev, ok := DefaultPatterns.Match(raw)
		if !ok {
			continue
		}
		if players, changed := p.apply(ev); changed {
			out = append(out, players)
		}
	}
	return out
}

func show(players []*int) string {
	parts := make([]string, 0, len(players))
	for _, n := range players {
		if n == nil {
			parts = append(parts, "unknown")
			continue
		}
		parts = append(parts, fmt.Sprint(*n))
	}
	return strings.Join(parts, " → ")
}

// TestTheCountFollowsTheCapturedSession replays the whole capture and asserts the exact
// sequence of values, including the unknown the session ends on: the timeout at 08:47:47 is
// followed by no count line of any kind, so the last thing the panel can honestly say is that
// it no longer knows.
func TestTheCountFollowsTheCapturedSession(t *testing.T) {
	got := replay(t, captured(t))
	want := "0 → 1 → 0 → 1 → 0 → 1 → unknown"
	if show(got) != want {
		t.Fatalf("the derived count went %s, want %s", show(got), want)
	}
}

// TestARejectedLoginIsNeverAJoin is why the count line alone cannot drive an event. The
// capture holds three handshakes and two authenticated joins: the 08:29:22 attempt reached a
// handshake and was dropped at 08:29:24 with the wrong password.
func TestARejectedLoginIsNeverAJoin(t *testing.T) {
	seen := map[EventKind]int{}
	for _, raw := range captured(t) {
		if ev, ok := DefaultPatterns.Match(raw); ok {
			seen[ev.Kind]++
		}
	}
	if seen[EventPeerJoined] != 2 {
		t.Errorf("authenticated joins = %d, want 2", seen[EventPeerJoined])
	}
	if seen[EventPeerTimeout] != 1 {
		t.Errorf("timeouts = %d, want 1", seen[EventPeerTimeout])
	}
	// Two RPC_Disconnects: the rejected login and the clean leave at 08:39:18. The third
	// ending was the timeout, which prints none.
	if seen[EventPeerLeft] != 2 {
		t.Errorf("RPC_Disconnect lines = %d, want 2", seen[EventPeerLeft])
	}
}

// TestATransportBlipDoesNotDecrement is the 08:46:17 pair: the server logged a lost connection
// while keeping the socket, and its own count line still said one. Pairing joins with leaves
// would have decremented here; taking the number the line carries does not.
func TestATransportBlipDoesNotDecrement(t *testing.T) {
	var p playerCount
	for _, raw := range []string{
		`09/08/2026 08:42:29: Player joined server "ModdedTest" that has join code 111111, now 1 player(s)`,
		`09/08/2026 08:46:17: Keep socket for playfab/BFB3B9AADC0CDC42, try to reconnect before timeout`,
		`09/08/2026 08:46:17: Player connection lost server "ModdedTest" that has join code 111111, now 1 player(s)`,
	} {
		if ev, ok := DefaultPatterns.Match(raw); ok {
			p.apply(ev)
		}
	}
	if n := p.current(); n == nil || *n != 1 {
		t.Fatalf("players = %v after a blip that never dropped the player, want 1", show([]*int{p.current()}))
	}
}

// TestAnUnreadableCountIsNotZero. Every path that cannot state a number states nothing: a
// tracker that has seen no count line, and one whose evidence broke.
func TestAnUnreadableCountIsNotZero(t *testing.T) {
	var p playerCount
	if n := p.current(); n != nil {
		t.Fatalf("a tracker that has read nothing reports %d, want unknown", *n)
	}
	// A line that looks like the real one and carries no number matches nothing at all,
	// rather than matching and defaulting.
	if _, ok := DefaultPatterns.Match("now several player(s)"); ok {
		t.Error("a count line without a number matched")
	}

	ev, ok := DefaultPatterns.Match(`Player joined server "x" that has join code 1, now 2 player(s)`)
	if !ok {
		t.Fatal("the measured count line did not match")
	}
	p.apply(ev)
	timeout, _ := DefaultPatterns.Match("09/08/2026 08:47:47: ZRpc timeout detected")
	players, changed := p.apply(timeout)
	if !changed || players != nil {
		t.Fatalf("a timeout left the count at %v, want unknown", players)
	}
}

// TestTheTimestampPrefixDoesNotChangeTheResult is the two grammars of 03 §3.5: the game's own
// prefix is present on these lines and absent from BepInEx's, and Match strips it before
// matching.
func TestTheTimestampPrefixDoesNotChangeTheResult(t *testing.T) {
	bare := `Player joined server "ModdedTest" that has join code 111111, now 3 player(s)`
	for _, raw := range []string{bare, "09/08/2026 08:30:28: " + bare} {
		ev, ok := DefaultPatterns.Match(raw)
		if !ok || ev.Kind != EventPlayerCount || ev.Groups[1] != "3" {
			t.Fatalf("%q matched as %+v", raw, ev)
		}
	}
}

// TestTheSessionLineStaysTheJoinCode guards the one captured line that carries both a join code
// and a count. Match returns a single kind, and the join code is the one nothing else supplies
// (Q25); the count on it is redundant with the lines either side.
func TestTheSessionLineStaysTheJoinCode(t *testing.T) {
	raw := `09/08/2026 08:46:50: Session "ModdedTest" with join code 111111 and IP 203.0.113.10:2471 is active with 1 player(s)`
	ev, ok := DefaultPatterns.Match(raw)
	if !ok || ev.Kind != EventCrossplaySession {
		t.Fatalf("the session line matched as %q, want %q", ev.Kind, EventCrossplaySession)
	}
}

// TestAStreamResetInvalidatesTheCount is C20 at the reader: a stream that restarted saw none of
// what happened while it was closed, so it publishes an observation gap rather than carrying
// the old number across.
func TestAStreamResetInvalidatesTheCount(t *testing.T) {
	var got []PlayerObservation
	r := newReader()
	r.onPlayers = func(obs PlayerObservation) { got = append(got, obs) }

	at := time.Date(2026, 9, 8, 8, 30, 28, 0, time.UTC)
	r.append(Line{
		Stream: StreamStdout, TS: at,
		Text: `09/08/2026 08:30:28: Player joined server "x" that has join code 1, now 1 player(s)`,
	})
	if n := r.Players(); n == nil || *n != 1 {
		t.Fatalf("players = %v after a count line, want 1", n)
	}
	r.reset()

	if r.Players() != nil {
		t.Fatal("the count survived a stream reset")
	}
	if len(got) != 2 || got[1].Players != nil {
		t.Fatalf("observations = %v, want a value then a gap", got)
	}
	// Docker's receive time, not the reader's clock (14 §4.1).
	if !got[0].TS.Equal(at) {
		t.Errorf("the observation is stamped %s, want the line's own %s", got[0].TS, at)
	}
}
