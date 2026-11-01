package backup

import (
	"compress/gzip"
	"encoding/binary"
	"fmt"
	"io"
	"maps"
	"math/bits"
	"path"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// AttentionCode names one reason an archive's world deserves a look. Codes are stored with the
// archive and published in the API, so a code is never renamed.
type AttentionCode string

const (
	// AttentionWorldShrunk is a world under half the size of the last archive that passed.
	AttentionWorldShrunk AttentionCode = "world_shrunk"
	// AttentionDBShorterThanOld is a pre-1.0 `.db` under half the size of the `.db.old` the game
	// kept from its previous save.
	AttentionDBShorterThanOld AttentionCode = "db_shorter_than_old"
	// AttentionSaveIncomplete is a 1.0 world whose newest save generation is missing a file.
	AttentionSaveIncomplete AttentionCode = "save_incomplete"
	// AttentionVersionMismatch is a 1.0 save whose own files disagree on the world version.
	AttentionVersionMismatch AttentionCode = "version_mismatch"
	// AttentionChunkMissing is a 1.0 chunk index naming a chunk that is absent, empty or not the
	// one the index describes.
	AttentionChunkMissing AttentionCode = "chunk_missing"
	// AttentionDB2Damaged is a 1.0 `.db2` whose compressed body is cut short or corrupt.
	AttentionDB2Damaged AttentionCode = "db2_damaged"
)

// Attention is one finding of the world check. It never refuses an archive; it tells the
// operator to look at the server log before relying on it.
type Attention struct {
	Code    AttentionCode `json:"code"`
	Message string        `json:"message"`
}

// checkedVersions are the 1.0 world versions whose file layouts were measured. Content checks
// run only for these, since a later version may lay its files out differently.
var checkedVersions = []uint32{41, 42}

// Bounds on what the check reads into memory. A chunk index holds 11 bytes per chunk, and a
// world of a hundred thousand chunks is far beyond any measured one.
const (
	maxManifestBytes = 10 + 11*100_000
	db2HeaderBytes   = 16
	chunkHeaderBytes = 6
)

// mainGeneration matches a 1.0 save file and captures its generation and extension. The
// generation is bounded so it always parses as an int.
var mainGeneration = regexp.MustCompile(`^_main\.(\d{1,9})\.(db2|fwl2|chunks|ok)$`)

// worldCheck gathers what the check needs about one world while Verify walks the archive, so the
// archive is read once. Only small files and file heads are kept; a `.db2` is checked as it
// streams past.
type worldCheck struct {
	world string
	// dirs are the 1.0 world directories named after the world, by their archive path.
	dirs map[string]*worldDir
	// pairs are the pre-1.0 `.db` and `.db.old` sizes, by the directory holding them.
	pairs map[string]*pairSizes
}

// worldDir is one 1.0 world directory.
type worldDir struct {
	gens   map[int]*generation
	chunks map[string]chunkFile
}

// generation is one `_main.<gen>` save.
type generation struct {
	seen map[string]bool
	ok   []byte
	fwl2 []byte
	// manifest is the `.chunks` index, nil when it was too large to hold.
	manifest      []byte
	manifestBytes int64
	db2Header     []byte
	db2Err        error
}

// chunkFile is a chunk's size and the head the index is checked against.
type chunkFile struct {
	size int64
	head []byte
}

type pairSizes struct {
	db, dbOld int64
}

func newWorldCheck(world string) *worldCheck {
	return &worldCheck{world: world, dirs: map[string]*worldDir{}, pairs: map[string]*pairSizes{}}
}

// add records one archive entry. r reads its body; an error from r itself is the archive failing
// rather than the world, and is returned for Verify to report.
func (c *worldCheck) add(rel string, size int64, r io.Reader) error {
	rel = cleanRel(rel)
	base, parent := path.Base(rel), path.Dir(rel)

	switch base {
	case c.world + ".db":
		c.pair(parent).db = size
		return nil
	case c.world + ".db.old":
		c.pair(parent).dbOld = size
		return nil
	}
	if path.Base(parent) != c.world {
		return nil
	}

	dir := c.dirs[parent]
	if dir == nil {
		dir = &worldDir{gens: map[int]*generation{}, chunks: map[string]chunkFile{}}
		c.dirs[parent] = dir
	}
	if strings.HasSuffix(base, ".chunk") {
		head, err := readHead(r, chunkHeaderBytes)
		dir.chunks[base] = chunkFile{size: size, head: head}
		return err
	}
	m := mainGeneration.FindStringSubmatch(base)
	if m == nil {
		return nil
	}
	n, _ := strconv.Atoi(m[1])
	gen := dir.gens[n]
	if gen == nil {
		gen = &generation{seen: map[string]bool{}}
		dir.gens[n] = gen
	}
	gen.seen[m[2]] = true
	return gen.read(m[2], size, r)
}

func (c *worldCheck) pair(dir string) *pairSizes {
	p := c.pairs[dir]
	if p == nil {
		p = &pairSizes{db: -1, dbOld: -1}
		c.pairs[dir] = p
	}
	return p
}

// read keeps what the check needs of one `_main` file.
func (g *generation) read(ext string, size int64, r io.Reader) error {
	var err error
	switch ext {
	case "ok":
		g.ok, err = readHead(r, 8)
	case "fwl2":
		g.fwl2, err = readHead(r, 8)
	case "chunks":
		g.manifestBytes = size
		if size <= maxManifestBytes {
			g.manifest, err = readHead(r, int(size))
		}
	case "db2":
		g.db2Header, g.db2Err, err = checkDB2(r)
	}
	return err
}

// checkDB2 reads a `.db2` header and decompresses the body it announces: the version, the world
// time, the compressed length L, then L bytes of gzip. The first error is the world's, the
// second the archive's.
func checkDB2(r io.Reader) (header []byte, damaged, failed error) {
	src := &sourceReader{r: r}
	header, _ = readHead(src, db2HeaderBytes)
	if src.err != nil {
		return header, nil, src.err
	}
	if len(header) < db2HeaderBytes {
		return header, fmt.Errorf("it is %d bytes, shorter than its header", len(header)), nil
	}
	length := int64(binary.LittleEndian.Uint32(header[12:]))
	body := io.LimitReader(src, length)
	zr, err := gzip.NewReader(body)
	if err == nil {
		zr.Multistream(false)
		//nolint:gosec // Inflated into io.Discard, so nothing is held; the archive is the panel's own.
		_, err = io.Copy(io.Discard, zr)
	}
	if src.err != nil {
		return header, nil, src.err
	}
	if err != nil {
		return header, fmt.Errorf("its compressed world data does not decompress: %w", err), nil
	}
	return header, nil, nil
}

// sourceReader remembers an error from the archive itself, so a corrupt archive is not mistaken
// for a damaged world file inside it.
type sourceReader struct {
	r   io.Reader
	err error
}

func (s *sourceReader) Read(p []byte) (int, error) {
	n, err := s.r.Read(p)
	if err != nil && err != io.EOF {
		s.err = err
	}
	//nolint:wrapcheck // A reader passes its source's errors through, io.EOF included.
	return n, err
}

// readHead reads up to n bytes; a shorter body is returned as it is.
func readHead(r io.Reader, n int) ([]byte, error) {
	buf := make([]byte, n)
	got, err := io.ReadFull(r, buf)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		err = nil
	}
	return buf[:got], err
}

// attention evaluates what was gathered for the world at dir, the directory Verify found it in.
// unchecked is set when the world's 1.0 files are of a version whose layout is unmeasured.
func (c *worldCheck) attention(dir string, directory bool) (found []Attention, unchecked string) {
	if !directory {
		if p := c.pairs[cleanDirOf(dir)]; p != nil && p.dbOld > 0 && Shrunk(p.db, p.dbOld) {
			found = append(found, Attention{AttentionDBShorterThanOld, fmt.Sprintf(
				"%s.db is %s, under half of the %s the game kept from its previous save (%s.db.old)",
				c.world, formatBytes(p.db), formatBytes(p.dbOld), c.world)})
		}
		return found, ""
	}
	wd := c.dirs[path.Join(dir, c.world)]
	if wd == nil || len(wd.gens) == 0 {
		return nil, ""
	}
	return wd.attention()
}

// attention checks the newest save generation of a 1.0 world directory.
func (d *worldDir) attention() (found []Attention, unchecked string) {
	newest := slices.Max(slices.Collect(maps.Keys(d.gens)))
	gen := d.gens[newest]
	name := "_main." + strconv.Itoa(newest)

	version, ok := gen.version()
	if ok && !slices.Contains(checkedVersions, version) {
		return nil, fmt.Sprintf("world version %d has not been measured, so the save's files were not checked", version)
	}

	var missing []string
	for _, ext := range []string{"db2", "fwl2", "chunks", "ok"} {
		if !gen.seen[ext] {
			missing = append(missing, name+"."+ext)
		}
	}
	if len(missing) > 0 {
		return []Attention{{AttentionSaveIncomplete, fmt.Sprintf(
			"the newest save, generation %d, is missing %s", newest, strings.Join(missing, ", "))}}, ""
	}
	if len(gen.ok) != 4 {
		return []Attention{{AttentionSaveIncomplete, fmt.Sprintf(
			"%s.ok is %d bytes where the game writes 4", name, len(gen.ok))}}, ""
	}

	if a, bad := gen.versionMismatch(name); bad {
		found = append(found, a)
	}
	if a, bad := d.chunkProblems(name, gen); bad {
		found = append(found, a)
	}
	if gen.db2Err != nil {
		found = append(found, Attention{AttentionDB2Damaged, name + ".db2: " + gen.db2Err.Error()})
	}
	return found, ""
}

// version is the world version the save records, read from `.ok`, the file written last.
func (g *generation) version() (uint32, bool) {
	if len(g.ok) != 4 {
		return 0, false
	}
	return binary.LittleEndian.Uint32(g.ok), true
}

// versionMismatch compares the version each of the save's own files carries. Chunk files are
// left out: chunks a save did not rewrite keep the version they were written with.
func (g *generation) versionMismatch(name string) (Attention, bool) {
	want, _ := g.version()
	var wrong []string
	check := func(file string, got uint32, readable bool) {
		if !readable {
			wrong = append(wrong, file+" is too short to carry one")
		} else if got != want {
			wrong = append(wrong, fmt.Sprintf("%s says %d", file, got))
		}
	}
	check(name+".db2", le32(g.db2Header, 0), len(g.db2Header) >= 4)
	check(name+".fwl2", le32(g.fwl2, 4), len(g.fwl2) >= 8)
	check(name+".chunks", uint32(le16(g.manifest, 0)), len(g.manifest) >= 2)
	if len(wrong) == 0 {
		return Attention{}, false
	}
	return Attention{AttentionVersionMismatch, fmt.Sprintf(
		"%s.ok says world version %d, but %s", name, want, strings.Join(wrong, "; "))}, true
}

// maxChunksNamed bounds how many problem chunks a message lists.
const maxChunksNamed = 4

// chunkProblems checks the chunk index against the chunk files: the index is a uint16 version, a
// uint32 total of every chunk's count, a uint32 number of entries, then 11 bytes per entry. An
// entry names its file from its first seven bytes and repeats the count the chunk file carries
// at offset 2.
func (d *worldDir) chunkProblems(name string, gen *generation) (Attention, bool) {
	fail := func(format string, args ...any) (Attention, bool) {
		return Attention{AttentionChunkMissing, name + ".chunks: " + fmt.Sprintf(format, args...)}, true
	}
	m := gen.manifest
	if m == nil {
		return fail("the index is %s, larger than any world could need", formatBytes(gen.manifestBytes))
	}
	if len(m) < 10 {
		return fail("the index is %d bytes, shorter than its header", len(m))
	}
	count := int64(le32(m, 6))
	if int64(len(m)) != 10+11*count {
		return fail("the index lists %d chunks but is %d bytes long", count, len(m))
	}

	var total uint64
	var bad []string
	for i := range int(count) {
		e := m[10+11*i : 21+11*i]
		file := fmt.Sprintf("%02x_%02x__%d_%d.chunk", e[1], e[0], e[2], le32(e, 3))
		want := le32(e, 7)
		total += uint64(want)
		chunk, ok := d.chunks[file]
		switch {
		case !ok:
			bad = append(bad, file+" is missing")
		case chunk.size == 0:
			bad = append(bad, file+" is empty")
		case len(chunk.head) < chunkHeaderBytes || le32(chunk.head, 2) != want:
			bad = append(bad, file+" does not match its index entry")
		}
	}
	if len(bad) > 0 {
		more := ""
		if len(bad) > maxChunksNamed {
			more = fmt.Sprintf(" and %d more", len(bad)-maxChunksNamed)
			bad = bad[:maxChunksNamed]
		}
		return fail("%s%s", strings.Join(bad, ", "), more)
	}
	if total != uint64(le32(m, 2)) {
		return fail("the index's total %d does not match the %d its entries add up to", le32(m, 2), total)
	}
	return Attention{}, false
}

func le32(b []byte, off int) uint32 {
	if len(b) < off+4 {
		return 0
	}
	return binary.LittleEndian.Uint32(b[off:])
}

func le16(b []byte, off int) uint16 {
	if len(b) < off+2 {
		return 0
	}
	return binary.LittleEndian.Uint16(b[off:])
}

// formatBytes renders a size for a message, in binary units with one decimal.
func formatBytes(n int64) string {
	if n < 1024 {
		return fmt.Sprintf("%d B", n)
	}
	units := [...]string{"KiB", "MiB", "GiB", "TiB"}
	m := min((bits.Len64(uint64(n))-1)/10, len(units))
	return fmt.Sprintf("%.1f %s", float64(n)/float64(uint64(1)<<(m*10)), units[m-1])
}
