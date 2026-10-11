package config

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strings"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

// The copies the panel keeps beside a config file it writes.
const (
	// BackupSuffix names the copy of the bytes the last write replaced.
	BackupSuffix = ".bak"
	// OriginalSuffix names the copy of the file as it was before the panel's first write. It is
	// written once and then left alone; it is empty when the file did not exist yet.
	OriginalSuffix = ".orig"
)

// KeepCopies records the bytes a write to name is about to replace: always as its backup, and
// as its original when the panel has not written the file before.
func KeepCopies(root *os.Root, name string, current []byte) error {
	if err := fsutil.WriteFileAtomicIn(root, name+BackupSuffix, current); err != nil {
		return fmt.Errorf("back up %s: %w", name, err)
	}
	_, err := root.Lstat(name + OriginalSuffix)
	if errors.Is(err, fs.ErrNotExist) {
		if err := fsutil.WriteFileAtomicIn(root, name+OriginalSuffix, current); err != nil {
			return fmt.Errorf("keep the original of %s: %w", name, err)
		}
		return nil
	}
	if err != nil {
		return fmt.Errorf("inspect the original of %s: %w", name, err)
	}
	return nil
}

// Tweaks lists the settings of current that differ from orig as a partial config: section
// headers and `Key = Value` lines, in file order. A setting orig lacks counts as changed unless
// it holds the value its `# Default value:` comment declares. A changed setting whose name marks
// a credential is left out and counted in secrets.
func Tweaks(orig, current []byte) (out []byte, kept, secrets int) {
	was := Parse(orig)
	var b strings.Builder
	last := ""
	for _, s := range Parse(current).Settings() {
		if v, ok := was.Get(s.Section, s.Key); ok {
			if v == s.Value {
				continue
			}
		} else if def, ok := declaredDefault(s.Comments); ok && def == s.Value {
			continue
		}
		if secretKey(s.Key) {
			secrets++
			continue
		}
		writeSetting(&b, s, &last, kept == 0)
		kept++
	}
	return []byte(b.String()), kept, secrets
}

// Merge returns base with every setting of over applied. A setting base has takes the new
// value in place; the rest are added under their section headers, after base's content or, for
// a setting outside any section, before it. Everything else in base is kept.
func Merge(base, over []byte) []byte {
	doc := Parse(base)
	var head, tail strings.Builder
	last := ""
	for _, s := range Parse(over).Settings() {
		if _, ok := doc.Get(s.Section, s.Key); ok {
			// Cannot fail: the setting exists and a parsed value holds no newline.
			_ = doc.Set(s.Section, s.Key, s.Value)
			continue
		}
		if s.Section == "" && tail.Len() == 0 {
			writeSetting(&head, s, &last, true)
			continue
		}
		writeSetting(&tail, s, &last, tail.Len() == 0)
	}
	out := doc.Bytes()
	if tail.Len() > 0 && len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	return append(append([]byte(head.String()), out...), tail.String()...)
}

// writeSetting appends one setting, preceded by its section header when it starts a new
// section or is the first sectioned setting written.
func writeSetting(b *strings.Builder, s Setting, last *string, first bool) {
	if first && s.Section != "" || !first && s.Section != *last {
		b.WriteString("[")
		b.WriteString(s.Section)
		b.WriteString("]\n")
	}
	*last = s.Section
	b.WriteString(s.Key)
	b.WriteString(" = ")
	b.WriteString(s.Value)
	b.WriteString("\n")
}

// declaredDefault is the value a setting's `# Default value:` comment declares, and whether it
// has one.
func declaredDefault(comments []string) (string, bool) {
	for _, c := range comments {
		var def string
		if cut(c, "# Default value:", &def) {
			return def, true
		}
	}
	return "", false
}
