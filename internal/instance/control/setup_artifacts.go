package control

import (
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/valminhq/valmin/internal/instance"
	"github.com/valminhq/valmin/internal/mods/cache"
	"github.com/valminhq/valmin/internal/mods/extract"
	"github.com/valminhq/valmin/internal/mods/fsutil"
	"github.com/valminhq/valmin/internal/mods/installer"
	"github.com/valminhq/valmin/internal/mods/source"
	"github.com/valminhq/valmin/internal/setupblob"
	"github.com/valminhq/valmin/internal/store"
)

// setupArtifacts captures and stages the package bytes referenced by saved setups.
type setupArtifacts struct{ DataRoot string }

//nolint:gocritic // Callers use immutable snapshot values shared with restore validation.
func setupManifest(mod SetupMod) ([]installer.ManifestEntry, error) {
	var entries []installer.ManifestEntry
	if err := installer.CheckFullName(mod.FullName); err != nil {
		return nil, fmt.Errorf("validate mod name %s: %w", mod.FullName, err)
	}
	if err := json.Unmarshal([]byte(mod.FileManifest), &entries); err != nil {
		return nil, fmt.Errorf("decode %s file manifest: %w", mod.FullName, err)
	}
	for _, e := range entries {
		if e.Path == "" || !filepath.IsLocal(filepath.FromSlash(e.Path)) || strings.Contains(e.Path, "\\") {
			return nil, fmt.Errorf("unsafe path %q in %s", e.Path, mod.FullName)
		}
	}
	return entries, nil
}

func openManagedSetupFile(inst *store.Instance, fullName string, e installer.ManifestEntry) (*os.File, error) {
	if err := installer.CheckFullName(fullName); err != nil {
		return nil, fmt.Errorf("validate mod name %s: %w", fullName, err)
	}
	if e.Path == "" || !filepath.IsLocal(filepath.FromSlash(e.Path)) || strings.Contains(e.Path, "\\") {
		return nil, fmt.Errorf("unsafe managed path %q", e.Path)
	}
	base := instance.ServerDir(inst.DataDir)
	if e.Parked {
		base = filepath.Join(instance.ParkedModsDir(inst.DataDir), fullName)
	}
	root, err := os.OpenRoot(base)
	if err != nil {
		return nil, fmt.Errorf("open managed package root %s: %w", base, err)
	}
	defer func() { _ = root.Close() }()
	f, _, err := fsutil.OpenRegularIn(root, filepath.FromSlash(e.Path))
	if err != nil {
		return nil, fmt.Errorf("open managed file: %w", err)
	}
	return f, nil
}

func hashManagedSetupFile(
	inst *store.Instance, fullName string, e installer.ManifestEntry,
) (digest string, size int64, err error) {
	f, err := openManagedSetupFile(inst, fullName, e)
	if err != nil {
		return "", 0, err
	}
	defer func() { _ = f.Close() }()
	if extract.MaxEntryUncompressedBytes >= math.MaxInt64 {
		return "", 0, extract.ErrLimit
	}
	limit := int64(extract.MaxEntryUncompressedBytes) + 1
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit))
	if err != nil {
		return "", 0, fmt.Errorf("hash managed file %s: %w", e.Path, err)
	}
	if n >= limit {
		return "", 0, extract.ErrLimit
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}

func setupPayloadEntries(entries []installer.ManifestEntry) []installer.ManifestEntry {
	out := make([]installer.ManifestEntry, 0, len(entries))
	for _, e := range entries {
		if !installer.UserConfig(e.Path) {
			out = append(out, e)
		}
	}
	return out
}

func (a *setupArtifacts) Save(
	ctx context.Context, inst *store.Instance, snap *SetupSnapshot, staging string,
) ([]store.SetupArtifactRef, error) {
	blobs := setupblob.New(a.DataRoot)
	refs := make([]store.SetupArtifactRef, 0, len(snap.Mods))
	for i := range snap.Mods {
		if err := ctx.Err(); err != nil {
			return nil, fmt.Errorf("save setup interrupted: %w", err)
		}
		ref, err := a.retainSetupModArtifact(ctx, blobs, inst, &snap.Mods[i], staging, i)
		if err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	return refs, nil
}

func (a *setupArtifacts) retainSetupModArtifact(
	ctx context.Context, blobs *setupblob.Store, inst *store.Instance, mod *SetupMod, staging string, index int,
) (store.SetupArtifactRef, error) {
	entries, err := setupManifest(*mod)
	if err != nil {
		return store.SetupArtifactRef{}, err
	}
	payload := setupPayloadEntries(entries)
	if len(payload) > extract.MaxEntries {
		return store.SetupArtifactRef{}, extract.ErrLimit
	}
	cacheMatches, err := captureSetupModEntries(inst, mod, entries)
	if err != nil {
		return store.SetupArtifactRef{}, err
	}
	raw, err := json.Marshal(entries)
	if err != nil {
		return store.SetupArtifactRef{}, fmt.Errorf("encode %s file manifest: %w", mod.FullName, err)
	}
	mod.FileManifest = string(raw)
	archive := ""
	kind := "files"
	if cacheMatches {
		archive = a.reproducibleSetupCache(mod, payload, staging)
		if archive != "" {
			kind = "zip"
		}
	}
	if archive == "" {
		archive, err = makeSetupFilesArchive(ctx, inst, mod.FullName, payload, staging, index)
		if err != nil {
			return store.SetupArtifactRef{}, err
		}
	}
	digest, err := blobs.PutFileContext(ctx, archive)
	if err != nil {
		return store.SetupArtifactRef{}, fmt.Errorf("retain package %s: %w", mod.FullName, err)
	}
	if _, err := blobs.Verify(digest); err != nil {
		return store.SetupArtifactRef{}, fmt.Errorf("verify package %s: %w", mod.FullName, err)
	}
	return store.SetupArtifactRef{
		FullName: mod.FullName, Source: mod.Source, Version: mod.Version,
		Kind: kind, SHA256: digest,
	}, nil
}

func captureSetupModEntries(inst *store.Instance, mod *SetupMod, entries []installer.ManifestEntry) (bool, error) {
	var total uint64
	cacheMatches := true
	for i := range entries {
		if installer.UserConfig(entries[i].Path) {
			continue
		}
		hash, size, err := hashManagedSetupFile(inst, mod.FullName, entries[i])
		if err != nil {
			return false, fmt.Errorf("capture %s/%s: %w", mod.FullName, entries[i].Path, err)
		}
		// io.Copy returns a nonnegative count.
		total += uint64(size) //nolint:gosec // io.Copy returns a nonnegative byte count.
		if total > extract.MaxTotalUncompressedBytes {
			return false, extract.ErrLimit
		}
		cacheMatches = cacheMatches && hash == entries[i].SHA256
		entries[i].SHA256 = hash
	}
	return cacheMatches, nil
}

func (a *setupArtifacts) reproducibleSetupCache(
	mod *SetupMod,
	entries []installer.ManifestEntry,
	staging string,
) string {
	src, ok := source.ByName(mod.Source)
	if !ok || !filepath.IsLocal(mod.Version) || strings.ContainsAny(mod.Version, "/\\") {
		return ""
	}
	cacheRoot := cache.RootFor(a.DataRoot, src)
	root, err := os.OpenRoot(cacheRoot)
	if err != nil {
		return ""
	}
	archiveName := mod.FullName + "-" + mod.Version + ".zip"
	_, err = root.Stat(archiveName)
	_ = root.Close()
	if err != nil {
		return ""
	}
	archive := filepath.Join(cacheRoot, archiveName)
	extracted, err := setupStagingDir(staging, "candidate", mod.FullName)
	if err != nil {
		return ""
	}
	target := filepath.Join(staging, "candidate-target", mod.FullName)
	if err := extract.Extract(archive, extracted); err != nil {
		return ""
	}
	if err := installer.Replay(entries, extracted, target); err != nil {
		return ""
	}
	targetRoot, err := os.OpenRoot(target)
	if err != nil {
		return ""
	}
	defer func() { _ = targetRoot.Close() }()
	for _, e := range entries {
		f, err := targetRoot.OpenFile(filepath.FromSlash(e.Path), os.O_RDONLY, 0)
		if err != nil {
			return ""
		}
		sum := sha256.New()
		_, err = io.Copy(sum, f)
		_ = f.Close()
		if err != nil || hex.EncodeToString(sum.Sum(nil)) != e.SHA256 {
			return ""
		}
	}
	return archive
}

func makeSetupFilesArchive(
	ctx context.Context, inst *store.Instance, fullName string,
	entries []installer.ManifestEntry, staging string, index int,
) (string, error) {
	root, err := os.OpenRoot(staging)
	if err != nil {
		return "", fmt.Errorf("open setup staging directory: %w", err)
	}
	defer func() { _ = root.Close() }()
	name := fmt.Sprintf("files-%d.zip", index)
	f, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", fmt.Errorf("create setup package archive: %w", err)
	}
	defer func() { _ = f.Close() }()
	zw := zip.NewWriter(f)
	for i, e := range entries {
		if err := ctx.Err(); err != nil {
			return "", fmt.Errorf("archive setup interrupted: %w", err)
		}
		src, err := openManagedSetupFile(inst, fullName, e)
		if err != nil {
			return "", err
		}
		dst, err := zw.Create(fmt.Sprintf("files/%06d", i))
		if err == nil {
			_, err = io.Copy(dst, src)
		}
		_ = src.Close()
		if err != nil {
			return "", fmt.Errorf("archive %s/%s: %w", fullName, e.Path, err)
		}
	}
	if err := zw.Close(); err != nil {
		return "", fmt.Errorf("close setup package archive: %w", err)
	}
	if err := f.Sync(); err != nil {
		return "", fmt.Errorf("sync setup package archive: %w", err)
	}
	if err := f.Close(); err != nil {
		return "", fmt.Errorf("close setup package archive: %w", err)
	}
	return filepath.Join(staging, name), nil
}

func ValidateSetupRefs(mods []SetupMod, refs []store.SetupArtifactRef) error {
	if len(mods) != len(refs) {
		return errors.New("a package payload is missing")
	}
	byName := make(map[string]store.SetupArtifactRef, len(refs))
	for _, ref := range refs {
		if _, exists := byName[ref.FullName]; exists {
			return fmt.Errorf("duplicate payload for %s", ref.FullName)
		}
		byName[ref.FullName] = ref
	}
	for _, mod := range mods {
		ref, ok := byName[mod.FullName]
		if !ok || ref.Source != mod.Source || ref.Version != mod.Version ||
			(ref.Kind != "zip" && ref.Kind != "files") {
			return fmt.Errorf("payload metadata mismatch for %s", mod.FullName)
		}
	}
	return nil
}

func (a *setupArtifacts) stage(
	ctx context.Context, snap *SetupSnapshot, refs []store.SetupArtifactRef, staging string,
) error {
	if err := ValidateSetupRefs(snap.Mods, refs); err != nil {
		return err
	}
	byName := make(map[string]store.SetupArtifactRef, len(refs))
	for _, ref := range refs {
		byName[ref.FullName] = ref
	}
	owned := map[string]string{}
	for i := range snap.Mods {
		if err := ctx.Err(); err != nil {
			return fmt.Errorf("stage setup interrupted: %w", err)
		}
		mod := &snap.Mods[i]
		ref := byName[mod.FullName]
		if err := a.stageSetupMod(mod, &ref, staging, owned); err != nil {
			return err
		}
	}
	return nil
}

func (a *setupArtifacts) stageSetupMod(
	mod *SetupMod, ref *store.SetupArtifactRef, staging string, owned map[string]string,
) error {
	archive, err := setupblob.New(a.DataRoot).Verify(ref.SHA256)
	if err != nil {
		return fmt.Errorf("verify %s payload: %w", mod.FullName, err)
	}
	entries, err := setupManifest(*mod)
	if err != nil {
		return err
	}
	extracted, err := setupStagingDir(staging, "extracted", mod.FullName)
	if err != nil {
		return fmt.Errorf("create extraction directory for %s: %w", mod.FullName, err)
	}
	if err := extract.Extract(archive, extracted); err != nil {
		return fmt.Errorf("extract %s: %w", mod.FullName, err)
	}
	serverEntries, parkedEntries, err := partitionSetupEntries(mod, entries, owned)
	if err != nil {
		return err
	}
	for _, part := range []struct {
		entries []installer.ManifestEntry
		dest    string
	}{
		{serverEntries, filepath.Join(staging, "target", "server")},
		{parkedEntries, filepath.Join(staging, "target", "park", mod.FullName)},
	} {
		if err := installer.Replay(part.entries, extracted, part.dest); err != nil {
			return fmt.Errorf("replay %s: %w", mod.FullName, err)
		}
	}
	return nil
}

func partitionSetupEntries(
	mod *SetupMod, entries []installer.ManifestEntry, owned map[string]string,
) (serverEntries, parkedEntries []installer.ManifestEntry, err error) {
	for _, e := range setupPayloadEntries(entries) {
		key := "server/" + e.Path
		if e.Parked {
			key = "park/" + mod.FullName + "/" + e.Path
		}
		if prev, exists := owned[key]; exists {
			return nil, nil, fmt.Errorf("%s and %s both own %s", prev, mod.FullName, e.Path)
		}
		owned[key] = mod.FullName
		if e.Parked {
			parkedEntries = append(parkedEntries, e)
		} else {
			serverEntries = append(serverEntries, e)
		}
	}
	return serverEntries, parkedEntries, nil
}

func setupStagingDir(staging, parent, fullName string) (string, error) {
	if err := installer.CheckFullName(fullName); err != nil {
		return "", fmt.Errorf("validate mod name %s: %w", fullName, err)
	}
	root, err := os.OpenRoot(staging)
	if err != nil {
		return "", fmt.Errorf("open setup staging root: %w", err)
	}
	defer func() { _ = root.Close() }()
	rel := filepath.Join(parent, fullName)
	if err := root.MkdirAll(rel, 0o750); err != nil {
		return "", fmt.Errorf("create setup staging directory: %w", err)
	}
	return filepath.Join(staging, rel), nil
}

func setupModsChanged(current, target []SetupMod) bool {
	if len(current) != len(target) {
		return true
	}
	a := append([]SetupMod(nil), current...)
	b := append([]SetupMod(nil), target...)
	slices.SortFunc(a, func(x, y SetupMod) int { return strings.Compare(x.FullName, y.FullName) })
	slices.SortFunc(b, func(x, y SetupMod) int { return strings.Compare(x.FullName, y.FullName) })
	for i := range a {
		if a[i].FullName != b[i].FullName || a[i].Source != b[i].Source ||
			a[i].Version != b[i].Version || a[i].Enabled != b[i].Enabled ||
			a[i].FileManifest != b[i].FileManifest {
			return true
		}
	}
	return false
}
