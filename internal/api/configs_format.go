package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path"

	"gopkg.in/yaml.v3"

	apierr "github.com/valminhq/valmin/internal/api/errors"
)

// utf8BOM is the byte-order mark C# mods often write at the start of a text file.
var utf8BOM = []byte("\xef\xbb\xbf")

// checkTextFormat refuses a raw write whose content is not valid JSON or YAML for name's
// extension, as a 422 on the field "content".
func checkTextFormat(name string, next []byte) error {
	parse := textParser(name)
	if parse == nil {
		return nil
	}
	err := parse(next)
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
