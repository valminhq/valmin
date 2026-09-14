package backup

import "testing"

// Two layouts exist in the field: the `.db`/`.fwl` pair every build before 1.0 wrote, and the
// directory of `_main.<gen>.db2`/`.fwl2` plus chunks that build 25253791 writes
// (evidence/world-format-1.0-2026-09-14.md). One classifier reads both, because every caller
// that asks "is this world here" has to answer for a host that holds both.
func TestClassifyWorldFileReadsBothLayouts(t *testing.T) {
	for _, tc := range []struct {
		rel   string
		dir   string
		world string
		part  WorldPart
	}{
		// Pre-1.0.
		{"worlds_local/World2.db", "worlds_local", "World2", PartData},
		{"worlds_local/World2.fwl", "worlds_local", "World2", PartHeader},
		{"World2.db", "", "World2", PartData},
		// 1.0: the world is the directory, and the generation counter moves.
		{"worlds_local/Worild1/_main.14.db2", "worlds_local", "Worild1", PartData},
		{"worlds_local/Worild1/_main.14.fwl2", "worlds_local", "Worild1", PartHeader},
		{"worlds_local/Worild1/_main.9.db2", "worlds_local", "Worild1", PartData},

		// Neither half, in both layouts.
		{"worlds_local/World2.db.old", "", "", PartOther},
		{"worlds_local/World2.fwl.old", "", "", PartOther},
		{"worlds_local/Worild1/_main.14.chunks", "", "", PartOther},
		{"worlds_local/Worild1/_main.14.ok", "", "", PartOther},
		{"worlds_local/Worild1/20_20__1_9.chunk", "", "", PartOther},
		{"worlds_local/adminlist.txt", "", "", PartOther},
		{"cache/Worild1_biomedatacache.bin", "", "", PartOther},

		// The game's own rolling saves sit beside the world in both layouts and are named
		// after it. They must never satisfy the pair check on its behalf (03 §4.1 rule 5).
		{
			"worlds_local/World2_backup_auto-20260903073824.db", "worlds_local",
			"World2_backup_auto-20260903073824", PartData,
		},
		{
			"worlds_local/Worild1_backup_auto-20260913-204651/_main.10.db2", "worlds_local",
			"Worild1_backup_auto-20260913-204651", PartData,
		},

		// A `_main.<gen>.db2` with no directory over it belongs to no named world.
		{"_main.14.db2", "", "", PartOther},
	} {
		dir, world, part := ClassifyWorldFile(tc.rel)
		if dir != tc.dir || world != tc.world || part != tc.part {
			t.Errorf("ClassifyWorldFile(%q) = %q, %q, %v; want %q, %q, %v",
				tc.rel, dir, world, part, tc.dir, tc.world, tc.part)
		}
	}
}

// A world needs both halves whichever layout it is in, and the two layouts are never mixed
// into one world: half a pair beside half a directory is two broken worlds, not one whole one.
func TestWorldScanNeedsBothHalves(t *testing.T) {
	scan := WorldScan{}
	scan.Add("worlds_local/Worild1/_main.14.db2", 147000)
	if scan.Complete("Worild1") {
		t.Error("a world with no header read as complete")
	}
	scan.Add("worlds_local/Worild1/_main.14.fwl2", 113)
	if !scan.Complete("Worild1") {
		t.Error("a 1.0 world with both halves read as incomplete")
	}
	if !scan["Worild1"].Directory {
		t.Error("a 1.0 world was not recognised as the directory layout")
	}

	legacy := WorldScan{}
	legacy.Add("worlds_local/World2.db", 4096)
	legacy.Add("worlds_local/World2.fwl", 32)
	if !legacy.Complete("World2") || legacy["World2"].Directory {
		t.Error("a pre-1.0 pair did not read as a complete legacy world")
	}
}
