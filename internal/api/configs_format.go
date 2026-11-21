package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"
	"strings"

	"gopkg.in/yaml.v3"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// utf8BOM is the byte-order mark C# mods often write at the start of a text file.
var utf8BOM = []byte("\xef\xbb\xbf")

// checkTextFormat refuses a raw write whose content is not valid JSON, YAML or `.cfg` for
// name's extension, as a 422 on the field "content".
func checkTextFormat(name string, current, next []byte) error {
	var err error
	if path.Ext(name) == ".cfg" {
		err = checkCfgLines(current, next)
	} else if parse := textParser(name); parse != nil {
		err = parse(next)
	}
	if err == nil {
		return nil
	}
	val := &apierr.Validation{}
	val.Add("content", apierr.FieldInvalid, err.Error())
	return val.Err()
}

// textParser is the syntax check for name's extension, or nil when it has none.
func textParser(name string) func([]byte) error {
	switch path.Ext(name) {
	case ".json":
		return parseJSON
	case ".yaml", ".yml":
		return parseYAML
	}
	return nil
}

// parseJSON checks raw is one JSON value, naming the line of a syntax error.
func parseJSON(raw []byte) error {
	raw = bytes.TrimPrefix(raw, utf8BOM)
	var v any
	err := json.Unmarshal(raw, &v)
	if syntax, ok := errors.AsType[*json.SyntaxError](err); ok {
		return fmt.Errorf("line %d: %w", bytes.Count(raw[:syntax.Offset], []byte("\n"))+1, err)
	}
	if err != nil {
		return fmt.Errorf("invalid JSON: %w", err)
	}
	return nil
}

// parseYAML checks every document in raw parses, naming the document that does not.
func parseYAML(raw []byte) error {
	dec := yaml.NewDecoder(bytes.NewReader(raw))
	for n := 1; ; n++ {
		var v any
		err := dec.Decode(&v)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("document %d: %w", n, err)
		}
	}
}

// checkCfgLines names the first line of next that is neither blank, a `# comment`, a closed
// `[section]` nor a `key = value` setting. A current file that already breaks those rules is in
// a mod's own dialect, and its edits are not checked.
func checkCfgLines(current, next []byte) error {
	if cfgSyntaxError(current) == nil {
		return cfgSyntaxError(next)
	}
	return nil
}

// cfgSyntaxError reports the first line of raw outside BepInEx's `.cfg` grammar, or nil.
func cfgSyntaxError(raw []byte) error {
	n := 0
	for line := range strings.Lines(string(bytes.TrimPrefix(raw, utf8BOM))) {
		n++
		body := strings.TrimSpace(line)
		switch {
		case body == "", strings.HasPrefix(body, "#"):
		case strings.HasPrefix(body, "["):
			if !strings.HasSuffix(body, "]") {
				return fmt.Errorf("line %d: the section header has no closing ]", n)
			}
		case !strings.Contains(body, "="):
			return fmt.Errorf("line %d: expected a setting (Key = value), a [Section] or a # comment", n)
		}
	}
	return nil
}

// matchLineEndings rewrites next with CRLF line endings when current used them, since a browser
// text box sends LF whatever the file held.
func matchLineEndings(current, next []byte) []byte {
	if !bytes.Contains(current, []byte("\r\n")) {
		return next
	}
	lf := bytes.ReplaceAll(next, []byte("\r\n"), []byte("\n"))
	return bytes.ReplaceAll(lf, []byte("\n"), []byte("\r\n"))
}
