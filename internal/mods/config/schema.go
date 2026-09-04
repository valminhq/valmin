package config

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"strings"
)

// Widget names the control the frontend renders. The backend decides it so the SPA never
// learns a Valheim type name (02 §2.1, F2).
const (
	WidgetToggle      = "toggle"
	WidgetSlider      = "slider"
	WidgetNumber      = "number"
	WidgetSelect      = "select"
	WidgetMultiSelect = "multi-select"
	WidgetText        = "text"
)

// Field codes for 11 §2.4's per-field validation errors. They are their own small registry
// because the frontend renders each one differently.
const (
	CodeUnknownSetting = "unknown_setting"
	CodeWrongType      = "wrong_type"
	CodeOutOfRange     = "out_of_range"
	CodeNotAnOption    = "not_an_option"
)

// Schema is 04 §3's response for one `.cfg`: the file's settings grouped by section, each
// carrying the type, constraints and widget a form needs to render it.
//
// Values are real JSON booleans and numbers where the setting's declared type says so, and
// strings otherwise (11 §1.1). A type this package does not recognise is not an error: the
// value comes through verbatim as a string with a text widget, so the setting stays
// editable rather than disappearing from the form.
type Schema struct {
	File     string          `json:"file"`
	Plugin   string          `json:"plugin"`
	Sections []SchemaSection `json:"sections"`
}

type SchemaSection struct {
	Name     string       `json:"name"`
	Settings []SchemaItem `json:"settings"`
}

type SchemaItem struct {
	Key         string       `json:"key"`
	Type        string       `json:"type"`
	Description string       `json:"description"`
	Default     any          `json:"default"`
	Current     any          `json:"current"`
	Range       *SchemaRange `json:"range"`
	Options     []string     `json:"options"`
	Widget      string       `json:"widget"`
	// Step is the increment a numeric control moves by, or 0 for a continuous one. It is
	// decided here for the same reason the widget is: only this side knows a type that
	// cannot hold a fraction (F2).
	Step float64 `json:"step"`
}

type SchemaRange struct {
	Min float64 `json:"min"`
	Max float64 `json:"max"`
}

// FieldError is one entry of 11 §2.4's `details.fields`.
type FieldError struct {
	Field   string `json:"field"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// Schema projects the document into 04 §3's response shape, in file order.
func (d *Document) Schema(file string) Schema {
	out := Schema{File: file, Plugin: d.plugin(), Sections: []SchemaSection{}}
	for _, s := range d.Settings() {
		m := parseMetadata(s.Comments)
		item := SchemaItem{
			Key:         s.Key,
			Type:        m.typ,
			Description: m.description,
			Default:     typed(m.typ, m.def),
			Current:     typed(m.typ, s.Value),
			Range:       m.rng,
			Options:     m.options,
			Widget:      widgetFor(&m),
			Step:        stepFor(&m),
		}
		if n := len(out.Sections); n > 0 && out.Sections[n-1].Name == s.Section {
			out.Sections[n-1].Settings = append(out.Sections[n-1].Settings, item)
			continue
		}
		out.Sections = append(out.Sections, SchemaSection{Name: s.Section, Settings: []SchemaItem{item}})
	}
	return out
}

// plugin reads the name and version from the file's own header. It is not joined against
// instance_mods, which the file carries no link to.
func (d *Document) plugin() string {
	const prefix = "## Settings file was created by plugin "
	for _, ln := range d.lines {
		if ln.kind != kindComment {
			break
		}
		if name, ok := strings.CutPrefix(strings.TrimRight(ln.raw, "\r\n"), prefix); ok {
			name = strings.TrimSpace(name)
			// The header writes the version as `vX.Y.Z`; 04 §3 wants it bare.
			if i := strings.LastIndex(name, " v"); i >= 0 {
				name = name[:i+1] + name[i+2:]
			}
			return name
		}
	}
	return ""
}

// metadata is what a setting's comment block declares about it.
type metadata struct {
	description string
	typ         string
	def         string
	rng         *SchemaRange
	options     []string
	multiple    bool
}

// parseMetadata reads a comment block. Lines it does not recognise are ignored here and
// preserved by the document, which is 03 §9 rule 4: a newer BepInEx costs nothing.
func parseMetadata(comments []string) metadata {
	var m metadata
	var description []string
	for _, c := range comments {
		switch {
		case strings.HasPrefix(c, "## "):
			description = append(description, strings.TrimPrefix(c, "## "))
		case c == "##":
			description = append(description, "")
		case cut(c, "# Setting type:", &m.typ):
		case cut(c, "# Default value:", &m.def):
		case strings.HasPrefix(c, "# Acceptable values:"):
			m.options = splitList(strings.TrimPrefix(c, "# Acceptable values:"))
		case strings.HasPrefix(c, "# Acceptable value range:"):
			m.rng = parseRange(strings.TrimPrefix(c, "# Acceptable value range:"))
		case strings.HasPrefix(c, "# Multiple values can be set"):
			m.multiple = true
		}
	}
	m.description = strings.TrimSpace(strings.Join(description, "\n"))
	return m
}

// cut assigns the trimmed remainder of a `# Key: value` line and reports whether it matched.
func cut(line, prefix string, dst *string) bool {
	rest, ok := strings.CutPrefix(line, prefix)
	if ok {
		*dst = strings.TrimSpace(rest)
	}
	return ok
}

// splitList reads a comma-separated metadata list.
func splitList(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		if part = strings.TrimSpace(part); part != "" {
			out = append(out, part)
		}
	}
	return out
}

// parseRange reads `From X to Y`. An unrecognised shape yields no range, which downgrades a
// slider to a number input rather than inventing bounds.
func parseRange(s string) *SchemaRange {
	fields := strings.Fields(s)
	if len(fields) != 4 || fields[0] != "From" || fields[2] != "to" {
		return nil
	}
	lo, err := strconv.ParseFloat(fields[1], 64)
	if err != nil {
		return nil
	}
	hi, err := strconv.ParseFloat(fields[3], 64)
	if err != nil {
		return nil
	}
	return &SchemaRange{Min: lo, Max: hi}
}

// numericTypes are the ones 03 §9 records. Anything else falls through to a text widget
// rather than being guessed at, which Q45 requires while five of its types stay unobserved.
var numericTypes = map[string]bool{"Int32": true, "Single": true, "Double": true}

// widgetFor implements 03 §9's mapping table. The table names `String` for a select; any
// type carrying `Acceptable values` is treated the same, since an enum lists its values the
// same way and would otherwise fall through to free text.
func widgetFor(m *metadata) string {
	switch {
	case m.typ == "Boolean":
		return WidgetToggle
	case len(m.options) > 0 && m.multiple:
		return WidgetMultiSelect
	case len(m.options) > 0:
		return WidgetSelect
	case numericTypes[m.typ] && m.rng != nil:
		return WidgetSlider
	case numericTypes[m.typ]:
		return WidgetNumber
	default:
		return WidgetText
	}
}

// stepFor sizes a numeric control's increment. A whole-number type steps by one whatever its
// range; anything else with bounds gets a hundredth of the span, which is a slider's
// resolution rather than a claim about the value's precision.
func stepFor(m *metadata) float64 {
	if m.typ == "Int32" {
		return 1
	}
	if m.rng != nil && m.rng.Max > m.rng.Min {
		return (m.rng.Max - m.rng.Min) / 100
	}
	return 0
}

// typed converts a raw value to the JSON type its declared type implies. A value that does
// not parse comes back as the string it is, so the form shows what the file holds.
func typed(typ, raw string) any {
	switch {
	case typ == "Boolean":
		if b, err := strconv.ParseBool(strings.TrimSpace(raw)); err == nil {
			return b
		}
	case numericTypes[typ]:
		if f, err := strconv.ParseFloat(strings.TrimSpace(raw), 64); err == nil {
			return f
		}
	}
	return raw
}

// Apply validates a `{"Section.Key": value}` patch and writes it. Nothing is written unless
// every change is valid, so one bad field cannot leave the file half-edited (11 §2.4).
func (d *Document) Apply(changes map[string]any) []FieldError {
	type write struct{ section, key, value string }
	var (
		writes []write
		errs   []FieldError
	)
	for _, field := range slices.Sorted(maps.Keys(changes)) {
		section, key := splitField(field)
		s, ok := d.setting(section, key)
		if !ok {
			errs = append(errs, FieldError{
				Field: field, Code: CodeUnknownSetting,
				Message: "This setting is not in the file. Start the server once to regenerate it.",
			})
			continue
		}
		m := parseMetadata(s.Comments)
		value, fe := check(field, &m, changes[field])
		if fe != nil {
			errs = append(errs, *fe)
			continue
		}
		writes = append(writes, write{section, key, value})
	}
	if len(errs) > 0 {
		return errs
	}
	for _, w := range writes {
		if err := d.Set(w.section, w.key, w.value); err != nil {
			return []FieldError{{Field: w.section + "." + w.key, Code: CodeWrongType, Message: err.Error()}}
		}
	}
	return nil
}

// splitField reads a `Section.Key` path. The split is on the last dot because a section name
// may contain them (`Logging.Console`) and a key does not.
func splitField(field string) (section, key string) {
	if i := strings.LastIndex(field, "."); i >= 0 {
		return field[:i], field[i+1:]
	}
	return "", field
}

// setting finds one setting with its comment block.
func (d *Document) setting(section, key string) (Setting, bool) {
	i, ok := d.index[settingKey{section, strings.ToLower(key)}]
	if !ok {
		return Setting{}, false
	}
	ln := d.lines[i]
	return Setting{
		Section:  ln.section,
		Key:      ln.key,
		Value:    ln.raw[ln.valStart:ln.valEnd],
		Comments: d.commentsAbove(i),
	}, true
}

// check validates one incoming value against its declared type and constraints, returning
// the text to write.
func check(field string, m *metadata, value any) (string, *FieldError) {
	fail := func(code, msg string) (string, *FieldError) {
		return "", &FieldError{Field: field, Code: code, Message: msg}
	}
	switch v := value.(type) {
	case bool:
		if m.typ != "Boolean" {
			return fail(CodeWrongType, fmt.Sprintf("Expected a %s, not true or false.", m.typ))
		}
		return strconv.FormatBool(v), nil
	case float64:
		if !numericTypes[m.typ] {
			return fail(CodeWrongType, fmt.Sprintf("Expected a %s, not a number.", m.typ))
		}
		if m.rng != nil && (v < m.rng.Min || v > m.rng.Max) {
			return fail(CodeOutOfRange, fmt.Sprintf("Must be between %s and %s.",
				number(m.rng.Min), number(m.rng.Max)))
		}
		if m.typ == "Int32" && v != float64(int64(v)) {
			return fail(CodeWrongType, "Must be a whole number.")
		}
		return number(v), nil
	case string:
		if m.typ == "Boolean" || numericTypes[m.typ] {
			return fail(CodeWrongType, fmt.Sprintf("Expected a %s.", m.typ))
		}
		if bad, ok := notAnOption(m, v); !ok {
			return fail(CodeNotAnOption, fmt.Sprintf("%q is not one of: %s.",
				bad, strings.Join(m.options, ", ")))
		}
		return v, nil
	default:
		return fail(CodeWrongType, "This value cannot be written to a config file.")
	}
}

// notAnOption checks a value against `Acceptable values`. A multi-value setting carries
// several at once, so each is checked; the first that misses is named.
func notAnOption(m *metadata, value string) (bad string, ok bool) {
	if len(m.options) == 0 {
		return "", true
	}
	parts := []string{value}
	if m.multiple {
		parts = splitList(value)
	}
	for _, part := range parts {
		if !slices.Contains(m.options, part) {
			return part, false
		}
	}
	return "", true
}

// number formats a float without a trailing zero, so writing 1 back produces `1` rather than
// `1.000000` and a save is not a diff on every numeric line.
func number(f float64) string {
	return strconv.FormatFloat(f, 'g', -1, 64)
}
