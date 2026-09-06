package installer

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// Replay places only recorded destinations, matching archive content by its recorded hash.
func Replay(manifest []ManifestEntry, extracted, dest string) error {
	sources := make(map[string]string)
	err := filepath.WalkDir(extracted, func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk replay archive: %w", err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return ErrUnsupportedEntry
		}
		hash, err := sha256File(name)
		if err != nil {
			return err
		}
		sources[hash] = name
		return nil
	})
	if err != nil {
		return fmt.Errorf("index replay content: %w", err)
	}
	changes := make([]Change, 0, len(manifest))
	for _, entry := range manifest {
		if err := checkDest(entry.Path); err != nil {
			return err
		}
		src, ok := sources[entry.SHA256]
		if !ok {
			return fmt.Errorf("no archived content matches %s", entry.Path)
		}
		changes = append(changes, Change{Placement: Placement{Source: src, Dest: entry.Path}, Action: ActionCreate})
	}
	return Apply(changes, dest)
}

// CopyTree copies regular files from a confined source tree; links and devices are refused.
func CopyTree(src *os.Root, dest string) error {
	err := fs.WalkDir(src.FS(), ".", func(name string, d fs.DirEntry, err error) error {
		if err != nil {
			return fmt.Errorf("walk saved files: %w", err)
		}
		if d.IsDir() {
			return nil
		}
		if !d.Type().IsRegular() {
			return ErrUnsupportedEntry
		}
		f, err := src.Open(name)
		if err != nil {
			return fmt.Errorf("open saved file: %w", err)
		}
		defer func() { _ = f.Close() }()
		return publishFile(f, filepath.Join(dest, filepath.FromSlash(name)))
	})
	if err != nil {
		return fmt.Errorf("copy saved tree: %w", err)
	}
	return nil
}
