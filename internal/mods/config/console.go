// Package config makes the one BepInEx setting the panel depends on true, and nothing
// else. It imports neither store nor api and is a pure function over bytes plus one
// atomic write (CLAUDE.md §5).
//
// This is deliberately not a `.cfg` parser. ADR-010's comment-preserving AST is
// a later milestone's, specified in 03 §9; what this needs is 03 §5.5's single assertion —
// console logging on, or the panel never sees a chainloader line. A surgical edit that
// copies every other byte through cannot lose a comment, so B10 holds literally rather
// than by care.
//
// Specification: 03 §5.5, ADR-010, ADR-108.
package config

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// ErrConsoleKeyMissing is a BepInEx.cfg with no `Enabled` under `[Logging.Console]`.
//
// Not a benign absence: BepInEx's own default for that key is false, so a file
// without it produces a server that boots, loads its plugins, and says nothing the panel
// can read — 03 §5.2's failure shape exactly. The caller is told rather than left to infer
// silence, because this package will not invent a section it was not asked to write.
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

// enableConsole is the whole edit, as a pure function over the file's text so it can be
// tested and fuzzed without a filesystem. It walks lines, tracks the current section, and
// rewrites at most the value token of one line — every byte before the `=`, the spacing
// after it, anything trailing, and the line ending are all carried through untouched.
func enableConsole(text string) (out string, changed bool, err error) {
	var b strings.Builder
	b.Grow(len(text))

	section := ""
	found := false
	for line := range strings.Lines(text) {
		body := strings.TrimSpace(strings.TrimRight(line, "\r\n"))
		switch {
		case strings.HasPrefix(body, "[") && strings.HasSuffix(body, "]"):
			section = body[1 : len(body)-1]
		case section == consoleSection && !found && !strings.HasPrefix(body, "#"):
			// The section carries several keys and the surrounding comments mention this
			// one by name ("# Default value: false"), so a comment line is skipped rather
			// than matched, and only the first assignment counts.
			if rewritten, ok := enableLine(line); ok {
				found = true
				changed = changed || rewritten != line
				line = rewritten
			}
		}
		b.WriteString(line)
	}
	if !found {
		return text, false, ErrConsoleKeyMissing
	}
	return b.String(), changed, nil
}

// enableLine rewrites one `Enabled = false` assignment to `true`. ok reports whether the
// line is that key at all; a line already reading `true`, or holding a value this package
// does not recognise, comes back unchanged rather than being overwritten with a guess.
func enableLine(line string) (string, bool) {
	eq := strings.Index(line, "=")
	if eq < 0 || !strings.EqualFold(strings.TrimSpace(line[:eq]), consoleKey) {
		return line, false
	}
	rest := line[eq+1:]
	start := len(rest) - len(strings.TrimLeft(rest, " \t"))
	end := len(strings.TrimRight(rest, " \t\r\n"))
	if start > end || !strings.EqualFold(rest[start:end], "false") {
		return line, true
	}
	return line[:eq+1] + rest[:start] + "true" + rest[end:], true
}
