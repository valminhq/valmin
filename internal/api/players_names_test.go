package api

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/store"
)

// recordNamed stores an account and the names it was seen with: the first exact, the second a
// guess with the given hits.
func recordNamed(t *testing.T, db *store.DB, platformID, exact, guess string, hits int, at time.Time) {
	t.Helper()
	row := store.PlayerIdentity{PlatformID: platformID, Name: exact, FirstSeenAt: at, LastSeenAt: at}
	if err := db.RecordPlayerIdentity(t.Context(), "inst-a", &row); err != nil {
		t.Fatal(err)
	}
	for _, n := range []store.PlayerName{
		{PlatformID: platformID, Name: exact, Exact: true, FirstSeenAt: at, LastSeenAt: at},
		{PlatformID: platformID, Name: guess, Hits: hits, FirstSeenAt: at, LastSeenAt: at},
	} {
		if err := db.RecordPlayerName(t.Context(), "inst-a", &n); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSeenPlayersListTheNamesToBelieveFirst(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, _ := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	recordNamed(t, db, "Steam_1", "Troll", "Newbald", 4, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))

	res := as(rt, admin, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/players/seen", http.NoBody))
	if res.Code != http.StatusOK {
		t.Fatalf("GET seen = %d, want 200 (%s)", res.Code, res.Body)
	}
	var page Page[seenPlayerView]
	decodeInto(t, res, &page)
	if len(page.Items) != 1 || len(page.Items[0].Names) != 2 {
		t.Fatalf("seen = %+v, want one account with two names", page.Items)
	}
	first, second := page.Items[0].Names[0], page.Items[0].Names[1]
	if first.Name != "Troll" || !first.Exact || !first.Likely {
		t.Errorf("first name = %+v, want the exact Troll", first)
	}
	if second.Name != "Newbald" || second.Exact || second.Hits != 4 || !second.Likely {
		t.Errorf("second name = %+v, want Newbald with 4 hits, believed", second)
	}
}

func TestHostPlayersGatherAccountsAcrossServersForAnAdminOnly(t *testing.T) {
	t.Parallel()
	rt, db, fake, admin, member := lifecycleWorld(t)
	seedInstance(t, rt, db, fake, "stopped")
	recordNamed(t, db, "Steam_1", "Troll", "Newbald", 1, time.Date(2026, 10, 11, 9, 0, 0, 0, time.UTC))
	recordNamed(t, db, "Steam_2", "Alex", "Bo", 1, time.Date(2026, 10, 11, 10, 0, 0, 0, time.UTC))

	res := as(rt, admin, httptest.NewRequest(http.MethodGet, "/api/v1/players", http.NoBody))
	if res.Code != http.StatusOK {
		t.Fatalf("GET players = %d, want 200 (%s)", res.Code, res.Body)
	}
	var page Page[hostPlayerView]
	decodeInto(t, res, &page)
	if len(page.Items) != 2 || page.Items[0].PlatformID != "Steam_2" || page.Items[1].PlatformID != "Steam_1" {
		t.Fatalf("players = %+v, want Steam_2 then Steam_1, newest first", page.Items)
	}
	troll := page.Items[1]
	if len(troll.Servers) != 1 || troll.Servers[0].InstanceID != "inst-a" || troll.Servers[0].Name != "Troll" {
		t.Errorf("servers = %+v, want the one server it was seen on", troll.Servers)
	}
	if len(troll.Names) != 2 || troll.Names[0].Name != "Troll" {
		t.Errorf("names = %+v, want the exact name first", troll.Names)
	}

	denied := as(rt, member, httptest.NewRequest(http.MethodGet, "/api/v1/players", http.NoBody))
	if denied.Code != http.StatusNotFound {
		t.Errorf("a member GET players = %d, want 404", denied.Code)
	}
}
