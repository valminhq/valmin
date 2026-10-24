package profile

import (
	"archive/zip"
	"bytes"
	"os"
	"slices"
	"strings"
	"testing"
)

var testLimits = Limits{ManifestBytes: 1 << 16, Configs: 3, ConfigBytes: 64, TotalBytes: 100}

// zipOf builds an archive from name/content pairs.
func zipOf(t *testing.T, files map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	for name, content := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(content)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func fixture(t *testing.T, name string) string {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestReadManifest(t *testing.T) {
	galeMods := []Mod{
		{FullName: "denikson-BepInExPack_Valheim", Version: "5.4.2333", Source: "thunderstore", Enabled: true},
		{FullName: "Hexium-OnlyHere", Version: "1.2.3", Source: "hexium", Enabled: false},
		{FullName: "Author-NoEnabledKey", Version: "0.1.0", Enabled: true},
	}
	tests := []struct {
		name     string
		file     string
		wantName string
		wantMods []Mod
		wantLen  int
	}{
		{name: "gale", file: "gale.r2x", wantName: "Gale Pack", wantMods: galeMods},
		{name: "r2modman", file: "r2modman.r2x", wantName: "TestPannel", wantLen: 11},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Read(zipOf(t, map[string]string{"export.r2x": fixture(t, tt.file)}), testLimits)
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			if got.Name != tt.wantName {
				t.Errorf("Name = %q, want %q", got.Name, tt.wantName)
			}
			if tt.wantMods != nil {
				if !slices.Equal(got.Mods, tt.wantMods) {
					t.Errorf("Mods = %+v, want %+v", got.Mods, tt.wantMods)
				}
			}
			if tt.wantLen != 0 && len(got.Mods) != tt.wantLen {
				t.Errorf("len(Mods) = %d, want %d", len(got.Mods), tt.wantLen)
			}
		})
	}
}

func TestReadConfigs(t *testing.T) {
	manifest := fixture(t, "gale.r2x")
	tests := []struct {
		name    string
		files   map[string]string
		want    []string
		wantErr string
	}{
		{
			name: "both layouts",
			files: map[string]string{
				"export.r2x": manifest, "BepInEx/config/a.cfg": "x", "config/b.cfg": "y",
			},
			want: []string{"a.cfg", "b.cfg"},
		},
		{
			name: "nested, binary and non-cfg entries are ignored",
			files: map[string]string{
				"export.r2x": manifest, "BepInEx/config/sub/a.cfg": "x", "BepInEx/plugins/m.dll": "MZ",
				"BepInEx/config/readme.txt": "z", "mods.yml": "m", "BepInEx/config/.cfg": "q",
			},
		},
		{
			name: "windows separators",
			files: map[string]string{
				"export.r2x": manifest, `BepInEx\config\a.cfg`: "x",
			},
			want: []string{"a.cfg"},
		},
		{
			name: "oversize config",
			files: map[string]string{
				"export.r2x": manifest, "config/a.cfg": strings.Repeat("x", 65),
			},
			wantErr: "larger than 64 bytes",
		},
		{
			name: "too many configs",
			files: map[string]string{
				"export.r2x": manifest, "config/1.cfg": "", "config/2.cfg": "", "config/3.cfg": "", "config/4.cfg": "",
			},
			wantErr: "more than 3 config files",
		},
		{
			name: "configs too large together",
			files: map[string]string{
				"export.r2x":   manifest,
				"config/a.cfg": strings.Repeat("x", 60),
				"config/b.cfg": strings.Repeat("y", 60),
			},
			wantErr: "100 bytes together",
		},
		{name: "no manifest", files: map[string]string{"config/a.cfg": "x"}, wantErr: "no export.r2x"},
		{
			name:    "mod without a version",
			files:   map[string]string{"export.r2x": "profileName: p\nmods:\n  - name: A-B\n"},
			wantErr: "without a name or version",
		},
		{
			name:    "manifest too large",
			files:   map[string]string{"export.r2x": strings.Repeat("#", 1<<16+1)},
			wantErr: "larger than",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Read(zipOf(t, tt.files), testLimits)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("Read() error = %v, want it to contain %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("Read() error = %v", err)
			}
			var files []string
			for _, c := range got.Configs {
				files = append(files, c.File)
			}
			slices.Sort(files)
			if !slices.Equal(files, tt.want) {
				t.Errorf("configs = %v, want %v", files, tt.want)
			}
		})
	}
}

func TestReadRejectsANonArchive(t *testing.T) {
	if _, err := Read([]byte("not a zip"), testLimits); err == nil {
		t.Error("Read(non-zip) error = nil, want an error")
	}
}

func TestArchiveRoundTrip(t *testing.T) {
	mods := []Mod{
		{FullName: "denikson-BepInExPack_Valheim", Version: "5.4.2333", Enabled: true},
		{FullName: "Hexium-OnlyHere", Version: "1.2.3", Source: "hexium", Enabled: true},
	}
	archive, err := Archive(`My "pack"`, mods, false)
	if err != nil {
		t.Fatalf("Archive() error = %v", err)
	}
	got, err := Read(archive, testLimits)
	if err != nil {
		t.Fatalf("Read(Archive()) error = %v", err)
	}
	if got.Name != `My "pack"` {
		t.Errorf("Name = %q", got.Name)
	}
	if !slices.Equal(got.Mods, mods) {
		t.Errorf("Mods = %+v, want %+v", got.Mods, mods)
	}
}

func TestArchiveRefuses(t *testing.T) {
	tests := []struct {
		name string
		mod  Mod
	}{
		{"version without a patch", Mod{FullName: "A-B", Version: "1.2"}},
		{"registry that is not a plain name", Mod{FullName: "A-B", Version: "1.2.3", Source: "x\nmods: []"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Archive("p", []Mod{tt.mod}, false); err == nil {
				t.Error("Archive() error = nil, want an error")
			}
		})
	}
}

func TestArchiveNamesEveryRegistryOnRequest(t *testing.T) {
	mods := []Mod{
		{FullName: "A-Ts", Version: "1.0.0", Source: "thunderstore", Enabled: true},
		{FullName: "A-Hex", Version: "1.0.0", Source: "hexium", Enabled: true},
		{FullName: "A-None", Version: "1.0.0", Enabled: true},
	}
	for _, every := range []bool{false, true} {
		archive, err := Archive("p", mods, every)
		if err != nil {
			t.Fatal(err)
		}
		got, err := Read(archive, testLimits)
		if err != nil {
			t.Fatal(err)
		}
		sources := []string{got.Mods[0].Source, got.Mods[1].Source, got.Mods[2].Source}
		want := []string{"", "hexium", ""}
		if every {
			want = []string{"thunderstore", "hexium", ""}
		}
		if !slices.Equal(sources, want) {
			t.Errorf("everySource=%v: sources = %q, want %q", every, sources, want)
		}
	}
}

func TestArchiveIsDeterministic(t *testing.T) {
	mods := []Mod{{FullName: "A-B", Version: "1.0.0", Enabled: true}}
	first, err := Archive("p", mods, false)
	if err != nil {
		t.Fatal(err)
	}
	second, _ := Archive("p", mods, false)
	if !bytes.Equal(first, second) {
		t.Error("two archives of the same mods differ")
	}
}

// modBlocks splits a manifest into its per-mod blocks, sorted: r2modman lists mods in install
// order and Archive in the order it is given, and the comparison is about grammar, not order.
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
	slices.Sort(blocks)
	return blocks
}

// TestArchiveMatchesARealR2modmanExport compares the manifest Archive writes with export.r2x
// from a real r2modman export of an eleven-mod Valheim profile, block for block.
func TestArchiveMatchesARealR2modmanExport(t *testing.T) {
	export := fixture(t, "r2modman.r2x")
	read, err := Read(zipOf(t, map[string]string{"export.r2x": export}), testLimits)
	if err != nil {
		t.Fatal(err)
	}
	if len(read.Mods) != 11 {
		t.Fatalf("read %d mods from the fixture, want the profile's eleven", len(read.Mods))
	}
	archive, err := Archive(read.Name, read.Mods, false)
	if err != nil {
		t.Fatal(err)
	}
	zr, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		t.Fatal(err)
	}
	f, err := zr.Open("export.r2x")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var written bytes.Buffer
	if _, err := written.ReadFrom(f); err != nil {
		t.Fatal(err)
	}

	want, got := modBlocks(t, export), modBlocks(t, written.String())
	if !slices.Equal(got, want) {
		t.Errorf("manifest differs from r2modman's:\n got: %q\nwant: %q", got, want)
	}
}
