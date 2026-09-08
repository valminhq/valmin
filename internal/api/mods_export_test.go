package api

import (
	"archive/zip"
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/valminhq/valmin/internal/store"
)

func getExport(t *testing.T, rt *Router, u *store.User, query string) *httptest.ResponseRecorder {
	t.Helper()
	return as(rt, u, httptest.NewRequest(
		http.MethodGet, "/api/v1/instances/inst-a/mods/export"+query, http.NoBody))
}

func exportPreviewOf(t *testing.T, rt *Router, u *store.User) exportPreview {
	t.Helper()
	rec := getExport(t, rt, u, "")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	var preview exportPreview
	decodeInto(t, rec, &preview)
	return preview
}

// modsIn flattens a preview's members to full_name -> version.
func modsIn(entries []exportEntry) map[string]string {
	out := map[string]string{}
	for _, e := range entries {
		out[e.FullName] = e.Version
	}
	return out
}

// r2xIn reads the manifest out of a downloaded archive.
func r2xIn(t *testing.T, archive []byte) string {
	t.Helper()
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	if len(zr.File) != 1 || zr.File[0].Name != "export.r2x" {
		t.Fatalf("archive holds %d entries, want only export.r2x", len(zr.File))
	}
	f, err := zr.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	body, err := io.ReadAll(f)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}

// exportWorld installs the three-deep closure and tags its root, which is the shape every
// export test starts from: one package an admin says players need, two it dragged in.
func exportWorld(t *testing.T) (*Router, *store.DB, *store.User) {
	t.Helper()
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	if rec := patchMod(t, rt, admin, "OdinPlus-OdinArchitect",
		map[string]any{"side": "client_required"}); rec.Code != http.StatusOK {
		t.Fatalf("tagging status = %d, want 200 (%s)", rec.Code, rec.Body)
	}
	return rt, db, admin
}

// TestExportKeepsRequiredDependencies is 03 §5.6's parity rule: a client told to install the
// tagged mod alone would be missing everything under it, so the closure travels with it, at
// the versions this server actually runs.
func TestExportKeepsRequiredDependencies(t *testing.T) {
	rt, _, admin := exportWorld(t)

	preview := exportPreviewOf(t, rt, admin)
	got := modsIn(preview.Mods)
	want := map[string]string{
		"OdinPlus-OdinArchitect":       "1.7.0",
		"ValheimModding-Jotunn":        "2.29.2",
		"denikson-BepInExPack_Valheim": "5.4.2333",
	}
	for name, version := range want {
		if got[name] != version {
			t.Errorf("export has %s at %q, want %q", name, got[name], version)
		}
	}
	if len(got) != len(want) {
		t.Errorf("export holds %d packages, want %d: %v", len(got), len(want), got)
	}
	if len(preview.Conflicts) != 0 {
		t.Errorf("conflicts = %+v, want none", preview.Conflicts)
	}
	for _, entry := range preview.Mods {
		if entry.FullName == "OdinPlus-OdinArchitect" && entry.Reason != reasonTagged {
			t.Errorf("the tagged mod's reason = %q, want %q", entry.Reason, reasonTagged)
		}
		if entry.FullName == "ValheimModding-Jotunn" && entry.Reason != reasonDependency {
			t.Errorf("a pulled-in mod's reason = %q, want %q", entry.Reason, reasonDependency)
		}
	}
}

// TestExportReportsAServerOnlyDependency is the conflict rule of 04 §3: a client needs
// something the admin says clients must not have. Dropping it silently would produce an
// export that installs and then fails to start, so it is reported and the file is refused.
func TestExportReportsAServerOnlyDependency(t *testing.T) {
	rt, _, admin := exportWorld(t)
	if rec := patchMod(t, rt, admin, "ValheimModding-Jotunn",
		map[string]any{"side": "server_only"}); rec.Code != http.StatusOK {
		t.Fatalf("tagging status = %d, want 200 (%s)", rec.Code, rec.Body)
	}

	preview := exportPreviewOf(t, rt, admin)
	if len(preview.Conflicts) != 1 {
		t.Fatalf("conflicts = %+v, want exactly the server-only dependency", preview.Conflicts)
	}
	conflict := preview.Conflicts[0]
	if conflict.FullName != "ValheimModding-Jotunn" || conflict.RequiredBy != "OdinPlus-OdinArchitect" {
		t.Errorf("conflict = %+v, want Jotunn required by OdinArchitect", conflict)
	}
	if _, in := modsIn(preview.Mods)["ValheimModding-Jotunn"]; in {
		t.Error("a server-only package is in the client export")
	}

	rec := getExport(t, rt, admin, "?format=r2z")
	if rec.Code != http.StatusConflict {
		t.Fatalf("download status = %d, want 409 (%s)", rec.Code, rec.Body)
	}
	if code := errCode(t, rec); code != "mod_conflict" {
		t.Errorf("error code = %q, want mod_conflict", code)
	}
}

// TestExportLeavesUntaggedPackagesOut is 03 §5.6's do-not-classify rule. Installed is not a
// claim that players need it, and the panel never invents the tag an admin has not set.
func TestExportLeavesUntaggedPackagesOut(t *testing.T) {
	rt, db, admin, _, _ := installWorld(t, threeDeep()...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")

	preview := exportPreviewOf(t, rt, admin)
	if len(preview.Mods) != 0 {
		t.Errorf("mods = %+v, want nothing exported before anything is tagged", preview.Mods)
	}
	if len(preview.Excluded) != 3 {
		t.Fatalf("excluded = %+v, want all three installed packages", preview.Excluded)
	}
	for _, entry := range preview.Excluded {
		if entry.Reason != store.SideUnknown {
			t.Errorf("%s excluded for %q, want unknown", entry.FullName, entry.Reason)
		}
	}
}

// TestExportIsDeterministicAndCarriesOnlyIdents covers 04 §3's two content rules at once:
// repeated downloads of unchanged input are byte-identical, and the archive holds package
// idents and versions and nothing else — no game password, no host path.
func TestExportIsDeterministicAndCarriesOnlyIdents(t *testing.T) {
	rt, _, admin := exportWorld(t)

	first := getExport(t, rt, admin, "?format=r2z")
	if first.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (%s)", first.Code, first.Body)
	}
	second := getExport(t, rt, admin, "?format=r2z")
	if !bytes.Equal(first.Body.Bytes(), second.Body.Bytes()) {
		t.Error("two exports of the same input differ")
	}
	if got := first.Header().Get("Content-Disposition"); !strings.Contains(got, "inst-a.r2z") {
		t.Errorf("Content-Disposition = %q, want the instance's own .r2z filename", got)
	}

	manifest := r2xIn(t, first.Body.Bytes())
	// The schema r2modman's importer reads: an ident with no version suffix, then three
	// integers (03 §5.6).
	for _, want := range []string{
		"profileName:",
		"  - name: OdinPlus-OdinArchitect\n",
		"      major: 1\n      minor: 7\n      patch: 0\n",
		"    enabled: true\n",
	} {
		if !strings.Contains(manifest, want) {
			t.Errorf("manifest is missing %q:\n%s", want, manifest)
		}
	}
	if strings.Contains(manifest, "OdinPlus-OdinArchitect-1.7.0") {
		t.Error("name carries a version suffix; the importer parses name and version apart")
	}
	for _, forbidden := range []string{seededWorldPassword, "/instances/inst-a", "worlds"} {
		if strings.Contains(manifest, forbidden) {
			t.Errorf("the export carries %q", forbidden)
		}
	}
}

// TestExportRejectsAnUnknownFormat keeps the parameter closed: a typo that silently served
// the preview would be downloaded and imported as a broken profile.
func TestExportRejectsAnUnknownFormat(t *testing.T) {
	rt, _, admin := exportWorld(t)

	rec := getExport(t, rt, admin, "?format=zip")
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 (%s)", rec.Code, rec.Body)
	}
	if code := errCode(t, rec); code != "invalid_parameter" {
		t.Errorf("error code = %q, want invalid_parameter", code)
	}
}

// TestExportIsInvisibleWithoutTheInstance is ADR-038: no grant means the instance does not
// exist, and its mod list is not a way around that.
func TestExportIsInvisibleWithoutTheInstance(t *testing.T) {
	rt, db, admin, member, _ := installWorld(t, threeDeep()...)
	alreadyModded(t, db)
	installClosure(t, rt, admin, "OdinPlus-OdinArchitect", "1.7.0")
	seed(t, db, `DELETE FROM instance_grants WHERE user_id = 'u-member'`)

	if rec := getExport(t, rt, member, ""); rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 (%s)", rec.Code, rec.Body)
	}
}

// modBlocks splits a manifest into its per-mod blocks, sorted. The two producers list mods in
// different orders — r2modman in install order, this panel alphabetically — and the assertion
// below is about grammar, not order.
func modBlocks(t *testing.T, manifest string) []string {
	t.Helper()
	head, rest, ok := strings.Cut(manifest, "mods:\n")
	if !ok {
		t.Fatalf("no mods: key in manifest:\n%s", manifest)
	}
	if !strings.HasPrefix(head, "profileName: ") {
		t.Errorf("manifest starts %q, want a profileName line", head)
	}
	var blocks []string
	for _, block := range strings.Split(rest, "  - name: ") {
		if block != "" {
			blocks = append(blocks, "  - name: "+block)
		}
	}
	sort.Strings(blocks)
	return blocks
}

// TestExportMatchesARealR2modmanExport is Q5's evidence: the manifest this panel writes is
// compared against export.r2x from a real r2modman export of an eleven-mod Valheim profile,
// block for block. A schema read from the importer's source is the claim; this is the check
// that the bytes agree with a producer in the field.
func TestExportMatchesARealR2modmanExport(t *testing.T) {
	fixture, err := os.ReadFile(filepath.Join("testdata", "r2modman_export.r2x"))
	if err != nil {
		t.Fatal(err)
	}
	want := modBlocks(t, string(fixture))

	preview := &exportPreview{ProfileName: "TestPannel"}
	for _, block := range want {
		name, rest, _ := strings.Cut(strings.TrimPrefix(block, "  - name: "), "\n")
		var major, minor, patch int
		if _, err := fmt.Sscanf(rest,
			"    version:\n      major: %d\n      minor: %d\n      patch: %d\n    enabled: true\n",
			&major, &minor, &patch); err != nil {
			t.Fatalf("fixture block for %s does not parse: %v", name, err)
		}
		preview.Mods = append(preview.Mods, exportEntry{
			FullName: name, Version: fmt.Sprintf("%d.%d.%d", major, minor, patch),
		})
	}
	if len(preview.Mods) != 11 {
		t.Fatalf("read %d mods from the fixture, want the profile's eleven", len(preview.Mods))
	}

	manifest, err := r2xManifest(preview)
	if err != nil {
		t.Fatal(err)
	}
	got := modBlocks(t, manifest)
	if len(got) != len(want) {
		t.Fatalf("wrote %d blocks, want %d", len(got), len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d differs from r2modman's:\n got: %q\nwant: %q", i, got[i], want[i])
		}
	}
}
