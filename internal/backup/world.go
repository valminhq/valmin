package backup

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
)

// WorldPart names which half of a world a file is. A world needs both, in either layout, and
// half of one is not restorable (03 §4).
type WorldPart int

const (
	// PartOther is a file that is not either half: a chunk, a marker, a player list, the
	// engine's `.old` fallbacks, a biome cache.
	PartOther WorldPart = iota
	// PartData is the world itself — `<name>.db` before 1.0, `_main.<gen>.db2` since.
	PartData
	// PartHeader is the world's header — `<name>.fwl` before 1.0, `_main.<gen>.fwl2` since.
	PartHeader
)

// mainFile matches 1.0's per-save files inside a world directory. The generation counter
// moves with every save (measured at 14 in a live world and 10 in an older rolling save), so
// it is read rather than pinned; the extension is what says which half this is.
var mainFile = regexp.MustCompile(`^_main\.\d+\.(db2|fwl2)$`)

// ClassifyWorldFile reports which world a path under a savedir belongs to, the directory that
// world sits in, and which half of it this file is.
//
// It reads **both layouts**, because a host can hold both: every build before 1.0 wrote a
// world as a `<name>.db` + `<name>.fwl` pair, and build 25253791 writes it as a directory
// named exactly what `-world` names, holding `_main.<gen>.db2`, `_main.<gen>.fwl2`, a chunk
// index, an `.ok` marker and one file per chunk (03 §4,
// evidence/world-format-1.0-2026-09-14.md). Whether 1.0 converts a pre-1.0 world it opens is
// unmeasured (Q56), so neither reader can be retired on a guess.
//
// rel is slash-separated and relative to the savedir, so one function serves a walk of the
// filesystem and a walk of a tar. The game's own rolling saves are named after the world and
// sit beside it in both layouts; they classify as their own world, whose name is not the
// instance's, which is what keeps them from satisfying the pair check on its behalf
// (03 §4.1 rule 5).
func ClassifyWorldFile(rel string) (dir, world string, part WorldPart) {
	rel = path.Clean(strings.ReplaceAll(rel, `\`, "/"))
	base := path.Base(rel)
	parent := path.Dir(rel)

	if m := mainFile.FindStringSubmatch(base); m != nil {
		// The world is the directory. Without one there is no world this file could name.
		if parent == "." || parent == "/" {
			return "", "", PartOther
		}
		return cleanDir(path.Dir(parent)), path.Base(parent), partOfExt(m[1])
	}

	switch {
	case strings.HasSuffix(base, ".db"):
		return cleanDir(parent), strings.TrimSuffix(base, ".db"), PartData
	case strings.HasSuffix(base, ".fwl"):
		return cleanDir(parent), strings.TrimSuffix(base, ".fwl"), PartHeader
	}
	return "", "", PartOther
}

func partOfExt(ext string) WorldPart {
	if ext == "db2" {
		return PartData
	}
	return PartHeader
}

// cleanDir renders path.Dir's "." for the savedir root as "".
func cleanDir(dir string) string {
	if dir == "." || dir == "/" {
		return ""
	}
	return dir
}

// WorldFiles is what has been seen of one world.
type WorldFiles struct {
	// ModifiedAt is the newest mtime seen among the world's own files, set by a caller that
	// walks a filesystem. Zero for a scan of an archive, which is asked a different question.
	ModifiedAt time.Time
	// Bytes is everything the world occupies, which for 1.0 is a directory of chunk files
	// that hold most of the world and for a pair is the two files. Zero unless a caller
	// measured it.
	Bytes int64
	// Files are the world's own files relative to the scanned root, sorted. Empty for a scan
	// of an archive, which is asked a different question.
	Files []string
	// Dir is where the world sits relative to the savedir, "" for the savedir itself.
	Dir string
	// DataBytes and HeaderBytes are -1 for a half that has not been seen.
	DataBytes, HeaderBytes int64
	// Directory is 1.0's layout: the world is a directory rather than a pair of files. It
	// decides which size floors apply, since the pre-1.0 floors were measured against files
	// that held the whole world and a `.db2` sits beside chunk files that hold most of it.
	Directory bool
}

// Complete reports whether both halves have been seen.
func (w *WorldFiles) Complete() bool { return w.DataBytes >= 0 && w.HeaderBytes >= 0 }

// WorldScan accumulates the worlds found while walking a savedir or an archive of one.
type WorldScan map[string]*WorldFiles

// Add folds one path into the scan, ignoring anything that is not half of a world. It reports
// which world the file belonged to, so a caller with more to record about it can.
func (s WorldScan) Add(rel string, size int64) (world string, ok bool) {
	dir, name, part := ClassifyWorldFile(rel)
	if part == PartOther {
		return "", false
	}
	found := s[name]
	if found == nil {
		found = &WorldFiles{Dir: dir, DataBytes: -1, HeaderBytes: -1}
		s[name] = found
	}
	// A `_main.<gen>.*` file is only ever inside a world directory, so seeing one is what
	// settles the layout; a pair never sets it.
	if strings.HasPrefix(path.Base(rel), "_main.") {
		found.Directory = true
	}
	if part == PartData {
		found.DataBytes = size
	} else {
		found.HeaderBytes = size
	}
	return name, true
}

// Complete reports whether the named world was seen whole.
func (s WorldScan) Complete(name string) bool {
	world, ok := s[name]
	return ok && world.Complete()
}

// ScanWorlds walks a savedir and reports every world under it, in either layout, with the
// bytes each occupies and when it was last written.
//
// A savedir that does not exist is no worlds and no error: an instance that has never run has
// never written one.
func ScanWorlds(root string) (WorldScan, error) {
	files, err := regularFiles(root)
	if err != nil {
		return nil, fmt.Errorf("scan the worlds under %s: %w", root, err)
	}

	scan := WorldScan{}
	for _, f := range files {
		if name, ok := scan.Add(f.rel, f.size); ok {
			if f.mod.After(scan[name].ModifiedAt) {
				scan[name].ModifiedAt = f.mod
			}
		}
	}
	// A second pass for the size, because a 1.0 world is a directory whose chunk files hold
	// most of it and are not either half. Attributing them needs the world's directory, which
	// only the first pass establishes.
	for _, f := range files {
		for name, world := range scan {
			if world.owns(f.rel, name) {
				world.Bytes += f.size
				world.Files = append(world.Files, f.rel)
			}
		}
	}
	for _, world := range scan {
		slices.Sort(world.Files)
	}
	return scan, nil
}

// scannedFile is one regular file under a scanned root.
type scannedFile struct {
	rel  string
	size int64
	mod  time.Time
}

// regularFiles lists every regular file under root, with slash-separated relative paths so the
// classifier reads them the same way it reads a tar entry.
//
// A root that does not exist is no files and no error: an instance that has never run has
// never written a world, and a staging directory nothing landed in is an upload that carried
// no world — both of which the caller reports in its own words.
func regularFiles(root string) ([]scannedFile, error) {
	var files []scannedFile
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, p)
		if err != nil {
			return fmt.Errorf("locate %s: %w", p, err)
		}
		files = append(files, scannedFile{filepath.ToSlash(rel), fi.Size(), fi.ModTime()})
		return nil
	})
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("walk %s: %w", root, err)
	}
	return files, nil
}

// owns reports whether rel is part of this world. For 1.0 that is everything under the world's
// own directory — chunks, the index and the `.ok` marker included. For a pair it is the two
// files and nothing else: the engine's `.old` fallbacks are a previous save, not this one.
func (w *WorldFiles) owns(rel, name string) bool {
	if w.Directory {
		return strings.HasPrefix(rel, path.Join(w.Dir, name)+"/")
	}
	base := path.Base(rel)
	return path.Dir(rel) == cleanDirOf(w.Dir) && (base == name+".db" || base == name+".fwl")
}

// cleanDirOf renders WorldFiles.Dir's "" for the savedir root back as path.Dir's ".".
func cleanDirOf(dir string) string {
	if dir == "" {
		return "."
	}
	return dir
}
