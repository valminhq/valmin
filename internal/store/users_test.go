package store

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

// seedGrant inserts a grant, with expiresAt nil meaning no expiry.
func seedGrant(t *testing.T, db *DB, userID, instanceID string, role GrantRole, expiresAt *time.Time) {
	t.Helper()
	var expiry any
	if expiresAt != nil {
		expiry = FormatTime(*expiresAt)
	}
	exec(t, db.Writer, `
		INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at, expires_at)
		VALUES (?, ?, ?, '[]', ?, ?)`,
		userID, instanceID, string(role), Now(), expiry)
}

// TestExpiredGrantIsNoGrant is D11. Q10 defers the UI for setting an expiry; it has never
// deferred enforcement, and a column that silently never expires is worse than no column.
func TestExpiredGrantIsNoGrant(t *testing.T) {
	db := open(t)
	user := seedUser(t, db, "u1")
	instance := seedInstance(t, db, "i1", 2456)

	// One second in the past, which is the case 09 §4 names. The storage format that makes
	// this comparison sound has its own test — TestStoredTimeComparesInSQLite — because a
	// whole second's difference resolves before the fraction is ever reached.
	past := time.Now().Add(-time.Second)
	seedGrant(t, db, user, instance, GrantOperator, &past)

	got, err := db.GrantFor(t.Context(), user, instance)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a grant that expired one second ago was returned: %+v", got)
	}

	ids, err := db.GrantedInstances(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 0 {
		t.Errorf("an expired grant still lists its instance: %v", ids)
	}
}

func TestLiveGrantIsReturned(t *testing.T) {
	db := open(t)
	user := seedUser(t, db, "u1")
	never := seedInstance(t, db, "i1", 2456)
	later := seedInstance(t, db, "i2", 2461)

	future := time.Now().Add(time.Hour)
	seedGrant(t, db, user, never, GrantViewer, nil)
	seedGrant(t, db, user, later, GrantOperator, &future)

	for _, tc := range []struct {
		instance string
		want     GrantRole
	}{{never, GrantViewer}, {later, GrantOperator}} {
		got, err := db.GrantFor(t.Context(), user, tc.instance)
		if err != nil {
			t.Fatal(err)
		}
		if got == nil {
			t.Fatalf("no grant returned for %s", tc.instance)
		}
		if got.Role != tc.want {
			t.Errorf("grant role = %q, want %q", got.Role, tc.want)
		}
	}

	ids, err := db.GrantedInstances(t.Context(), user)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Errorf("live grants listed %v, want both instances", ids)
	}

	// A grant belongs to one user. A second member with none of their own must not read
	// through to the first's.
	other := seedUser(t, db, "u2")
	got, err := db.GrantFor(t.Context(), other, never)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Errorf("a second user read through to another's grant: %+v", got)
	}
	if ids, err = db.GrantedInstances(t.Context(), other); err != nil || len(ids) != 0 {
		t.Errorf("second user sees %v (err %v), want an empty dashboard", ids, err)
	}
}

func TestGrantPermsDecode(t *testing.T) {
	db := open(t)
	user := seedUser(t, db, "u1")
	instance := seedInstance(t, db, "i1", 2456)

	exec(t, db.Writer, `
		INSERT INTO instance_grants (user_id, instance_id, role, perms, granted_at)
		VALUES (?, ?, 'viewer', '["mods.manage","config.raw"]', ?)`,
		user, instance, Now())

	got, err := db.GrantFor(t.Context(), user, instance)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Perms) != 2 || got.Perms[0] != "mods.manage" || got.Perms[1] != "config.raw" {
		t.Errorf("perms = %v, want the two stored capabilities", got.Perms)
	}
}

// TestEveryGrantQueryFiltersExpiry is the structural half of D11: the filter has to be in
// the SQL, so a second query added later cannot quietly omit it. Behavioural tests prove
// the two queries that exist; this one guards the ones that do not yet.
func TestEveryGrantQueryFiltersExpiry(t *testing.T) {
	// Matches a SQL string literal that reads instance_grants.
	reads := regexp.MustCompile("(?s)`[^`]*FROM instance_grants[^`]*`")

	root := ".."
	err := filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		src, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, query := range reads.FindAllString(string(src), -1) {
			if !strings.Contains(query, "expires_at") {
				t.Errorf("%s reads instance_grants without filtering expires_at (D11, 09 §4):\n%s",
					path, query)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
