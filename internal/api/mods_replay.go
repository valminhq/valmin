package api

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

// StageReplay materializes the installed manifests without recalculating placement.
func (m *Mods) StageReplay(ctx context.Context, inst *store.Instance, dest string) error {
	rows, err := m.DB.InstanceMods(ctx, inst.ID)
	if err != nil {
		return fmt.Errorf("read installed mods: %w", err)
	}
	for _, row := range rows {
		var manifest []installer.ManifestEntry
		if err := json.Unmarshal([]byte(row.FileManifest), &manifest); err != nil {
			return fmt.Errorf("decode replay manifest: %w", err)
		}
		url, size, _, err := m.DB.ModVersionDownload(ctx, row.FullName, row.Version)
		if err != nil {
			return fmt.Errorf("resolve replay archive: %w", err)
		}
		zip, err := m.Cache.Get(ctx, row.FullName+"-"+row.Version, url, size)
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
