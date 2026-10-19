package control

import (
	"testing"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/store"
)

// Asserts provision, clone and start build the same container spec for one instance row, so
// the first start after a provision or a clone never rebuilds the container as drifted.
func TestEveryPathBuildsOneSpecForOneRow(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	if _, err := w.db.Writer.ExecContext(t.Context(),
		`UPDATE instances SET game_backups = 7 WHERE id = ?`, seededInstanceID); err != nil {
		t.Fatal(err)
	}
	inst, err := w.db.InstanceByID(t.Context(), seededInstanceID)
	if err != nil {
		t.Fatal(err)
	}

	started, err := w.c.Starter.SpecFor(t.Context(), inst)
	if err != nil {
		t.Fatal(err)
	}
	provisioned, err := w.c.Provisioner.provisionSpec(
		t.Context(), provisionRunFor(inst, provisionPasswords{World: seededWorldPassword}))
	if err != nil {
		t.Fatal(err)
	}
	cloned, err := w.c.Cloner.cloneSpec(&CloneRun{Destination: inst, Password: seededWorldPassword})
	if err != nil {
		t.Fatal(err)
	}
	want := started.Labels[instance.LabelSpecHash]
	for name, spec := range map[string]map[string]string{"provision": provisioned.Labels, "clone": cloned.Labels} {
		if got := spec[instance.LabelSpecHash]; got != want {
			t.Errorf("%s spec hash = %s, want the start's %s", name, got, want)
		}
	}
}

// Asserts a clone keeps the source's rolling-save count and auto-stop setting.
func TestACloneKeepsTheSourcesSettings(t *testing.T) {
	w := newWorld(t)
	seedInstance(t, w, string(instance.StateStopped))
	if _, err := w.db.Writer.ExecContext(t.Context(),
		`UPDATE instances SET game_backups = 7, auto_stop_minutes = 30 WHERE id = ?`, seededInstanceID); err != nil {
		t.Fatal(err)
	}
	tx, err := w.db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := store.TxCreateCloneInstance(t.Context(), tx, seededInstanceID, &store.NewInstance{
		ID: "clone", Name: "clone", DataDir: instance.DataDir(w.cfg.Data.HostRoot, "clone"),
		BasePort: 2600, Password: "envelope", CrossplayInstanceID: "cp-clone",
	}); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	clone, err := w.db.InstanceByID(t.Context(), "clone")
	if err != nil {
		t.Fatal(err)
	}
	if clone.GameBackups != 7 || clone.AutoStopMinutes != 30 {
		t.Errorf("clone game_backups = %d, auto_stop_minutes = %d; want 7 and 30",
			clone.GameBackups, clone.AutoStopMinutes)
	}
}
