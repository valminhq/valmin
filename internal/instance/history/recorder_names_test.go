package history

import (
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/instance"
)

// TestAGuessCountsAHitAndAnExactNameDoesNot asserts the two kinds of evidence land in the one
// name row, and that a guess never touches the account's sighting.
func TestAGuessCountsAHitAndAnExactNameDoesNot(t *testing.T) {
	db := historyDB(t)
	p := New(db)
	base := time.Date(2026, 9, 8, 8, 0, 0, 0, time.UTC)

	p.writeIdentity(t.Context(), "inst-a", instance.PlayerIdentity{TS: base, PlatformID: "S1"})
	for i := range 3 {
		p.writeIdentity(t.Context(), "inst-a", instance.PlayerIdentity{
			TS: base.Add(time.Duration(i+1) * time.Hour), PlatformID: "S1", Name: "Troll", Guessed: true,
		})
	}
	p.writeIdentity(t.Context(), "inst-a", instance.PlayerIdentity{
		TS: base.Add(5 * time.Hour), PlatformID: "S1", Name: "Troll",
	})

	names, err := db.ListPlayerNames(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 1 || names[0].Hits != 3 || !names[0].Exact || !names[0].LastSeenAt.Equal(base.Add(5*time.Hour)) {
		t.Fatalf("names = %+v, want Troll with 3 hits, exact, last seen at +5h", names)
	}

	ids, err := db.ListPlayerIdentities(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 || ids[0].Name != "Troll" || !ids[0].LastSeenAt.Equal(base.Add(5*time.Hour)) {
		t.Fatalf("identities = %+v, want one account whose last sighting ignores the guesses", ids)
	}
}

func TestAGuessForAnUnseenAccountLeavesNoIdentityRow(t *testing.T) {
	db := historyDB(t)
	p := New(db)
	p.writeIdentity(t.Context(), "inst-a", instance.PlayerIdentity{
		TS: time.Now(), PlatformID: "S1", Name: "Troll", Guessed: true,
	})
	ids, err := db.ListPlayerIdentities(t.Context(), "inst-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("a guess created the account row %+v", ids)
	}
}
