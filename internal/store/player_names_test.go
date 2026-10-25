package store

import "testing"

func TestPlayerNamesAddHitsAndKeepExact(t *testing.T) {
	t.Parallel()
	db := open(t)
	a := seedInstance(t, db, "a", 2456)
	b := seedInstance(t, db, "b", 2556)

	record := func(instanceID, platformID, name string, exact bool, hits, minute int) {
		t.Helper()
		n := PlayerName{
			PlatformID: platformID, Name: name, Exact: exact, Hits: hits,
			FirstSeenAt: at(minute), LastSeenAt: at(minute),
		}
		if err := db.RecordPlayerName(t.Context(), instanceID, &n); err != nil {
			t.Fatal(err)
		}
	}
	record(a, "S1", "Troll", false, 1, 5)
	record(a, "S1", "Troll", false, 1, 9)
	record(a, "S1", "Troll", true, 0, 2)
	record(b, "S1", "Troll", false, 1, 7)

	one, err := db.ListPlayerNames(t.Context(), a)
	if err != nil {
		t.Fatal(err)
	}
	if len(one) != 1 || one[0].Hits != 2 || !one[0].Exact ||
		!one[0].FirstSeenAt.Equal(at(2)) || !one[0].LastSeenAt.Equal(at(9)) {
		t.Fatalf("instance a names = %+v, want 2 hits, exact, seen from +2 to +9", one)
	}

	all, err := db.ListAllPlayerNames(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 1 || all[0].Hits != 3 || !all[0].Exact || !all[0].LastSeenAt.Equal(at(9)) {
		t.Fatalf("host names = %+v, want the two servers summed to 3 hits", all)
	}
}

func TestAllPlayerIdentitiesCarryTheirServer(t *testing.T) {
	t.Parallel()
	db := open(t)
	a := seedInstance(t, db, "a", 2456)
	b := seedInstance(t, db, "b", 2556)
	for i, id := range []string{a, b} {
		row := PlayerIdentity{PlatformID: "S1", Name: "Troll", FirstSeenAt: at(i), LastSeenAt: at(i)}
		if err := db.RecordPlayerIdentity(t.Context(), id, &row); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := db.ListAllPlayerIdentities(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].InstanceName != "inst-b" || rows[1].InstanceName != "inst-a" {
		t.Fatalf("rows = %+v, want the account on inst-b then inst-a, newest first", rows)
	}
}
