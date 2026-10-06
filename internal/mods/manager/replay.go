package manager

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/store"
)

// StageReplay materializes every installed package's manifested files into dest from the
// cached archives, placed by recorded hash rather than by recalculating placement.
func (i *Installer) StageReplay(ctx context.Context, inst *store.Instance, dest string) error {
	rows, err := i.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("read installed mods: %w", err)
	}
	for j := range rows {
		row := &rows[j]
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(row.FileManifest), &manifest); err != nil {
			return fmt.Errorf("decode replay manifest: %w", err)
		}
		// A disabled package's parked files live beside server/, which the swap leaves alone,
		// so only what is in the server root is replayed.
		manifest, _ = installer.Split(manifest)
		if len(manifest) == 0 {
			continue
		}
		// The registry the files came from, not a preference: a replay reproduces the bytes
		// this instance already holds.
		zips, ok := i.Caches[row.Source]
		if !ok {
			return fmt.Errorf("%s was installed from the %s registry, which is not enabled",
				row.FullName, row.Source)
		}
		url, size, _, err := i.DB.ModVersionDownload(ctx, row.FullName, row.Version, row.Source)
		if err != nil {
			return fmt.Errorf("resolve replay archive: %w", err)
		}
		zip, err := zips.Get(ctx, row.FullName+"-"+row.Version, url, size)
		if err != nil {
			return fmt.Errorf("load replay archive: %w", err)
		}
		dir, err := os.MkdirTemp(filepath.Dir(dest), "replay-")
		if err != nil {
			return fmt.Errorf("create replay staging: %w", err)
		}
		err = extract.Extract(zip, dir)
		if err == nil {
			err = installer.Replay(manifest, dir, dest)
		}
		_ = os.RemoveAll(dir)
		if err != nil {
			return fmt.Errorf("replay %s: %w", row.FullName, err)
		}
	}
	return nil
}
