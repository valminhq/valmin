// Package sharecode encodes a server template as a short pasteable code: Prefix followed by
// unpadded base64url of a deflated text payload. The payload's first line holds the name and
// launch settings in URL query form; each mod line is "<full_name> <version>[ <side>[ <registry>]]";
// config follows as a "#<file>" line and that file's "[Section]" and "Key = Value" lines.
package sharecode

import (
	"bytes"
	"compress/flate"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/valminhq/valmin/internal/instance/control"
)

// Prefix starts every code this version writes and reads.
const Prefix = "valmin1:"

const thunderstore = "thunderstore"

// Template is what a code carries. Each config holds only the settings the template sets.
type Template struct {
	Name    string
	Launch  control.ManifestLaunch
	Mods    []control.ManifestMod
	Configs []control.ManifestConfig
}

// Errors Decode reports, worded for the person who pasted the code.
var (
	ErrNotACode   = errors.New("this is not a Valmin template code")
	ErrNewer      = errors.New("this code was made by a newer Valmin; update this panel to import it")
	ErrDamaged    = errors.New("this code is damaged or incomplete; copy it again")
	ErrTooLarge   = errors.New("this template is larger than this panel accepts")
	errBadPayload = errors.New("this code holds a template this panel cannot read")
)

// sideCodes maps each side tag to its letter; "-" is untagged.
var sideCodes = map[string]string{
	"server_only": "s", "client_required": "r", "client_optional": "o", "unknown": "-", "": "-",
}

// Encode renders t as a code.
func Encode(t *Template) (string, error) {
	header, err := encodeHeader(t)
	if err != nil {
		return "", err
	}
	var p strings.Builder
	p.WriteString(header)
	p.WriteByte('\n')
	for _, m := range t.Mods {
		line, err := encodeMod(m)
		if err != nil {
			return "", err
		}
		p.WriteString(line)
		p.WriteByte('\n')
	}
	for _, c := range t.Configs {
		if strings.ContainsAny(c.File, "\r\n") {
			return "", fmt.Errorf("config file name %q spans lines", c.File)
		}
		p.WriteString("#" + c.File + "\n")
		for line := range strings.Lines(c.Content) {
			if strings.HasPrefix(line, "#") {
				return "", fmt.Errorf("config %s has a line starting with #", c.File)
			}
			p.WriteString(line)
		}
		if c.Content != "" && !strings.HasSuffix(c.Content, "\n") {
			p.WriteByte('\n')
		}
	}

	var buf bytes.Buffer
	w, err := flate.NewWriter(&buf, flate.BestCompression)
	if err != nil {
		return "", fmt.Errorf("start compression: %w", err)
	}
	if _, err := w.Write([]byte(p.String())); err != nil {
		return "", fmt.Errorf("compress template: %w", err)
	}
	if err := w.Close(); err != nil {
		return "", fmt.Errorf("compress template: %w", err)
	}
	return Prefix + base64.RawURLEncoding.EncodeToString(buf.Bytes()), nil
}

// Decode reads a code, ignoring whitespace. maxBytes bounds the decompressed payload.
func Decode(code string, maxBytes int64) (*Template, error) {
	code = strings.Join(strings.Fields(code), "")
	body, ok := strings.CutPrefix(code, Prefix)
	if !ok {
		if isLaterVersion(code) {
			return nil, ErrNewer
		}
		return nil, ErrNotACode
	}
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(body, "="))
	if err != nil {
		return nil, ErrDamaged
	}
	payload, err := io.ReadAll(io.LimitReader(flate.NewReader(bytes.NewReader(raw)), maxBytes+1))
	if err != nil {
		return nil, ErrDamaged
	}
	if int64(len(payload)) > maxBytes {
		return nil, ErrTooLarge
	}
	if !utf8.Valid(payload) {
		return nil, ErrDamaged
	}
	return decodePayload(string(payload))
}

// isLaterVersion reports whether code carries the prefix of a format version above this one.
func isLaterVersion(code string) bool {
	rest, ok := strings.CutPrefix(code, "valmin")
	if !ok {
		return false
	}
	digits, _, ok := strings.Cut(rest, ":")
	n, err := strconv.Atoi(digits)
	return ok && err == nil && n > 1
}

func decodePayload(payload string) (*Template, error) {
	lines := strings.Split(payload, "\n")
	if len(lines) < 2 || lines[len(lines)-1] != "" {
		return nil, errBadPayload
	}
	lines = lines[:len(lines)-1]
	t, err := decodeHeader(lines[0])
	if err != nil {
		return nil, err
	}
	i := 1
	for ; i < len(lines) && !strings.HasPrefix(lines[i], "#"); i++ {
		m, err := decodeMod(lines[i])
		if err != nil {
			return nil, fmt.Errorf("mod line %d: %w", i, err)
		}
		t.Mods = append(t.Mods, m)
	}
	for ; i < len(lines); i++ {
		if file, ok := strings.CutPrefix(lines[i], "#"); ok {
			t.Configs = append(t.Configs, control.ManifestConfig{File: file})
			continue
		}
		c := &t.Configs[len(t.Configs)-1]
		c.Content += lines[i] + "\n"
	}
	return t, nil
}

func encodeMod(m control.ManifestMod) (string, error) {
	for _, field := range []string{m.FullName, m.Version, m.Source} {
		if strings.ContainsAny(field, " \t\r\n") {
			return "", fmt.Errorf("mod %q has a field with whitespace", m.FullName)
		}
	}
	if m.FullName == "" || m.Version == "" || strings.HasPrefix(m.FullName, "#") {
		return "", fmt.Errorf("mod %q %q cannot be written", m.FullName, m.Version)
	}
	side, ok := sideCodes[m.Side]
	if !ok {
		return "", fmt.Errorf("mod %s has side %q", m.FullName, m.Side)
	}
	line := m.FullName + " " + m.Version
	switch {
	case m.Source != "" && m.Source != thunderstore:
		line += " " + side + " " + m.Source
	case side != "-":
		line += " " + side
	}
	return line, nil
}

func decodeMod(line string) (control.ManifestMod, error) {
	f := strings.Split(line, " ")
	if len(f) < 2 || len(f) > 4 || slices.Contains(f, "") {
		return control.ManifestMod{}, errBadPayload
	}
	m := control.ManifestMod{FullName: f[0], Version: f[1], Source: thunderstore}
	if len(f) > 2 {
		side, ok := sideNames[f[2]]
		if !ok {
			return control.ManifestMod{}, fmt.Errorf("unknown side %q", f[2])
		}
		m.Side = side
	}
	if len(f) > 3 {
		m.Source = f[3]
	}
	return m, nil
}

// sideNames maps each side letter back to its tag.
var sideNames = map[string]string{"s": "server_only", "r": "client_required", "o": "client_optional", "-": ""}

// headerKeys are the header's fields. Only the modifier "m" may repeat.
var headerKeys = map[string]bool{
	"n": true, "s": true, "w": true, "p": true, "x": true, "r": true, "m": true, "a": true,
	"mem": true, "cpu": true, "bc": true, "bh": true, "br": true,
}

func encodeHeader(t *Template) (string, error) {
	l := &t.Launch
	v := url.Values{}
	set := func(key, value string) {
		if value != "" {
			v.Set(key, value)
		}
	}
	flag := func(key string, on bool) {
		if on {
			v.Set(key, "1")
		}
	}
	number := func(key string, n int) {
		if n != 0 {
			v.Set(key, strconv.Itoa(n))
		}
	}
	set("n", t.Name)
	set("s", l.ServerName)
	set("w", l.WorldName)
	flag("p", l.Public)
	flag("x", l.Crossplay)
	set("r", l.Preset)
	keys := make([]string, 0, len(l.Modifiers))
	for k := range l.Modifiers {
		if strings.Contains(k, ":") {
			return "", fmt.Errorf("modifier %q contains a colon", k)
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	for _, k := range keys {
		v.Add("m", k+":"+l.Modifiers[k])
	}
	set("a", l.ExtraArgs)
	number("mem", l.MemLimitMB)
	if l.CPULimit != nil {
		v.Set("cpu", strconv.FormatFloat(*l.CPULimit, 'g', -1, 64))
	}
	number("bc", l.BackupKeepCold)
	number("bh", l.BackupKeepHot)
	flag("br", l.BackupOnRestart)
	return v.Encode(), nil
}

func decodeHeader(line string) (*Template, error) {
	v, err := url.ParseQuery(line)
	if err != nil {
		return nil, errBadPayload
	}
	if err := checkHeaderKeys(v); err != nil {
		return nil, err
	}
	h := headerReader{v: v}
	t := &Template{Name: v.Get("n")}
	t.Launch = control.ManifestLaunch{
		ServerName: v.Get("s"), WorldName: v.Get("w"), Preset: v.Get("r"), ExtraArgs: v.Get("a"),
		Public: h.flag("p"), Crossplay: h.flag("x"), BackupOnRestart: h.flag("br"),
		MemLimitMB: h.number("mem"), BackupKeepCold: h.number("bc"), BackupKeepHot: h.number("bh"),
		CPULimit: h.cpu(),
	}
	if h.err != nil {
		return nil, h.err
	}
	if t.Launch.Modifiers, err = decodeModifiers(v["m"]); err != nil {
		return nil, err
	}
	return t, nil
}

// headerReader reads typed header fields, keeping the first error.
type headerReader struct {
	v   url.Values
	err error
}

func (h *headerReader) fail(key, want string) {
	if h.err == nil {
		h.err = fmt.Errorf("%w: field %q is not %s", errBadPayload, key, want)
	}
}

func (h *headerReader) flag(key string) bool {
	switch h.v.Get(key) {
	case "":
		return false
	case "1":
		return true
	}
	h.fail(key, "1")
	return false
}

func (h *headerReader) number(key string) int {
	if !h.v.Has(key) {
		return 0
	}
	n, err := strconv.Atoi(h.v.Get(key))
	if err != nil {
		h.fail(key, "a number")
	}
	return n
}

func (h *headerReader) cpu() *float64 {
	if !h.v.Has("cpu") {
		return nil
	}
	cpu, err := strconv.ParseFloat(h.v.Get("cpu"), 64)
	if err != nil || math.IsNaN(cpu) || math.IsInf(cpu, 0) {
		h.fail("cpu", "a number")
		return nil
	}
	return &cpu
}

func checkHeaderKeys(v url.Values) error {
	for key, values := range v {
		if !headerKeys[key] {
			return fmt.Errorf("%w: unknown field %q", errBadPayload, key)
		}
		if key != "m" && len(values) != 1 {
			return fmt.Errorf("%w: field %q repeats", errBadPayload, key)
		}
	}
	return nil
}

func decodeModifiers(values []string) (map[string]string, error) {
	var out map[string]string
	for _, kv := range values {
		k, val, ok := strings.Cut(kv, ":")
		if !ok || k == "" {
			return nil, fmt.Errorf("%w: modifier %q", errBadPayload, kv)
		}
		if _, dup := out[k]; dup {
			return nil, fmt.Errorf("%w: modifier %q repeats", errBadPayload, k)
		}
		if out == nil {
			out = map[string]string{}
		}
		out[k] = val
	}
	return out, nil
}
