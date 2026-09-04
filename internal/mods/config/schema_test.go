package config

import (
	"bytes"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"
)

// TestSchemaAssignsTheWidgetTable asserts 03 §9's mapping, one setting per row.
func TestSchemaAssignsTheWidgetTable(t *testing.T) {
	want := map[string]string{
		"Enabled":          WidgetToggle,
		"DamageMultiplier": WidgetSlider,
		"RaidSize":         WidgetSlider,
		"FallbackSeed":     WidgetNumber,
		"SaveInterval":     WidgetNumber,
		"Mode":             WidgetSelect,
		"StartBiome":       WidgetSelect,
		"EnabledEvents":    WidgetMultiSelect,
		"PanelKey":         WidgetText,
		"MarkerColor":      WidgetText,
		"CampLabel":        WidgetText,
		"ChunkSize":        WidgetText,
	}
	for _, item := range allItems(schemaOf(t, "plugin/com.example.everysetting.cfg")) {
		if got := item.Widget; got != want[item.Key] {
			t.Errorf("%s: widget = %q, want %q", item.Key, got, want[item.Key])
		}
	}
}

// TestSchemaKeepsAnUnrecognisedTypeEditable asserts a type this package does not know falls
// back to a text widget with its value verbatim, rather than being dropped from the form.
func TestSchemaKeepsAnUnrecognisedTypeEditable(t *testing.T) {
	item := itemFor(t, schemaOf(t, "plugin/com.example.everysetting.cfg"), "ChunkSize")
	if item.Widget != WidgetText {
		t.Errorf("widget = %q, want %q", item.Widget, WidgetText)
	}
	if item.Current != "32,32,16" {
		t.Errorf("current = %v, want the value verbatim", item.Current)
	}
	if item.Type != "SpatialHashBucket" {
		t.Errorf("type = %q, want the declared type carried through", item.Type)
	}
}

// TestSchemaProjectsEverySettingInTheCorpus asserts nothing is dropped and every setting is
// assigned a widget the table names.
func TestSchemaProjectsEverySettingInTheCorpus(t *testing.T) {
	widgets := []string{
		WidgetToggle, WidgetSlider, WidgetNumber, WidgetSelect, WidgetMultiSelect, WidgetText,
	}
	for _, path := range corpusFiles(t) {
		t.Run(path, func(t *testing.T) {
			doc := Parse(mustRead(t, path))
			schema := doc.Schema(path)
			if got, want := len(allItems(schema)), len(doc.Settings()); got != want {
				t.Fatalf("projected %d settings, the file holds %d", got, want)
			}
			for _, item := range allItems(schema) {
				if !slices.Contains(widgets, item.Widget) {
					t.Errorf("%s: widget = %q, which the table does not name", item.Key, item.Widget)
				}
			}
		})
	}
}

// TestSchemaValuesCarryTheirJSONType asserts 11 §1.1: real booleans and numbers, strings only
// where the declared type is one.
func TestSchemaValuesCarryTheirJSONType(t *testing.T) {
	tests := []struct {
		key  string
		want any
	}{
		{"Enabled", true},
		{"DamageMultiplier", 1.5},
		{"RaidSize", float64(12)},
		{"SaveInterval", 900.5},
		{"Mode", "Hard"},
		{"PanelKey", "LeftControl + F6"},
	}
	schema := schemaOf(t, "plugin/com.example.everysetting.cfg")
	for _, tt := range tests {
		t.Run(tt.key, func(t *testing.T) {
			if got := itemFor(t, schema, tt.key).Current; got != tt.want {
				t.Errorf("current = %#v, want %#v", got, tt.want)
			}
		})
	}
}

// TestSchemaCarriesConstraints asserts the range and option lists a form renders from.
func TestSchemaCarriesConstraints(t *testing.T) {
	schema := schemaOf(t, "plugin/com.example.everysetting.cfg")

	rng := itemFor(t, schema, "DamageMultiplier").Range
	if rng == nil || rng.Min != 0 || rng.Max != 10 {
		t.Errorf("range = %+v, want 0 to 10", rng)
	}
	if got := itemFor(t, schema, "Mode").Options; !slices.Equal(got, []string{"Normal", "Hard", "Insane"}) {
		t.Errorf("options = %v", got)
	}
	if got := itemFor(t, schema, "FallbackSeed").Range; got != nil {
		t.Errorf("range = %+v on a setting with no range line", got)
	}
}

// TestSchemaJoinsAMultiLineDescription asserts a description spanning several `##` lines
// arrives whole, with the author's line breaks kept.
func TestSchemaJoinsAMultiLineDescription(t *testing.T) {
	item := itemFor(t, schemaOf(t, "edge/long-description.cfg"), "ConflictPolicy")
	if n := strings.Count(item.Description, "\n"); n != 3 {
		t.Errorf("description has %d line breaks, want 3:\n%s", n, item.Description)
	}
	if strings.HasSuffix(item.Description, "\n") || strings.Contains(item.Description, "##") {
		t.Errorf("description carries its comment markers or a trailing blank:\n%q", item.Description)
	}
}

// TestSchemaReadsThePluginHeader asserts the name and version come from the file's own
// header, which 04 §3 wants without the `v` prefix.
func TestSchemaReadsThePluginHeader(t *testing.T) {
	if got := schemaOf(t, "plugin/com.example.everysetting.cfg").Plugin; got != "Every Setting 2.4.0" {
		t.Errorf("plugin = %q, want %q", got, "Every Setting 2.4.0")
	}
	if got := schemaOf(t, "plugin/BepInEx.cfg").Plugin; got != "" {
		t.Errorf("plugin = %q on a file with no header, want empty", got)
	}
}

// TestSchemaGroupsSectionsInFileOrder asserts sections arrive in the order the file writes
// them, which is the plugin author's grouping.
func TestSchemaGroupsSectionsInFileOrder(t *testing.T) {
	sections := schemaOf(t, "plugin/BepInEx.cfg").Sections
	got := make([]string, 0, len(sections))
	for _, s := range sections {
		got = append(got, s.Name)
	}
	want := []string{
		"Caching", "Chainloader", "Harmony.Logger", "Logging",
		"Logging.Console", "Logging.Disk", "Preloader.Entrypoint",
	}
	if !slices.Equal(got, want) {
		t.Errorf("sections = %v, want %v", got, want)
	}
}

// TestSchemaMarshalsToTheDocumentedShape asserts the JSON field names 04 §3 fixes, including
// the nulls a form branches on.
func TestSchemaMarshalsToTheDocumentedShape(t *testing.T) {
	item := itemFor(t, schemaOf(t, "plugin/com.example.everysetting.cfg"), "DamageMultiplier")
	raw, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"key":"DamageMultiplier","type":"Single",` +
		`"description":"How much damage enemies deal, as a multiplier.",` +
		`"default":1,"current":1.5,"range":{"min":0,"max":10},"options":null,"widget":"slider"}`
	if string(raw) != want {
		t.Errorf("marshalled to\n %s\nwant\n %s", raw, want)
	}
}

// TestApplyWritingTheCurrentValueBackChangesNothing is the number-formatting guard: a value
// read as JSON and written back must produce the text the file already holds, or every save
// is a diff on every numeric line.
func TestApplyWritingTheCurrentValueBackChangesNothing(t *testing.T) {
	for _, path := range corpusFiles(t) {
		t.Run(path, func(t *testing.T) {
			raw := mustRead(t, path)
			doc := Parse(raw)
			changes := map[string]any{}
			for _, s := range doc.Schema(path).Sections {
				for _, item := range s.Settings {
					changes[fieldPath(s.Name, item.Key)] = item.Current
				}
			}
			if errs := doc.Apply(changes); errs != nil {
				t.Fatalf("Apply rejected the file's own values: %+v", errs)
			}
			if !bytes.Equal(doc.Bytes(), raw) {
				g, w := firstDiff(doc.Bytes(), raw)
				t.Errorf("writing the current values back changed the file:\n got %q\nwant %q", g, w)
			}
		})
	}
}

// TestApplyRejectsPerField asserts each violation comes back with the field code the frontend
// renders from (11 §2.4).
func TestApplyRejectsPerField(t *testing.T) {
	tests := []struct {
		name  string
		field string
		value any
		code  string
	}{
		{"a Single given text", "General.DamageMultiplier", "abc", CodeWrongType},
		{"a Single given a boolean", "General.DamageMultiplier", true, CodeWrongType},
		{"above the range", "General.DamageMultiplier", 11.0, CodeOutOfRange},
		{"below the range", "General.DamageMultiplier", -1.0, CodeOutOfRange},
		{"a fraction for an Int32", "General.RaidSize", 3.5, CodeWrongType},
		{"a Boolean given a number", "General.Enabled", 1.0, CodeWrongType},
		{"not an acceptable value", "General.Mode", "Brutal", CodeNotAnOption},
		{"one bad value among several", "General.EnabledEvents", "Raid, Nope", CodeNotAnOption},
		{"a setting that is not there", "General.Absent", "x", CodeUnknownSetting},
		{"a section that is not there", "Nowhere.Enabled", true, CodeUnknownSetting},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := parseFile(t, "plugin/com.example.everysetting.cfg")
			before := doc.Bytes()
			errs := doc.Apply(map[string]any{tt.field: tt.value})
			if len(errs) != 1 {
				t.Fatalf("got %d errors, want 1: %+v", len(errs), errs)
			}
			if errs[0].Field != tt.field || errs[0].Code != tt.code {
				t.Errorf("error = %+v, want field %s code %s", errs[0], tt.field, tt.code)
			}
			if errs[0].Message == "" {
				t.Error("the error carries no message for the form to render")
			}
			if !bytes.Equal(doc.Bytes(), before) {
				t.Error("a rejected Apply still modified the document")
			}
		})
	}
}

// TestApplyReportsEveryProblemAndWritesNothing asserts 11 §2.4's one request, one response:
// a patch mixing valid and invalid keys writes nothing and names every problem.
func TestApplyReportsEveryProblemAndWritesNothing(t *testing.T) {
	doc := parseFile(t, "plugin/com.example.everysetting.cfg")
	before := doc.Bytes()

	errs := doc.Apply(map[string]any{
		"General.Enabled":          false,
		"General.DamageMultiplier": 99.0,
		"General.Mode":             "Brutal",
	})
	if len(errs) != 2 {
		t.Fatalf("got %d errors, want 2: %+v", len(errs), errs)
	}
	if !bytes.Equal(doc.Bytes(), before) {
		t.Error("the valid field was written even though the patch failed")
	}
}

// TestApplyWritesEveryValidChange asserts an accepted patch reaches the file, one line each.
func TestApplyWritesEveryValidChange(t *testing.T) {
	doc := parseFile(t, "plugin/com.example.everysetting.cfg")
	if errs := doc.Apply(map[string]any{
		"General.Enabled":          false,
		"General.DamageMultiplier": 2.0,
		"General.Mode":             "Insane",
		"General.EnabledEvents":    "Raid, Eclipse",
	}); errs != nil {
		t.Fatalf("Apply: %+v", errs)
	}
	out := string(doc.Bytes())
	for _, want := range []string{
		"Enabled = false", "DamageMultiplier = 2", "Mode = Insane", "EnabledEvents = Raid, Eclipse",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("the file does not contain %q", want)
		}
	}
}

// TestApplyAddressesASectionContainingDots asserts `Section.Key` splits on the last dot, so a
// dotted section name resolves rather than being read as a section and a compound key.
func TestApplyAddressesASectionContainingDots(t *testing.T) {
	doc := parseFile(t, "plugin/BepInEx.cfg")
	if errs := doc.Apply(map[string]any{"Logging.Console.Enabled": false}); errs != nil {
		t.Fatalf("Apply: %+v", errs)
	}
	if got, _ := doc.Get("Logging.Console", "Enabled"); got != "false" {
		t.Errorf("[Logging.Console] Enabled = %q, want false", got)
	}
	if got, _ := doc.Get("Logging.Disk", "Enabled"); got != "true" {
		t.Errorf("[Logging.Disk] Enabled = %q; the wrong section was written", got)
	}
}

func schemaOf(t *testing.T, name string) Schema {
	t.Helper()
	return parseFile(t, name).Schema(name)
}

func allItems(s Schema) []SchemaItem {
	var out []SchemaItem
	for _, sec := range s.Sections {
		out = append(out, sec.Settings...)
	}
	return out
}

func itemFor(t *testing.T, s Schema, key string) SchemaItem {
	t.Helper()
	items := allItems(s)
	for i := range items {
		if items[i].Key == key {
			return items[i]
		}
	}
	t.Fatalf("%s is not in the schema for %s", key, s.File)
	return SchemaItem{}
}

func fieldPath(section, key string) string {
	if section == "" {
		return key
	}
	return section + "." + key
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}
