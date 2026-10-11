package control

import (
	"bytes"
	"context"
	"os"
	"testing"
	"time"

	"github.com/valminhq/valmin/internal/config"
	"github.com/valminhq/valmin/internal/crypto"
	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/jobs"
	"github.com/valminhq/valmin/internal/runtime"
	"github.com/valminhq/valmin/internal/store"
	"github.com/valminhq/valmin/internal/store/storetest"
)

// world is the components under test with the database, fake runtime and configuration they
// were built from.
type world struct {
	c      *Components
	db     *store.DB
	fake   *runtime.Fake
	cfg    *config.Config
	keeper *crypto.Keeper
}

// newWorld builds every component against a fresh database and a fake runtime, with readiness
// timings short enough that a start never waits out the real settle.
func newWorld(t *testing.T) *world {
	t.Helper()
	dir := t.TempDir()
	cfg := config.Defaults()
	cfg.Data.Root = dir
	cfg.Data.HostRoot = dir
	cfg.Jobs.ReadySettle = config.Duration(20 * time.Millisecond)
	cfg.Jobs.ReadyTimeout = config.Duration(2 * time.Second)

	keeper, err := crypto.NewKeeper(bytes.Repeat([]byte{7}, crypto.MasterKeyLen), []byte("salt"), "1")
	if err != nil {
		t.Fatal(err)
	}
	db := storetest.Open(t)
	engine := jobs.New(db, "test:"+store.NewID(), jobs.Config{
		LeaseTTL:         cfg.Jobs.LeaseTTL.Std(),
		ProgressInterval: cfg.Jobs.ProgressInterval.Std(),
		LogCap:           cfg.Jobs.LogCap,
		RetentionDays:    cfg.Jobs.RetentionDays,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		engine.Shutdown(ctx)
	})
	fake := runtime.NewFake()
	c, err := New(&cfg, &Deps{
		DB: db, Engine: engine, Runtime: fake, Keeper: keeper, Streams: instance.NewStreams(fake),
		Snapshotter: &Snapshotter{DataRoot: dir, Runtime: fake},
		ReadMods: func(ctx context.Context, inst *store.Instance) ([]store.InstanceMod, error) {
			return db.InstanceMods(ctx, inst.ID)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return &world{c: c, db: db, fake: fake, cfg: &cfg, keeper: keeper}
}

// seed runs one statement against the writer, failing the test on error.
func seed(t *testing.T, db *store.DB, query string, args ...any) {
	t.Helper()
	if _, err := db.Writer.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatalf("seed: %v", err)
	}
}

// The launch fields seedInstance's container and its row agree on.
const (
	seededInstanceID    = "inst-a"
	seededWorldPassword = "a-world-password"
	seededMemLimitMB    = 4096
	testContainerUser   = "10000:10000"
)

// seedInstance inserts inst-a in state with a fake container built from the same launch spec
// the row describes, so a start does not rebuild it as drifted, and returns the container id.
func seedInstance(t *testing.T, w *world, state string) string {
	t.Helper()
	dataDir := instance.DataDir(w.cfg.Data.HostRoot, seededInstanceID)
	spec, err := instance.BuildSpec(&instance.LaunchSpec{
		InstanceID: seededInstanceID, DataDir: dataDir, BasePort: 2456,
		ServerName: "Server", WorldName: "World", Password: seededWorldPassword,
		CrossplayInstanceID: "cp-inst-a", MemLimitMB: seededMemLimitMB,
	}, w.cfg.Game.Image, w.cfg.Game.Network, w.cfg.Game.StopTimeout.Std())
	if err != nil {
		t.Fatal(err)
	}
	containerID, err := w.fake.Create(t.Context(), spec)
	if err != nil {
		t.Fatal(err)
	}
	if state == string(instance.StateRunning) {
		if err := w.fake.Start(t.Context(), containerID); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	envelope, err := w.keeper.Encrypt(crypto.PurposeInstancePassword,
		crypto.InstancePasswordLocation(seededInstanceID), []byte(seededWorldPassword))
	if err != nil {
		t.Fatal(err)
	}
	seed(t, w.db, `INSERT INTO instances (
		id, name, state, container_id, data_dir, base_port, server_name, world_name, password,
		crossplay_instance_id, created_at, updated_at
	) VALUES ('inst-a', 'inst-a', ?, ?, ?, 2456, 'Server', 'World', ?, 'cp-inst-a', ?, ?)`,
		state, containerID, dataDir, envelope, store.Now(), store.Now())
	return containerID
}

// TestNewRefusesANonPositiveStopTimeout asserts that a stop timeout that would turn every stop
// into an immediate kill is refused when the components are built.
func TestNewRefusesANonPositiveStopTimeout(t *testing.T) {
	w := newWorld(t)
	cfg := *w.cfg
	cfg.Game.StopTimeout = 0
	_, err := New(&cfg, &Deps{
		DB: w.db, Engine: w.c.Starter.Engine, Runtime: w.fake, Keeper: w.keeper,
		Streams: w.c.Supervisor.Streams, Snapshotter: w.c.Snapshotter,
		ReadMods: func(context.Context, *store.Instance) ([]store.InstanceMod, error) { return nil, nil },
	})
	if err == nil {
		t.Fatal("New accepted a zero stop timeout")
	}
}
