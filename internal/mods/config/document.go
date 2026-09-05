// Package config parses and edits BepInEx `.cfg` files, preserving every byte it does not
// change: comments, spacing, ordering and line endings (03 §9, ADR-010).
package config

import (
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ErrNoSuchSetting is returned by Set for a section and key the file does not contain.
var ErrNoSuchSetting = errors.New("modconfig: no such setting")

// ErrValueSpansLines is returned by Set for a value carrying a newline, which would split one
// setting into two. A lone CR is allowed: it does not end a line.
var ErrValueSpansLines = errors.New("modconfig: a value cannot contain a newline")

// Document is a `.cfg` file held as the lines it was parsed from, each keeping its raw bytes
// and its terminator. Serialising is concatenation, so an unedited line is copied rather than
// regenerated (B10).
type Document struct {
	lines []docLine
	index map[settingKey]int
}

// settingKey addresses a setting. The key is folded, the section is not.
type settingKey struct {
	section string
	key     string
}

type lineKind int

const (
	// kindOther covers section headers, blank lines and anything the grammar does not
	// describe. Only comments and settings need telling apart.
	kindOther lineKind = iota
	kindComment
	kindSetting
)

// docLine is one line of the file. raw is verbatim; valStart and valEnd bound the value
// token within it, and are the only bytes an edit ever replaces.
type docLine struct {
	raw      string
	kind     lineKind
	section  string
	key      string
	valStart int
	valEnd   int
}

// Setting is one assignment and the comment block above it, handed over uninterpreted.
type Setting struct {
	Section  string
	Key      string
	Value    string
	Comments []string
}

// Parse reads a `.cfg` into a Document. It cannot fail: a line the grammar does not describe
// is kept opaque and serialises back unchanged.
func Parse(raw []byte) *Document {
	doc := &Document{index: make(map[settingKey]int)}
	section := ""
	for text := range strings.Lines(string(raw)) {
		ln := docLine{raw: text, section: section}
		body := strings.TrimSpace(strings.TrimRight(text, "\r\n"))
		switch {
		case strings.HasPrefix(body, "[") && strings.HasSuffix(body, "]"):
			section = body[1 : len(body)-1]
		case strings.HasPrefix(body, "#"):
			ln.kind = kindComment
		default:
			ln.kind = kindOther
			if key, start, end, ok := splitAssignment(text); ok {
				ln.kind, ln.key, ln.valStart, ln.valEnd = kindSetting, key, start, end
			}
		}
		if ln.kind == kindSetting {
			// First match wins: a duplicate is legal, and BepInEx reads the first.
			k := settingKey{ln.section, strings.ToLower(ln.key)}
			if _, seen := doc.index[k]; !seen {
				doc.index[k] = len(doc.lines)
			}
		}
		doc.lines = append(doc.lines, ln)
	}
	return doc
}

// splitAssignment locates the key and the bounds of the value token in one line. Only the
// first `=` separates, so a value may contain further ones.
func splitAssignment(raw string) (key string, valStart, valEnd int, ok bool) {
	eq := strings.Index(raw, "=")
	if eq < 0 {
		return "", 0, 0, false
	}
	key = strings.TrimSpace(raw[:eq])
	if key == "" {
		return "", 0, 0, false
	}
	rest := raw[eq+1:]
	lead := len(rest) - len(strings.TrimLeft(rest, " \t"))
	trail := len(strings.TrimRight(rest, " \t\r\n"))
	// An all-whitespace remainder trims past its own start, leaving an empty value.
	trail = max(trail, lead)
	return key, eq + 1 + lead, eq + 1 + trail, true
}

// Bytes serialises the document, returning an unedited one byte for byte.
func (d *Document) Bytes() []byte {
	var b strings.Builder
	for _, ln := range d.lines {
		b.WriteString(ln.raw)
	}
	return []byte(b.String())
}

// Get reads a setting's value unpadded. ok separates a missing setting from an empty one.
func (d *Document) Get(section, key string) (value string, ok bool) {
	i, ok := d.index[settingKey{section, strings.ToLower(key)}]
	if !ok {
		return "", false
	}
	ln := d.lines[i]
	return ln.raw[ln.valStart:ln.valEnd], true
}

// Set replaces one setting's value and touches nothing else on the line: the key, the spacing
// around the `=`, any trailing content and the line ending are carried through.
func (d *Document) Set(section, key, value string) error {
	if strings.Contains(value, "\n") {
		return fmt.Errorf("set [%s] %s: %w", section, key, ErrValueSpansLines)
	}
	i, ok := d.index[settingKey{section, strings.ToLower(key)}]
	if !ok {
		return fmt.Errorf("set [%s] %s: %w", section, key, ErrNoSuchSetting)
	}
	ln := &d.lines[i]
	ln.raw = ln.raw[:ln.valStart] + value + ln.raw[ln.valEnd:]
	ln.valEnd = ln.valStart + len(value)
	return nil
}

// Settings lists the settings in effect, in file order. A key assigned twice in one section
// appears once, as only the first assignment is reachable by Get and Set.
func (d *Document) Settings() []Setting {
	var out []Setting
	for i, ln := range d.lines {
		if ln.kind != kindSetting || d.index[settingKey{ln.section, strings.ToLower(ln.key)}] != i {
			continue
		}
		out = append(out, Setting{
			Section:  ln.section,
			Key:      ln.key,
			Value:    ln.raw[ln.valStart:ln.valEnd],
			Comments: d.commentsAbove(i),
		})
	}
	return out
}

// commentsAbove collects the comment run directly above a setting, in file order and without
// line endings. A blank line, section header or other setting ends the run.
func (d *Document) commentsAbove(i int) []string {
	var out []string
	for j := i - 1; j >= 0 && d.lines[j].kind == kindComment; j-- {
		out = append(out, strings.TrimRight(d.lines[j].raw, "\r\n"))
	}
	slices.Reverse(out)
	return out
}
