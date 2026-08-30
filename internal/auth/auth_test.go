package auth

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

// fakeKV satisfies the KV interface without a database, for tests that only exercise
// argon2 parameter loading.
type fakeKV struct{ values map[string]string }

func (f *fakeKV) KVGet(_ context.Context, key string, v any) (bool, error) {
	raw, ok := f.values[key]
	if !ok {
		return false, nil
	}
	return true, json.Unmarshal([]byte(raw), v)
}

func (f *fakeKV) KVSet(_ context.Context, key string, v any) error {
	if f.values == nil {
		f.values = make(map[string]string)
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return err
	}
	f.values[key] = string(raw)
	return nil
}

// testDB returns a migrated, real database — the auth package takes *store.DB directly
// rather than a narrow interface (06 §4): the surface it needs is most of the store's
// users/sessions/invites/kv API, and a seam with one implementation is dead weight.
func testDB(t *testing.T) *store.DB {
	t.Helper()
	dsn := "file:" + filepath.Join(t.TempDir(), "panel.db")
	db, err := store.Open(t.Context(), "sqlite", dsn)
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	if err := store.Migrate(t.Context(), db.Writer); err != nil {
		t.Fatalf("store.Migrate: %v", err)
	}
	return db
}

// useFastArgon2Params points the panel's kv row at test-speed parameters, so the suite
// does not pay Decision 4's real 64 MiB / t=3 cost on every hash.
func useFastArgon2Params(t *testing.T, db *store.DB) {
	t.Helper()
	if err := db.KVSet(t.Context(), argon2ParamsKey, fastParams); err != nil {
		t.Fatal(err)
	}
}

// seedGrantableInstance inserts the row an invite's instance_id foreign key requires.
func seedGrantableInstance(t *testing.T, db *store.DB, id string) {
	t.Helper()
	_, err := db.Writer.ExecContext(t.Context(), `
		INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, 'stopped', ?, 2456, ?, ?, 'v1.k.n.ct', ?, ?, ?)`,
		id, id, "/srv/valmin/instances/"+id, "Server "+id, "World"+id, "cp-"+id,
		store.Now(), store.Now())
	if err != nil {
		t.Fatal(err)
	}
}
