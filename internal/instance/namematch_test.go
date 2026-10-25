package instance

import (
	"slices"
	"testing"
	"time"
)

var matchStart = time.Date(2026, 9, 8, 8, 30, 0, 0, time.UTC)

func after(seconds int) time.Time { return matchStart.Add(time.Duration(seconds) * time.Second) }

// guessed lists the "account/name" pairs a spawn credited, sorted.
func guessed(ids []PlayerIdentity) []string {
	out := make([]string, 0, len(ids))
	for i := range ids {
		out = append(out, ids[i].PlatformID+"/"+ids[i].Name)
	}
	slices.Sort(out)
	return out
}

func TestACharacterLineIsMeasuredAsLogged(t *testing.T) {
	tests := []struct {
		line, name, number string
	}{
		{"09/08/2026 08:30:44: Got character ZDOID from Troll : 1971202858:1", "Troll", "1971202858"},
		{"Got character ZDOID from Troll : 0:0", "Troll", "0"},
		{"Got character ZDOID from Mr : Hyde : 12:7", "Mr : Hyde", "12"},
		{"Got character ZDOID from Game server connected : 5:1", "Game server connected", "5"},
	}
	for _, tt := range tests {
		ev, ok := DefaultPatterns.Match(tt.line)
		if !ok || ev.Kind != EventCharacter {
			t.Errorf("Match(%q) = %v, %v; want a character event", tt.line, ev.Kind, ok)
			continue
		}
		if ev.Groups[1] != tt.name || ev.Groups[2] != tt.number {
			t.Errorf("Match(%q) = name %q number %q, want %q %q",
				tt.line, ev.Groups[1], ev.Groups[2], tt.name, tt.number)
		}
	}
}

func TestASoleWaitingAccountOwnsTheName(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))

	got := guessed(m.spawn("Troll", "100", after(20)))
	if want := []string{"Steam_1/Troll"}; !slices.Equal(got, want) {
		t.Errorf("spawn credited %v, want %v", got, want)
	}
	if got := m.spawn("Troll", "100", after(300)); got != nil {
		t.Errorf("a respawn on the same connection credited %v, want nothing", guessed(got))
	}
}

func TestTwoWaitingAccountsShareTheEvidenceUntilBothSpawned(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))
	m.connected("playfab/B", "Steam_2", after(5))

	if got, want := guessed(
		m.spawn("Troll", "100", after(20)),
	), []string{
		"Steam_1/Troll",
		"Steam_2/Troll",
	}; !slices.Equal(
		got,
		want,
	) {
		t.Errorf("first spawn credited %v, want %v", got, want)
	}
	if got, want := guessed(
		m.spawn("Newbald", "200", after(25)),
	), []string{
		"Steam_1/Newbald",
		"Steam_2/Newbald",
	}; !slices.Equal(
		got,
		want,
	) {
		t.Errorf("second spawn credited %v, want %v", got, want)
	}
	if got := m.spawn("Late", "300", after(30)); got != nil {
		t.Errorf("a spawn with nobody waiting credited %v", guessed(got))
	}
}

func TestADestroyedCharacterIsNotASpawn(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))

	if got := m.spawn("Troll", "0", after(20)); got != nil {
		t.Errorf("0:0 credited %v, want nothing", guessed(got))
	}
	if got := guessed(m.spawn("Troll", "100", after(21))); len(got) != 1 {
		t.Errorf("the real spawn credited %v after a 0:0, want one account", got)
	}
}

func TestARefusedSocketNeverOwnsAName(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))
	m.refused("playfab/A")

	if got := m.spawn("Troll", "100", after(20)); got != nil {
		t.Errorf("a refused account was credited: %v", guessed(got))
	}
}

func TestAnAccountThatWaitedTooLongIsForgotten(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))

	if got := m.spawn("Troll", "100", after(int(matchWindow/time.Second)+1)); got != nil {
		t.Errorf("an account past the window was credited: %v", guessed(got))
	}
}

func TestAReconnectIsCreditedOnce(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))
	m.connected("playfab/B", "Steam_1", after(30))

	if got := guessed(m.spawn("Troll", "100", after(40))); len(got) != 1 {
		t.Errorf("a reconnecting account was credited %v, want once", got)
	}
}

// TestSpawnsBeforeTheOldestWaitingAccountDoNotCount. Two accounts that never both spawned age
// out; the pair that connected afterwards must still get a credit for each of its two spawns.
func TestSpawnsBeforeTheOldestWaitingAccountDoNotCount(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))
	m.connected("playfab/B", "Steam_2", after(5))
	m.spawn("Alice", "100", after(20))

	late := int(matchWindow/time.Second) + 10
	m.connected("playfab/C", "Steam_3", after(late))
	m.connected("playfab/D", "Steam_4", after(late+1))

	if got, want := guessed(
		m.spawn("Carl", "200", after(late+30)),
	), []string{
		"Steam_3/Carl",
		"Steam_4/Carl",
	}; !slices.Equal(
		got,
		want,
	) {
		t.Fatalf("first late spawn credited %v, want %v", got, want)
	}
	if got, want := guessed(
		m.spawn("Dora", "300", after(late+40)),
	), []string{
		"Steam_3/Dora",
		"Steam_4/Dora",
	}; !slices.Equal(
		got,
		want,
	) {
		t.Errorf("second late spawn credited %v, want %v", got, want)
	}
}

// TestARefusedAccountLeavesNobodyWaitingForAnAlreadyCountedSpawn. A spawned while B's socket
// waited; once B is refused, A has spawned and must not collect B's name after the retry.
func TestARefusedAccountLeavesNobodyWaitingForAnAlreadyCountedSpawn(t *testing.T) {
	var m nameMatcher
	m.connected("playfab/A", "Steam_1", after(0))
	m.connected("playfab/B1", "Steam_2", after(5))
	m.spawn("Alf", "100", after(20))
	m.refused("playfab/B1")
	m.connected("playfab/B2", "Steam_2", after(30))

	if got, want := guessed(m.spawn("Bea", "200", after(50))), []string{"Steam_2/Bea"}; !slices.Equal(got, want) {
		t.Errorf("the retry's spawn credited %v, want %v", got, want)
	}
}

func TestAReopenedStreamForgetsWhoWasWaiting(t *testing.T) {
	r := newReader(DefaultPatterns)
	r.names.connected("playfab/A", "Steam_1", after(0))
	r.opened(after(10))

	if got := r.names.spawn("Troll", "100", after(20)); got != nil {
		t.Errorf("a re-opened stream credited %v, want nothing", guessed(got))
	}
}

func TestAConnectionNumberMayBeNegative(t *testing.T) {
	ev, ok := DefaultPatterns.Match("Got character ZDOID from Troll : -1971202858:1")
	if !ok || ev.Kind != EventCharacter || ev.Groups[2] != "-1971202858" {
		t.Errorf("Match = %v %v %q, want a character event numbered -1971202858", ev.Kind, ok, ev.Groups)
	}
}

func TestOnlyLiveLinesReachTheMatcher(t *testing.T) {
	var got []PlayerIdentity
	r := newReader(DefaultPatterns)
	r.openedAt = after(100)
	r.onIdentity = func(id PlayerIdentity) { got = append(got, id) }

	feed := func(line string, ts time.Time) {
		ev, ok := DefaultPatterns.Match(line)
		if !ok {
			t.Fatalf("%q matched nothing", line)
		}
		r.matched(ev, ts)
	}
	const socket = "PlayFab socket with remote ID playfab/A received local Platform ID Steam_1"
	const spawn = "Got character ZDOID from Troll : 100:1"

	feed(socket, after(0))
	feed(spawn, after(20))
	for _, id := range got {
		if id.Guessed {
			t.Fatalf("a re-read line produced the guess %+v", id)
		}
	}
	feed(socket, after(110))
	feed(spawn, after(130))
	if last := got[len(got)-1]; !last.Guessed || last.PlatformID != "Steam_1" || last.Name != "Troll" {
		t.Errorf("last announcement = %+v, want the live guess Steam_1/Troll", last)
	}
}

func TestAJoinEventNamesItsAccountExactly(t *testing.T) {
	var got []PlayerIdentity
	r := newReader(DefaultPatterns)
	r.openedAt = after(0)
	r.onIdentity = func(id PlayerIdentity) { got = append(got, id) }

	ev, ok := DefaultPatterns.Match(`[Info   : ValminEye] {"v":1,"event":"join","player":"Alex","id":"Steam_7"}`)
	if !ok {
		t.Fatal("the join line matched nothing")
	}
	r.matched(ev, after(10))
	want := PlayerIdentity{TS: after(10), PlatformID: "Steam_7", Name: "Alex"}
	if len(got) != 1 || got[0] != want {
		t.Errorf("announced %+v, want %+v", got, want)
	}
}

func TestTheServerBotHistoryEntryNamesNobody(t *testing.T) {
	if ev, ok := DefaultPatterns.Match(
		"10/25/2026 10:36:26: Player history entry with index 1:  Server (Bot_0, )",
	); ok {
		t.Errorf("the bot entry matched %v, want no account", ev.Kind)
	}
}
