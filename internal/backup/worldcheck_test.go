package backup

import (
	"bytes"
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"strings"
	"testing"
)

// measuredDir is where measuredWorld puts its world, as the game lays it out under the savedir.
const measuredDir = "worlds_local/Builders/"

// measuredWorld builds a 1.0 world in the layout measured on world versions 41 and 42: an `.ok`
// holding the version, a `.fwl2` with a length prefix then the version, a `.db2` of version,
// world time, gzip length, gzip body and a 40-byte tail, and a chunk index whose entries name
// chunk files and repeat the count each chunk carries at offset 2.
func measuredWorld(t *testing.T, version int32) map[string]string {
	t.Helper()
	le := binary.LittleEndian
	u32 := func(v uint32) []byte { return le.AppendUint32(nil, v) }

	fwl2 := append(u32(uint32(version)), append([]byte{8}, "Builders"...)...)
	fwl2 = append(u32(uint32(len(fwl2))), fwl2...)

	var body bytes.Buffer
	zw := gzip.NewWriter(&body)
	if _, err := zw.Write(bytes.Repeat([]byte("zdo "), 2000)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	db2 := u32(uint32(version))
	db2 = le.AppendUint64(db2, 0x4111d00000000000)
	db2 = append(db2, u32(uint32(body.Len()))...)
	db2 = append(append(db2, body.Bytes()...), make([]byte, 40)...)

	world := map[string]string{
		measuredDir + "_main.406.ok":   string(u32(uint32(version))),
		measuredDir + "_main.406.fwl2": string(fwl2),
		measuredDir + "_main.406.db2":  string(db2),
		"worlds_local/adminlist.txt":   "// List admin players ID  ONE per line",
	}

	// Two chunks: (x=0x1e, y=0x20) last written in generation 9 with 2145 objects, and (0, 0)
	// in generation 405 with 81. The index stores the low coordinate byte first.
	type entry struct {
		lo, hi, kind byte
		gen, count   uint32
	}
	entries := []entry{{0x20, 0x1e, 1, 9, 2145}, {0x00, 0x00, 0, 405, 81}}
	index := le.AppendUint16(nil, uint16(version))
	index = append(index, u32(2145+81)...)
	index = append(index, u32(uint32(len(entries)))...)
	for _, e := range entries {
		index = append(index, e.lo, e.hi, e.kind)
		index = append(index, u32(e.gen)...)
		index = append(index, u32(e.count)...)
		chunk := le.AppendUint16(nil, 41)
		chunk = append(chunk, u32(e.count)...)
		chunk = append(chunk, bytes.Repeat([]byte{7}, 500)...)
		world[fmt.Sprintf("%s%02x_%02x__%d_%d.chunk", measuredDir, e.hi, e.lo, e.kind, e.gen)] = string(chunk)
	}
	world[measuredDir+"_main.406.chunks"] = string(index)
	return world
}

// codes is the attention codes Verify reported, in order.
func codes(v *Verified) []AttentionCode {
	out := make([]AttentionCode, 0, len(v.Attention))
	for _, a := range v.Attention {
		out = append(out, a.Code)
	}
	return out
}

func TestWorldCheckPassesAMeasuredWorld(t *testing.T) {
	for _, version := range []int32{41, 42} {
		got, err := Verify(archiveOf(t, measuredWorld(t, version)), "Builders")
		if err != nil {
			t.Fatalf("Verify(version %d): %v", version, err)
		}
		if len(got.Attention) != 0 || got.Unchecked != "" {
			t.Errorf("Verify(version %d) = %+v, want a world with nothing to look at", version, got)
		}
	}
}

// Each case damages a healthy world the way a save interrupted by a crash or a full disk can,
// and the check must name it. None of them refuses the archive.
func TestWorldCheckFlagsADamagedWorld(t *testing.T) {
	const chunk = measuredDir + "1e_20__1_9.chunk"
	for _, tc := range []struct {
		name   string
		damage func(w map[string]string)
		want   AttentionCode
	}{
		{"no .ok", func(w map[string]string) { delete(w, measuredDir+"_main.406.ok") }, AttentionSaveIncomplete},
		{"no chunk index", func(w map[string]string) { delete(w, measuredDir+"_main.406.chunks") }, AttentionSaveIncomplete},
		{"a newer generation left half written", func(w map[string]string) {
			w[measuredDir+"_main.407.db2"] = w[measuredDir+"_main.406.db2"]
			w[measuredDir+"_main.407.fwl2"] = w[measuredDir+"_main.406.fwl2"]
		}, AttentionSaveIncomplete},
		{"a short .ok", func(w map[string]string) { w[measuredDir+"_main.406.ok"] = "\x2a\x00" }, AttentionSaveIncomplete},
		{"the .db2 disagrees on the version", func(w map[string]string) {
			db2 := []byte(w[measuredDir+"_main.406.db2"])
			db2[0] = 40
			w[measuredDir+"_main.406.db2"] = string(db2)
		}, AttentionVersionMismatch},
		{"a chunk is missing", func(w map[string]string) { delete(w, chunk) }, AttentionChunkMissing},
		{"a chunk is empty", func(w map[string]string) { w[chunk] = "" }, AttentionChunkMissing},
		{"a chunk is not the one indexed", func(w map[string]string) {
			c := []byte(w[chunk])
			c[2]++
			w[chunk] = string(c)
		}, AttentionChunkMissing},
		{"the index is cut short", func(w map[string]string) {
			i := w[measuredDir+"_main.406.chunks"]
			w[measuredDir+"_main.406.chunks"] = i[:len(i)-3]
		}, AttentionChunkMissing},
		{"the index total disagrees with its entries", func(w map[string]string) {
			i := []byte(w[measuredDir+"_main.406.chunks"])
			i[2]++
			w[measuredDir+"_main.406.chunks"] = string(i)
		}, AttentionChunkMissing},
		{"the .db2 is truncated", func(w map[string]string) {
			db2 := w[measuredDir+"_main.406.db2"]
			w[measuredDir+"_main.406.db2"] = db2[:len(db2)/2]
		}, AttentionDB2Damaged},
		{"the .db2 body is corrupt", func(w map[string]string) {
			db2 := []byte(w[measuredDir+"_main.406.db2"])
			db2[len(db2)-40-5] ^= 0xff // inside the gzip trailer's CRC32
			w[measuredDir+"_main.406.db2"] = string(db2)
		}, AttentionDB2Damaged},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := measuredWorld(t, 42)
			tc.damage(world)
			got, err := Verify(archiveOf(t, world), "Builders")
			if err != nil {
				t.Fatalf("Verify refused the archive: %v", err)
			}
			if c := codes(&got); len(c) != 1 || c[0] != tc.want {
				t.Fatalf("Verify found %v (%+v), want exactly %s", c, got.Attention, tc.want)
			}
		})
	}
}

// A version whose layout was never measured is not checked against the measured one: that would
// flag every healthy world the day the game changes its format.
func TestWorldCheckSkipsAnUnmeasuredVersion(t *testing.T) {
	world := measuredWorld(t, 43)
	delete(world, measuredDir+"1e_20__1_9.chunk")

	got, err := Verify(archiveOf(t, world), "Builders")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Attention) != 0 {
		t.Errorf("an unmeasured version was checked: %+v", got.Attention)
	}
	if !strings.Contains(got.Unchecked, "43") {
		t.Errorf("Unchecked = %q, want it to name version 43", got.Unchecked)
	}
}

// The game's own rolling saves sit beside the world and hold a save of their own; damage there
// is not damage to the world.
func TestWorldCheckLooksOnlyAtTheWorld(t *testing.T) {
	world := measuredWorld(t, 42)
	world["worlds_local/Builders_backup_auto-20261016-132557/_main.353.db2"] = "\x29\x00\x00\x00short"
	world["worlds_local/Builders_backup_auto-20261016-132557/_main.353.fwl2"] = "fwl2"

	got, err := Verify(archiveOf(t, world), "Builders")
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if len(got.Attention) != 0 {
		t.Errorf("a rolling save's damage was reported for the world: %+v", got.Attention)
	}
}

func TestWorldCheckComparesAPairWithItsOldCopy(t *testing.T) {
	for _, tc := range []struct {
		name      string
		db, dbOld int
		flagged   bool
	}{
		// The classic signature: the game wrote a fresh or truncated world over a full one.
		{"under half of the old copy", 2000, 15000, true},
		// Measured on a healthy world: 14.6 KB against an 18.8 KB `.db.old`.
		{"a fifth smaller than the old copy", 14668, 18778, false},
		{"larger than the old copy", 20000, 18000, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			world := plausibleWorld()
			world["worlds_local/"+fixtureWorld+".db"] = strings.Repeat("d", tc.db)
			world["worlds_local/"+fixtureWorld+".db.old"] = strings.Repeat("o", tc.dbOld)
			got, err := Verify(archiveOf(t, world), fixtureWorld)
			if err != nil {
				t.Fatalf("Verify: %v", err)
			}
			flagged := len(got.Attention) == 1 && got.Attention[0].Code == AttentionDBShorterThanOld
			if flagged != tc.flagged || (!tc.flagged && len(got.Attention) != 0) {
				t.Errorf("Verify found %+v, want flagged=%v", got.Attention, tc.flagged)
			}
		})
	}
}

func TestVerifyMeasuresWhatTheWorldOwns(t *testing.T) {
	t.Run("a pair counts its two files and not the old copies", func(t *testing.T) {
		world := plausibleWorld()
		world["worlds_local/"+fixtureWorld+".db.old"] = strings.Repeat("o", 9000)
		world["worlds_local/"+fixtureWorld+".fwl.old"] = "old header"
		got, err := Verify(archiveOf(t, world), fixtureWorld)
		if err != nil {
			t.Fatal(err)
		}
		want := int64(len(world["worlds_local/"+fixtureWorld+".db"]) + len(world["worlds_local/"+fixtureWorld+".fwl"]))
		if got.WorldBytes != want {
			t.Errorf("WorldBytes = %d, want %d", got.WorldBytes, want)
		}
	})
	t.Run("a 1.0 world counts its directory and not its neighbours", func(t *testing.T) {
		world := measuredWorld(t, 42)
		var want int64
		for name, body := range world {
			if strings.HasPrefix(name, measuredDir) {
				want += int64(len(body))
			}
		}
		world["worlds_local/Builders_backup_auto-20261016-132557/_main.353.db2"] = strings.Repeat("d", 5000)
		world["cache/Builders_biomedatacache.bin"] = strings.Repeat("c", 5000)
		got, err := Verify(archiveOf(t, world), "Builders")
		if err != nil {
			t.Fatal(err)
		}
		if got.WorldBytes != want {
			t.Errorf("WorldBytes = %d, want %d", got.WorldBytes, want)
		}
	})
}

func TestShrunk(t *testing.T) {
	for _, tc := range []struct {
		bytes, baseline int64
		want            bool
	}{
		{0, 0, false},
		{100, 0, false},
		{0, 100, true},
		{49, 100, true},
		{50, 100, false},
		{80, 100, false},
		{200, 100, false},
		// The FreshWorld zone reset measured on a real server: 4.0 MB of chunks against 17.8 MB.
		{4_007_665, 17_825_765, true},
	} {
		if got := Shrunk(tc.bytes, tc.baseline); got != tc.want {
			t.Errorf("Shrunk(%d, %d) = %v, want %v", tc.bytes, tc.baseline, got, tc.want)
		}
	}
}

func FuzzWorldCheck(f *testing.F) {
	f.Add(
		[]byte("\x2a\x00\x00\x00"),
		[]byte("\x2a\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x00\x05\x00\x00\x00junk"),
		[]byte("\x2a\x00\x01\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x09\x00\x00\x00\x01\x00\x00\x00"),
	)
	f.Fuzz(func(t *testing.T, ok, db2, index []byte) {
		c := newWorldCheck("W")
		for name, body := range map[string][]byte{
			"W/_main.1.ok": ok, "W/_main.1.db2": db2, "W/_main.1.chunks": index,
			"W/_main.1.fwl2": ok, "W/00_00__0_9.chunk": index,
		} {
			if err := c.add(name, int64(len(body)), bytes.NewReader(body)); err != nil {
				t.Fatalf("add(%s): %v", name, err)
			}
		}
		c.attention("", true)
	})
}
