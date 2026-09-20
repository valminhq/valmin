package diag

import (
	"archive/zip"
	"bytes"
	"io"
	"strings"
	"testing"
	"time"
)

// bundleOf builds a bundle from cfg and returns every entry's contents by name.
func bundleOf(t *testing.T, in *Input) map[string]string {
	t.Helper()

	var buf bytes.Buffer
	report := Collect(t.Context(), in)
	if err := WriteBundle(&buf, &report, NewConfigView(in.Config)); err != nil {
		t.Fatalf("write bundle: %v", err)
	}
	z, err := zip.NewReader(bytes.NewReader(buf.Bytes()), int64(buf.Len()))
	if err != nil {
		t.Fatalf("read bundle: %v", err)
	}

	out := make(map[string]string, len(z.File))
	for _, f := range z.File {
		r, err := f.Open()
		if err != nil {
			t.Fatalf("open %s: %v", f.Name, err)
		}
		body, err := io.ReadAll(r)
		_ = r.Close()
		if err != nil {
			t.Fatalf("read %s: %v", f.Name, err)
		}
		out[f.Name] = string(body)
	}
	return out
}

// TestTheBundleCarriesNoSecretPathOrIdentifier is the assertion the support bundle
// exists to keep: it is attached to public issues unedited, so a value that would
// identify the operator or unlock anything must never be in it (D14, 11 §9).
func TestTheBundleCarriesNoSecretPathOrIdentifier(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Config.Data.Root = "/home/torvalds/valmin-data"
	in.Config.Data.HostRoot = "/home/torvalds/valmin-data"
	in.Config.DB.DSN = "/home/torvalds/valmin-data/panel.db?_txlock=immediate"
	in.Config.Secrets.MasterKeyFile = "/home/torvalds/valmin-data/master.key"
	in.Config.Server.ExternalURL = "https://valheim.torvalds.example"
	in.Instances = []Instance{{
		ID: "0198f0a1", Name: "midgard", State: "running", Running: true,
		BasePort: 2456, ExpectedPorts: []int{2456, 2457}, BoundPorts: []int{2456, 2457},
	}}

	forbidden := map[string]string{
		"the data root":               "/home/torvalds/valmin-data",
		"the operator's account name": "torvalds",
		"the database DSN":            "_txlock=immediate",
		"the master key path":         "master.key",
		"the panel's hostname":        "valheim.torvalds.example",
		"a player-facing world":       "Midgard",
	}

	files := bundleOf(t, in)
	for name, body := range files {
		for what, secret := range forbidden {
			if strings.Contains(body, secret) {
				t.Errorf("%s leaks %s (%q)", name, what, secret)
			}
		}
	}
}

// TestTheBundleOmitsVerbatimProbeOutput asserts the one class of text this package did
// not compose, and so cannot vouch for, stays out of an archive meant for a public issue.
func TestTheBundleOmitsVerbatimProbeOutput(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Recorded = map[string]Observation{
		CheckHostRoot: {
			OK:        false,
			Detail:    "data.root = /home/torvalds/valmin-data does not match",
			CheckedAt: in.Now,
		},
	}

	report := Collect(t.Context(), in)
	if find(t, &report, CheckHostRoot).Diagnostic == "" {
		t.Fatal("the report itself dropped the probe output; the page needs it")
	}
	if body := bundleOf(t, in)["report.json"]; strings.Contains(body, "/home/torvalds") {
		t.Error("the bundle carries verbatim probe output naming a filesystem path")
	}
}

// TestTheBundleCarriesWhatItPromises asserts the README is not describing an archive
// with different contents.
func TestTheBundleCarriesWhatItPromises(t *testing.T) {
	t.Parallel()

	files := bundleOf(t, testInput())
	for _, name := range []string{"README.txt", "report.json", "config.json"} {
		if _, ok := files[name]; !ok {
			t.Errorf("bundle has no %s", name)
		}
	}
	if len(files) != 3 {
		t.Errorf("bundle holds %d entries, want exactly the three the README names", len(files))
	}
	for _, want := range []string{"report.json", "config.json"} {
		if !strings.Contains(files["README.txt"], want) {
			t.Errorf("README does not mention %s", want)
		}
	}
}

// TestTheBundleKeepsTheFactsDerivedFromThePathsItOmits asserts redaction did not cost the
// bundle its diagnostic value.
func TestTheBundleKeepsTheFactsDerivedFromThePathsItOmits(t *testing.T) {
	t.Parallel()

	in := testInput()
	in.Config.Data.Root = "/home/torvalds/valmin-data"
	files := bundleOf(t, in)

	for _, want := range []string{"ext4", "free_space_floor_bytes", "external_url_scheme"} {
		if !strings.Contains(files["report.json"]+files["config.json"], want) {
			t.Errorf("bundle omits %s, which is the point of omitting the path", want)
		}
	}
}

// TestTwoBundlesOfTheSameStateAreIdentical asserts the archive carries no incidental
// timestamp, so two bundles can be diffed.
func TestTwoBundlesOfTheSameStateAreIdentical(t *testing.T) {
	t.Parallel()

	in := testInput()
	report := Collect(t.Context(), in)

	var first, second bytes.Buffer
	for _, w := range []*bytes.Buffer{&first, &second} {
		if err := WriteBundle(w, &report, NewConfigView(in.Config)); err != nil {
			t.Fatalf("write bundle: %v", err)
		}
	}
	if !bytes.Equal(first.Bytes(), second.Bytes()) {
		t.Error("two bundles of one report differ")
	}
}

func TestBundleNameIsSortableAndScoped(t *testing.T) {
	t.Parallel()

	got := BundleName(time.Date(2026, 9, 20, 14, 5, 6, 0, time.UTC))
	if got != "valmin-support-20260920-140506.zip" {
		t.Errorf("BundleName = %q", got)
	}
}
