package store

import (
	"errors"
	"testing"
)

func TestInstanceByIDNeverCarriesThePassword(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2456)

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		t.Fatal("instance not found")
	}
	if inst.BasePort != 2456 {
		t.Errorf("base_port = %d, want 2456", inst.BasePort)
	}
	// Instance carries no password field at all — the struct cannot marshal one by
	// accident (11 §9), unlike a field merely tagged `json:"-"`.
}

func TestInstanceByIDMissingIsNilNotError(t *testing.T) {
	db := open(t)
	inst, err := db.InstanceByID(t.Context(), "no-such-id")
	if err != nil {
		t.Fatal(err)
	}
	if inst != nil {
		t.Errorf("got %+v, want nil", inst)
	}
}

func TestListInstancesNilListsEverything(t *testing.T) {
	db := open(t)
	seedInstance(t, db, NewID(), 2456)
	seedInstance(t, db, NewID(), 2461)

	got, err := db.ListInstances(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Errorf("got %d instances, want 2", len(got))
	}
}

func TestListInstancesFiltersByID(t *testing.T) {
	db := open(t)
	a := seedInstance(t, db, NewID(), 2456)
	seedInstance(t, db, NewID(), 2461) // not in ids

	got, err := db.ListInstances(t.Context(), []string{a})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != a {
		t.Errorf("got %+v, want just %s", got, a)
	}
}

// TestListInstancesEmptySliceListsNothing is the member-with-no-grants case: an empty,
// non-nil ids slice must not be read as "no filter".
func TestListInstancesEmptySliceListsNothing(t *testing.T) {
	db := open(t)
	seedInstance(t, db, NewID(), 2456)

	got, err := db.ListInstances(t.Context(), []string{})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Errorf("got %d instances, want 0", len(got))
	}
}

func TestInstancePasswordRoundTrips(t *testing.T) {
	db := open(t)
	id := NewID()
	exec(t, db.Writer, `
		INSERT INTO instances (
			id, name, state, data_dir, base_port, server_name, world_name, password,
			crossplay_instance_id, created_at, updated_at
		) VALUES (?, ?, 'stopped', ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, "inst-"+id, "/srv/valmin/instances/"+id, 2456,
		"Server", "World", "v1.envelope.opaque", "cp-"+id, Now(), Now())

	got, err := db.InstancePassword(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if got != "v1.envelope.opaque" {
		t.Errorf("got %q, want the stored envelope", got)
	}
}

func TestUsedBasePorts(t *testing.T) {
	db := open(t)
	seedInstance(t, db, NewID(), 2456)
	seedInstance(t, db, NewID(), 2461)

	used, err := db.UsedBasePorts(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !used[2456] || !used[2461] || used[2466] {
		t.Errorf("used = %v, want {2456,2461}", used)
	}
}

// TestPortReservationSurvivesAStateChange asserts that a failed provision leaves base_port
// and crossplay_instance_id reserved so a retry reuses them: both are plain columns on the
// durable instance row, so a state change alone can never lose them.
func TestPortReservationSurvivesAStateChange(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2456)

	ok, err := db.UpdateInstanceState(t.Context(), id, "stopped", "error")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected the CAS to succeed")
	}

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.BasePort != 2456 || inst.CrossplayInstanceID == "" {
		t.Errorf("got %+v, want the port and crossplay id still present", inst)
	}
}

func TestUpdateInstanceLimitsSetsRestartRequired(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2456)

	cpu := 2.5
	extra := "-logFile /dev/null"
	if err := db.UpdateInstanceLimits(t.Context(), id, InstanceLimits{
		MemLimitMB: 8192, CPULimit: &cpu, ExtraArgs: &extra,
	}); err != nil {
		t.Fatal(err)
	}

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.MemLimitMB != 8192 || inst.CPULimit == nil || *inst.CPULimit != 2.5 {
		t.Errorf("got %+v, want the new limits applied", inst)
	}
	if inst.ExtraArgs == nil || *inst.ExtraArgs != extra {
		t.Errorf("extra_args = %v, want %q", inst.ExtraArgs, extra)
	}
	if !inst.RestartRequired {
		t.Error("restart_required not set (12 §2.5)")
	}
}

func TestUpdateInstanceLimitsMissingInstance(t *testing.T) {
	db := open(t)
	err := db.UpdateInstanceLimits(t.Context(), "no-such-id", InstanceLimits{MemLimitMB: 4096})
	if err == nil {
		t.Fatal("want ErrInstanceNotFound")
	}
}

// TestUpdateInstanceStateIsCompareAndSwap is 12 §1's exactly-two-writers rule made
// concrete: a write only lands if the row is still where the caller last saw it.
func TestUpdateInstanceStateIsCompareAndSwap(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2456) // starts 'stopped'

	ok, err := db.UpdateInstanceState(t.Context(), id, "running", "stopping")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("CAS from the wrong current state must not apply")
	}

	ok, err = db.UpdateInstanceState(t.Context(), id, "stopped", "starting")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("CAS from the correct current state must apply")
	}

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "starting" {
		t.Errorf("state = %q, want starting", inst.State)
	}
}

func newInstance(id string, basePort int) *NewInstance {
	return &NewInstance{
		ID: id, Name: "inst-" + id, DataDir: "/srv/valmin/instances/" + id, BasePort: basePort,
		ServerName: "Server " + id, WorldName: "World" + id, Password: "v1.k.n.ct",
		CrossplayInstanceID: id, MemLimitMB: 4096,
	}
}

// TestCreateInstanceInsertsARowInCreated is 12 §2.1's entry point: the row a provision job
// then claims must already exist, already `created`, before the job ever sees it.
func TestCreateInstanceInsertsARowInCreated(t *testing.T) {
	db := open(t)
	id := NewID()
	if err := db.CreateInstance(t.Context(), newInstance(id, 2456)); err != nil {
		t.Fatal(err)
	}

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst == nil {
		t.Fatal("instance not found")
	}
	if inst.State != "created" {
		t.Errorf("state = %q, want created", inst.State)
	}
}

// TestCreateInstanceRejectsADuplicateName is the caller's own choice, so it is reported
// distinctly from a base_port collision (a panel-allocated value, never the caller's).
func TestCreateInstanceRejectsADuplicateName(t *testing.T) {
	db := open(t)
	first := newInstance(NewID(), 2456)
	if err := db.CreateInstance(t.Context(), first); err != nil {
		t.Fatal(err)
	}

	second := newInstance(NewID(), 2461)
	second.Name = first.Name
	err := db.CreateInstance(t.Context(), second)
	if !errors.Is(err, ErrInstanceNameTaken) {
		t.Errorf("err = %v, want ErrInstanceNameTaken", err)
	}
}

// TestCreateInstanceRejectsADuplicateBasePort is the allocator race backstop (A6): the DB's
// UNIQUE constraint is the final authority, not the allocator's own point-in-time check.
func TestCreateInstanceRejectsADuplicateBasePort(t *testing.T) {
	db := open(t)
	first := newInstance(NewID(), 2456)
	if err := db.CreateInstance(t.Context(), first); err != nil {
		t.Fatal(err)
	}

	second := newInstance(NewID(), 2456)
	err := db.CreateInstance(t.Context(), second)
	if !errors.Is(err, ErrBasePortTaken) {
		t.Errorf("err = %v, want ErrBasePortTaken", err)
	}
}

// TestTxUpdateInstanceStateAppliesWithinACallerTransaction is the seam a job's OnClaim/
// OnFinish hook needs (12 §6): the same CAS as UpdateInstanceState, but landing atomically
// with whatever else the caller's transaction does.
func TestTxUpdateInstanceStateAppliesWithinACallerTransaction(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2456) // starts 'stopped'

	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	ok, err := TxUpdateInstanceState(t.Context(), tx, id, "stopped", "starting")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("CAS from the correct current state must apply")
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "starting" {
		t.Errorf("state = %q, want starting", inst.State)
	}
}

// TestTxFinishProvisioningSetsStateAndContainer is the provision job's success path (12
// §6's Finish phase): the terminal state flip and the container id it produced land
// together, from data already in memory.
func TestTxFinishProvisioningSetsStateAndContainer(t *testing.T) {
	db := open(t)
	id := NewID()
	if err := db.CreateInstance(t.Context(), newInstance(id, 2456)); err != nil {
		t.Fatal(err)
	}
	if ok, err := db.UpdateInstanceState(t.Context(), id, "created", "provisioning"); err != nil || !ok {
		t.Fatalf("move to provisioning: ok=%v err=%v", ok, err)
	}

	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := TxFinishProvisioning(
		t.Context(),
		tx,
		id,
		"provisioning",
		"stopped",
		"container-123",
		"latest",
	); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	inst, err := db.InstanceByID(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if inst.State != "stopped" {
		t.Errorf("state = %q, want stopped", inst.State)
	}
	if inst.ContainerID == nil || *inst.ContainerID != "container-123" {
		t.Errorf("container_id = %v, want container-123", inst.ContainerID)
	}
	if inst.GameBuildID == nil || *inst.GameBuildID != "latest" {
		t.Errorf("game_build_id = %v, want latest", inst.GameBuildID)
	}
}

// TestTxFinishProvisioningFailsWhenNotInFromState guards against finishing a job whose
// instance moved out from under it — should never happen given C13's two-writer rule, but
// the CAS must still refuse rather than silently overwrite.
func TestTxFinishProvisioningFailsWhenNotInFromState(t *testing.T) {
	db := open(t)
	id := seedInstance(t, db, NewID(), 2456) // 'stopped', not 'provisioning'

	tx, err := db.Writer.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()

	if err := TxFinishProvisioning(
		t.Context(),
		tx,
		id,
		"provisioning",
		"stopped",
		"container-123",
		"latest",
	); err == nil {
		t.Error("want an error when the instance is not in the expected from-state")
	}
}
