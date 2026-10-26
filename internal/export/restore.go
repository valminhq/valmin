package export

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"

	"github.com/valminhq/valmin/internal/store"
)

// maxManifestBytes bounds the manifest read before anything else is trusted.
const maxManifestBytes = 1 << 16

// dirMode is given to a directory the archive names only as a parent: an instance directory,
// which the panel creates group-writable, among them.
const dirMode = 0o775

// lostFound is the one entry an empty data root on its own filesystem already has.
const lostFound = "lost+found"

// IncompleteMark is written to the data root before a restore unpacks anything, and removed by
// FinishRestore once the restored installation is ready. While it exists the panel refuses to
// start, and a new restore clears the root and begins again.
const IncompleteMark = ".restore-incomplete"

// Restore unpacks the export read from r into root and returns its manifest. root must be
// empty, or hold a restore that never finished, which is cleared first. It refuses an archive
// from a schema newer than schema. Every entry is written through an os.Root, and only
// directories and regular files are accepted, so no entry can land outside root. On failure it
// removes everything it wrote. The caller calls FinishRestore once it has prepared the rest.
func Restore(r io.Reader, root string, schema int) (*Manifest, error) {
	dir, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open data root: %w", err)
	}
	defer func() { _ = dir.Close() }()
	if err := requireEmpty(dir); err != nil {
		return nil, err
	}
	if err := clearRoot(dir); err != nil {
		return nil, err
	}
	if err := dir.WriteFile(IncompleteMark, nil, 0o600); err != nil {
		return nil, fmt.Errorf("mark the restore incomplete: %w", err)
	}

	m, err := unpack(r, dir, schema)
	if err != nil {
		if cleanErr := clearRoot(dir); cleanErr != nil {
			return nil, errors.Join(err, cleanErr)
		}
		return nil, err
	}
	return m, nil
}

// FinishRestore flushes the restored tree to disk and removes IncompleteMark.
func FinishRestore(root string) error {
	// One flush for the whole tree rather than one per file: a restore cut short is redone.
	syscall.Sync()
	if err := os.Remove(filepath.Join(root, IncompleteMark)); err != nil {
		return fmt.Errorf("finish the restore: %w", err)
	}
	return nil
}

// RestoreIncomplete reports whether root holds a restore that never finished.
func RestoreIncomplete(root string) bool {
	_, err := os.Lstat(filepath.Join(root, IncompleteMark))
	return err == nil
}

func unpack(r io.Reader, dir *os.Root, schema int) (*Manifest, error) {
	gz, err := gzip.NewReader(r)
	if err != nil {
		return nil, fmt.Errorf("read export: %w", err)
	}
	tr := tar.NewReader(gz)
	m, err := readManifest(tr, schema)
	if err != nil {
		return nil, err
	}
	for {
		hdr, err := tr.Next()
		if errors.Is(err, io.EOF) {
			return m, nil
		}
		if err != nil {
			return nil, fmt.Errorf("read export: %w", err)
		}
		if err := writeEntry(dir, tr, hdr); err != nil {
			return nil, err
		}
	}
}

// readManifest reads and checks the first entry.
func readManifest(tr *tar.Reader, schema int) (*Manifest, error) {
	hdr, err := tr.Next()
	if err != nil {
		return nil, fmt.Errorf("read export: %w", err)
	}
	if hdr.Name != ManifestName {
		return nil, fmt.Errorf("not a Valmin export: the first entry is %q, not %s", hdr.Name, ManifestName)
	}
	raw, err := io.ReadAll(io.LimitReader(tr, maxManifestBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", ManifestName, err)
	}
	if len(raw) > maxManifestBytes {
		return nil, fmt.Errorf("%s is larger than %d bytes", ManifestName, maxManifestBytes)
	}
	var m Manifest
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("decode %s: %w", ManifestName, err)
	}
	if m.Format != formatVersion {
		return nil, fmt.Errorf("export format %d is not supported; this build reads format %d", m.Format, formatVersion)
	}
	if m.Schema > schema {
		return nil, fmt.Errorf("the export was made by panel %s, whose database is newer than this build "+
			"knows (migration %d, this build knows up to %d); restore with that version or later",
			m.Panel, m.Schema, schema)
	}
	return &m, nil
}

// writeEntry creates one directory or regular file. Permission bits come from the archive;
// set-id and sticky bits never do.
func writeEntry(dir *os.Root, tr *tar.Reader, hdr *tar.Header) error {
	name := path.Clean(hdr.Name)
	if !filepath.IsLocal(name) || name == ManifestName {
		return fmt.Errorf("export entry %q is not a path inside the data root", hdr.Name)
	}
	perm := hdr.FileInfo().Mode().Perm()
	switch hdr.Typeflag {
	case tar.TypeDir:
		// Owner-writable whatever the archive says, or its own files could not be created.
		if err := dir.MkdirAll(name, perm|0o700); err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		return nil
	case tar.TypeReg:
		if parent := path.Dir(name); parent != "." {
			if err := dir.MkdirAll(parent, dirMode); err != nil {
				return fmt.Errorf("create %s: %w", parent, err)
			}
		}
		f, err := dir.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, perm)
		if err != nil {
			return fmt.Errorf("create %s: %w", name, err)
		}
		_, err = io.Copy(f, tr)
		if closeErr := f.Close(); err == nil {
			err = closeErr
		}
		if err != nil {
			return fmt.Errorf("write %s: %w", name, err)
		}
		return nil
	}
	return fmt.Errorf("export entry %q is neither a file nor a directory", hdr.Name)
}

// requireEmpty refuses a data root that holds anything but lost+found and the panel's lock
// file, unless it holds a restore that never finished.
func requireEmpty(dir *os.Root) error {
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return fmt.Errorf("list data root: %w", err)
	}
	for _, e := range entries {
		if e.Name() == IncompleteMark {
			return nil
		}
	}
	for _, e := range entries {
		if !keptOnClear(e.Name()) {
			return fmt.Errorf(
				"the data root is not empty (it holds %s); restore only into a new installation",
				e.Name(),
			)
		}
	}
	return nil
}

// keptOnClear names what a data root holds before anything is restored into it.
func keptOnClear(name string) bool { return name == lostFound || name == store.LockFile }

// clearRoot removes everything in a data root but what keptOnClear names. IncompleteMark goes
// last, so a clear cut short still reads as an unfinished restore.
func clearRoot(dir *os.Root) error {
	entries, err := fs.ReadDir(dir.FS(), ".")
	if err != nil {
		return fmt.Errorf("list data root: %w", err)
	}
	for _, e := range entries {
		if keptOnClear(e.Name()) || e.Name() == IncompleteMark {
			continue
		}
		if err := dir.RemoveAll(e.Name()); err != nil {
			return fmt.Errorf("remove %s from the data root: %w", e.Name(), err)
		}
	}
	if err := dir.Remove(IncompleteMark); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("remove %s: %w", IncompleteMark, err)
	}
	return nil
}
