package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// ErrConsoleKeyMissing is a BepInEx.cfg with no `Enabled` under `[Logging.Console]`. Not a
// benign absence: the default is false, so the file yields a server that loads its plugins and
// tells the panel nothing (03 §5.2). The caller is told rather than left to infer silence.
var ErrConsoleKeyMissing = errors.New("modconfig: [Logging.Console] Enabled is not in this file")

const (
	consoleSection = "Logging.Console"
	consoleKey     = "Enabled"
)

// EnsureConsoleLogging makes `[Logging.Console] Enabled` read `true`, and touches nothing
// else. changed reports whether the file was rewritten: a file that already reads `true` —
// which is what the denikson pack ships — is left alone entirely, not rewritten to identical
// bytes.
func EnsureConsoleLogging(path string) (changed bool, err error) {
	raw, err := os.ReadFile(path) //nolint:gosec // path is built by the caller from an instance's own server directory
	if err != nil {
		return false, fmt.Errorf("read %s: %w", path, err)
	}
	out, changed, err := enableConsole(string(raw))
	if err != nil || !changed {
		return false, err
	}
	if err := fsutil.WriteFileAtomic(path, []byte(out)); err != nil {
		return false, fmt.Errorf("rewrite %s: %w", path, err)
	}
	return true, nil
}

// enableConsole is the whole edit, as a pure function over the file's text. A value the panel
// does not recognise is the operator's and comes back untouched.
func enableConsole(text string) (out string, changed bool, err error) {
	doc := Parse([]byte(text))
	value, ok := doc.Get(consoleSection, consoleKey)
	if !ok {
		return text, false, ErrConsoleKeyMissing
	}
	if !strings.EqualFold(value, "false") {
		return text, false, nil
	}
	if err := doc.Set(consoleSection, consoleKey, "true"); err != nil {
		return text, false, fmt.Errorf("enable console logging: %w", err)
	}
	return string(doc.Bytes()), true, nil
}
