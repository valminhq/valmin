package instance

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"unicode"

	"github.com/valminhq/valmin/internal/mods/fsutil"
)

const (
	// MaxConfigSize bounds a config file the panel edits, imports or carries in a template.
	// ConfigTree applies it to YAML, JSON and text files only, so it lists a `.cfg` of any size.
	MaxConfigSize = 1 << 20
	// maxConfigDepth bounds how many folders below the config directory a file may sit.
	maxConfigDepth = 8
	// maxConfigFiles bounds the files one walk of the config tree returns.
	maxConfigFiles = 5000
)

// configExts are the extensions of config files. A `.cfg` has settings; the rest are text.
var configExts = []string{".cfg", ".yaml", ".yml", ".json", ".txt"}

// CheckConfigPath accepts a slash-separated path relative to the config directory: no `.` or
// `..` element, no backslash or control character, no folder starting with a dot, at most maxConfigDepth folders,
// and an extension from configExts. Kept copies (`.bak`, `.orig`, `.pending`) fail the last.
func CheckConfigPath(name string) error {
	if !fs.ValidPath(name) || strings.Contains(name, `\`) || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%q is not a relative path", name)
	}
	parts := strings.Split(name, "/")
	if len(parts) > maxConfigDepth+1 {
		return fmt.Errorf("%q is more than %d folders deep", name, maxConfigDepth)
	}
	for _, folder := range parts[:len(parts)-1] {
		if strings.HasPrefix(folder, ".") {
			return fmt.Errorf("%q is in a hidden folder", name)
		}
	}
	if !slices.Contains(configExts, path.Ext(name)) {
		return fmt.Errorf("%q is not a config file", name)
	}
	return nil
}

// ConfigFile is one file of the config tree.
type ConfigFile struct {
	// Path is relative to the config directory, slash-separated.
	Path string
	Size int64
}

// ConfigListing is one walk of the config tree.
type ConfigListing struct {
	Files []ConfigFile
	// TooLarge are the YAML, JSON and text files left out for exceeding MaxConfigSize.
	TooLarge []string
	// Truncated reports that a limit left a file out, one in TooLarge included.
	Truncated bool
}

// ConfigTree lists the config files under root, the opened config directory: every regular
// file CheckConfigPath accepts, up to maxConfigFiles, leaving out a non-`.cfg` file larger than
// MaxConfigSize. Hidden folders are not entered, and a symlink is listed only when it leads to
// a regular file inside root.
func ConfigTree(root *os.Root) (ConfigListing, error) {
	walk := configWalk{root: root, listing: ConfigListing{Files: []ConfigFile{}}}
	// ponytail: visits every entry under the directory; bound the entries visited if a tree
	// turns out huge.
	if err := fs.WalkDir(root.FS(), ".", walk.visit); err != nil {
		return ConfigListing{}, fmt.Errorf("list config files: %w", err)
	}
	return walk.listing, nil
}

// configWalk collects the config tree during one fs.WalkDir over root.
type configWalk struct {
	root    *os.Root
	listing ConfigListing
}

// visit is the fs.WalkDirFunc of configWalk. An unreadable subdirectory is skipped, and so is a
// symlink unless it leads to a regular file inside the directory.
func (c *configWalk) visit(p string, d fs.DirEntry, err error) error {
	if err != nil {
		if p == "." {
			return err
		}
		return fs.SkipDir
	}
	link := d.Type()&fs.ModeSymlink != 0
	switch {
	case d.IsDir():
		return c.enter(p, d)
	case !d.Type().IsRegular() && !link, CheckConfigPath(p) != nil:
		return nil //nolint:nilerr // a name CheckConfigPath refuses is not listed
	case len(c.listing.Files) == maxConfigFiles:
		c.listing.Truncated = true
		return fs.SkipAll
	}
	f, info, err := fsutil.OpenRegularIn(c.root, p)
	switch {
	case errors.Is(err, fsutil.ErrNotRegular), err != nil && link:
		return nil // a link out of the directory, or to something other than a file
	case err != nil:
		return err
	}
	_ = f.Close()
	if !strings.HasSuffix(p, ".cfg") && info.Size() > MaxConfigSize {
		c.listing.Truncated = true
		c.listing.TooLarge = append(c.listing.TooLarge, p)
		return nil
	}
	c.listing.Files = append(c.listing.Files, ConfigFile{Path: p, Size: info.Size()})
	return nil
}

// enter decides whether the walk descends into the directory at p: not into a hidden one, nor
// one more than maxConfigDepth deep.
func (c *configWalk) enter(p string, d fs.DirEntry) error {
	if p == "." {
		return nil
	}
	if strings.HasPrefix(d.Name(), ".") {
		return fs.SkipDir
	}
	if strings.Count(p, "/") >= maxConfigDepth {
		c.listing.Truncated = true
		return fs.SkipDir
	}
	return nil
}
