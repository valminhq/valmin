package store

import (
	"errors"
	"fmt"
	"testing"
)

// TestConfigRevisionsDedupeBaselineAndPrune asserts unchanged bytes are not recorded twice, the
// first revision is the baseline until a mod revision takes it over, and pruning keeps the
// newest revisions plus the baseline.
func TestConfigRevisionsDedupeBaselineAndPrune(t *testing.T) {
	t.Parallel()
	db := open(t)
	ctx := t.Context()
	id := seedInstance(t, db, "inst-a", 2456)
	record := func(source ConfigSource, body string) {
		t.Helper()
		err := db.RecordConfigRevisions(ctx, []ConfigRevision{
			{InstanceID: id, File: "a.cfg", Source: source, Content: []byte(body)},
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	baseline := func() string {
		t.Helper()
		r, err := db.ConfigBaseline(ctx, id, "a.cfg")
		if err != nil || r == nil {
			t.Fatalf("baseline = %v, %v", r, err)
		}
		return string(r.Content)
	}

	record(ConfigFound, "first")
	record(ConfigOutside, "first")
	if got, _ := db.ConfigRevisions(ctx, id, "a.cfg"); len(got) != 1 {
		t.Fatalf("revisions = %d, want unchanged bytes recorded once", len(got))
	}
	record(ConfigEdit, "edited")
	if got := baseline(); got != "first" {
		t.Errorf("baseline = %q, want the first revision", got)
	}
	record(ConfigMod, "the mod's")
	if got := baseline(); got != "the mod's" {
		t.Errorf("baseline = %q, want the mod's revision", got)
	}

	for i := range configRevisionsKept + 5 {
		record(ConfigEdit, fmt.Sprintf("edit %d", i))
	}
	got, err := db.ConfigRevisions(ctx, id, "a.cfg")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != configRevisionsKept+1 {
		t.Errorf("revisions = %d, want %d plus the baseline", len(got), configRevisionsKept)
	}
	if got[0].Source != ConfigEdit || got[0].Size != len(fmt.Sprintf("edit %d", configRevisionsKept+4)) {
		t.Errorf("newest = %+v, want the last edit", got[0])
	}
	if b := baseline(); b != "the mod's" {
		t.Errorf("baseline after pruning = %q, want it kept", b)
	}

	latest, err := db.LatestConfigRevisions(ctx, id)
	if err != nil || latest["a.cfg"].ID != got[0].ID {
		t.Errorf("latest = %+v, %v; want the newest revision", latest, err)
	}
	full, err := db.ConfigRevision(ctx, id, "a.cfg", got[0].ID)
	if err != nil || full == nil || len(full.Content) == 0 {
		t.Errorf("revision = %+v, %v; want its content", full, err)
	}
	if other, err := db.ConfigRevision(ctx, id, "b.cfg", got[0].ID); err != nil || other != nil {
		t.Errorf("revision under another file = %+v, %v; want nil", other, err)
	}
}

// TestConfigRevisionsRefuseAnOversizedFile asserts a file too large to keep is an error, so a
// write that needs its old version saved does not go ahead without it.
func TestConfigRevisionsRefuseAnOversizedFile(t *testing.T) {
	t.Parallel()
	db := open(t)
	id := seedInstance(t, db, "inst-a", 2456)
	err := db.RecordConfigRevisions(t.Context(), []ConfigRevision{{
		InstanceID: id, File: "big.json", Source: ConfigFound, Content: make([]byte, MaxConfigRevisionBytes+1),
	}})
	if !errors.Is(err, ErrConfigTooLarge) {
		t.Errorf("record = %v, want ErrConfigTooLarge", err)
	}
}

// TestConfigRevisionsCopyAndCascade asserts a copy skips the excluded file and an instance's
// revisions go with it.
func TestConfigRevisionsCopyAndCascade(t *testing.T) {
	t.Parallel()
	db := open(t)
	ctx := t.Context()
	src := seedInstance(t, db, "inst-a", 2456)
	dst := seedInstance(t, db, "inst-b", 2466)
	err := db.RecordConfigRevisions(ctx, []ConfigRevision{
		{InstanceID: src, File: "a.cfg", Source: ConfigFound, Content: []byte("a")},
		{InstanceID: src, File: "secret.cfg", Source: ConfigFound, Content: []byte("password")},
	})
	if err != nil {
		t.Fatal(err)
	}
	tx, err := db.Writer.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := TxCopyConfigRevisions(ctx, tx, src, dst, "secret.cfg"); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}
	latest, err := db.LatestConfigRevisions(ctx, dst)
	if err != nil || len(latest) != 1 || !latest["a.cfg"].Baseline {
		t.Errorf("copied = %+v, %v; want a.cfg alone, still the baseline", latest, err)
	}

	exec(t, db.Writer, `DELETE FROM instances WHERE id = ?`, src)
	if left, err := db.LatestConfigRevisions(ctx, src); err != nil || len(left) != 0 {
		t.Errorf("revisions after the instance went = %+v, %v", left, err)
	}
}
