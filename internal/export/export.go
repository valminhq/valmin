// Package export owns the installation export: one gzipped tar holding what a new host cannot
// download again, laid out as the data root so a restore unpacks it in place. A light export
// holds the database, the master key, the rclone config, and each server's worlds, configs,
// unmanaged mod files and parked mods; a full one adds the game builds and mod archives in use,
// the local world backups and the saved-setup blobs.
package export

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/valminhq/valmin/internal/backup"
)

// Mode is how much an export carries.
type Mode string

// The two modes.
const (
	Light Mode = "light"
	Full  Mode = "full"
)

// ParseMode reads a mode from a request.
func ParseMode(s string) (Mode, error) {
	switch m := Mode(s); m {
	case Light, Full:
		return m, nil
	}
	return "", fmt.Errorf("export mode must be %q or %q", Light, Full)
}

// ManifestName is the archive's first entry, which describes the rest.
const ManifestName = "valmin-export.json"

// formatVersion is the archive layout this build writes and reads.
const formatVersion = 1

// Manifest describes one export.
type Manifest struct {
	Format int  `json:"format"`
	Mode   Mode `json:"mode"`
	// Schema is the database's migration count; a build that knows fewer cannot restore it.
	Schema    int       `json:"schema_version"`
	Panel     string    `json:"panel_version"`
	CreatedAt time.Time `json:"created_at"`
	// Live lists the servers whose files were copied while they ran, so may not be consistent.
	Live []string `json:"live,omitempty"`
	// Builds is the game build each server's installation held, by server ID.
	Builds map[string]string `json:"builds,omitempty"`
}

// Plan is everything one export writes. Paths in Extra are relative to Root.
type Plan struct {
	Manifest Manifest
	Root     string
	// Database is a consistent copy of panel.db, archived as DatabaseName.
	Database string
	// KeyFile is the master key file, archived as KeyName; "" when the key comes from the
	// environment.
	KeyFile string
	// RcloneConfig is the rclone configuration file, archived as RcloneConfigName when present.
	RcloneConfig string
	Instances    []Instance
	// Extra is archived whole: the downloadable data a full export carries.
	Extra []string
}

// Instance is one server's part of an export.
type Instance struct {
	ID string
	// Live is set when the server is running, so its files may change during the copy.
	Live bool
	// Managed holds the server-relative paths installed packages place, which a restore
	// downloads again rather than unpacks.
	Managed map[string]bool
}

// Fixed archive paths, each relative to the data root, where a restore unpacks them.
const (
	DatabaseName     = "panel.db"
	KeyName          = "secret.key"
	RcloneConfigName = "rclone/rclone.conf"
)

// Write writes p as a gzipped tar to w.
func Write(w io.Writer, p *Plan) error {
	gz := gzip.NewWriter(w)
	tw := tar.NewWriter(gz)
	if err := writePlan(tw, p); err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return fmt.Errorf("finish tar: %w", err)
	}
	if err := gz.Close(); err != nil {
		return fmt.Errorf("finish gzip: %w", err)
	}
	return nil
}

func writePlan(tw *tar.Writer, p *Plan) error {
	raw, err := json.Marshal(p.Manifest)
	if err != nil {
		return fmt.Errorf("encode manifest: %w", err)
	}
	if err := writeBytes(tw, ManifestName, raw); err != nil {
		return err
	}
	if err := writeFile(tw, p.Database, DatabaseName); err != nil {
		return err
	}
	if p.KeyFile != "" {
		if err := writeOperatorFile(tw, p.KeyFile, KeyName); err != nil {
			return err
		}
	}
	if p.RcloneConfig != "" {
		err := writeOperatorFile(tw, p.RcloneConfig, RcloneConfigName)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	for i := range p.Instances {
		if err := writeInstance(tw, p.Root, &p.Instances[i]); err != nil {
			return err
		}
	}
	for _, rel := range p.Extra {
		if err := writeExtra(tw, p.Root, rel); err != nil {
			return err
		}
	}
	return nil
}

// writeInstance archives what a new host cannot rebuild for one server: its worlds, the config
// tree and unmanaged files of its game installation, and its parked mods.
func writeInstance(tw *tar.Writer, root string, inst *Instance) error {
	dir := path.Join("instances", inst.ID)
	// The game's rolling saves stay in either mode: for a server copied while running they are
	// the fallback to a world caught mid-save.
	if err := writeDirIfPresent(tw, root, dir+"/worlds", backup.NotDerived, inst.Live); err != nil {
		return err
	}
	if err := writeDirIfPresent(tw, root, dir+"/server", keptServerFile(inst.Managed), inst.Live); err != nil {
		return err
	}
	return writeDirIfPresent(tw, root, dir+"/mods-disabled", keepAll, false)
}

// keptServerFile selects, under server/, the files no download rebuilds: the `.cfg` files at
// its root and everything under BepInEx/ that no installed package places, the config tree
// included. These are what a game update carries across, plus what mods wrote at runtime.
func keptServerFile(managed map[string]bool) func(rel string) bool {
	return func(rel string) bool {
		top, rest, nested := strings.Cut(rel, "/")
		switch {
		case !nested:
			return top == "BepInEx" || strings.Contains(top, ".cfg")
		case top != "BepInEx":
			return false
		case strings.HasPrefix(rest, "config/") || rest == "config":
			return true
		}
		return !managed[rel]
	}
}

// writeExtra archives one root-relative file or directory, skipping unfinished `.part` files.
func writeExtra(tw *tar.Writer, root, rel string) error {
	src := filepath.Join(root, filepath.FromSlash(rel))
	fi, err := os.Lstat(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect %s: %w", rel, err)
	}
	if fi.IsDir() {
		_, err := backup.WriteTree(tw, src, rel, finished, true)
		return err //nolint:wrapcheck // WriteTree names the tree
	}
	if !fi.Mode().IsRegular() || strings.HasSuffix(rel, backup.PartSuffix) {
		return nil
	}
	return writeFile(tw, src, rel)
}

// writeDirIfPresent archives root/rel through keep, and nothing when it does not exist.
func writeDirIfPresent(tw *tar.Writer, root, rel string, keep func(string) bool, live bool) error {
	src := filepath.Join(root, filepath.FromSlash(rel))
	if _, err := os.Lstat(src); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	_, err := backup.WriteTree(tw, src, rel, keep, live)
	return err //nolint:wrapcheck // WriteTree names the tree
}

func keepAll(string) bool { return true }

func finished(rel string) bool { return !strings.HasSuffix(rel, backup.PartSuffix) }

// writeFile archives one regular file as name, mode 0600.
func writeFile(tw *tar.Writer, src, name string) error {
	f, err := os.OpenFile(src, os.O_RDONLY|syscall.O_NOFOLLOW, 0) //nolint:gosec // panel-built path
	if err != nil {
		return fmt.Errorf("open %s: %w", name, err)
	}
	defer func() { _ = f.Close() }()
	fi, err := f.Stat()
	if err != nil {
		return fmt.Errorf("stat %s: %w", name, err)
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", name)
	}
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: fi.Size(), ModTime: fi.ModTime(), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header for %s: %w", name, err)
	}
	if _, err := io.CopyN(tw, f, fi.Size()); err != nil {
		return fmt.Errorf("archive %s: %w", name, err)
	}
	return nil
}

// writeOperatorFile archives the small file at an operator-configured path as name, following
// a symlink as the panel does when it reads the file.
func writeOperatorFile(tw *tar.Writer, src, name string) error {
	data, err := os.ReadFile(src) //nolint:gosec // an operator setting
	if err != nil {
		return fmt.Errorf("read %s: %w", name, err)
	}
	return writeBytes(tw, name, data)
}

// writeBytes archives data as name, mode 0600.
func writeBytes(tw *tar.Writer, name string, data []byte) error {
	hdr := &tar.Header{Name: name, Mode: 0o600, Size: int64(len(data)), ModTime: time.Now(), Typeflag: tar.TypeReg}
	if err := tw.WriteHeader(hdr); err != nil {
		return fmt.Errorf("write header for %s: %w", name, err)
	}
	if _, err := tw.Write(data); err != nil {
		return fmt.Errorf("archive %s: %w", name, err)
	}
	return nil
}
