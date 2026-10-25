package extract

import (
	"archive/zip"
	"fmt"
	"io"
	"strings"
)

// ReadRootText reads the entry called name at the root of the zip at zipPath, matching its case
// loosely, and returns at most limit bytes of it. It writes nothing to disk. found is false when
// the zip has no such entry; truncated says the entry was longer than limit.
func ReadRootText(zipPath, name string, limit int64) (text string, truncated, found bool, err error) {
	r, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", false, false, fmt.Errorf("open %s: %w", zipPath, err)
	}
	defer func() { _ = r.Close() }()

	for _, f := range r.File {
		if f.FileInfo().IsDir() || !strings.EqualFold(f.Name, name) {
			continue
		}
		data, err := readEntry(f, limit+1)
		if err != nil {
			return "", false, false, fmt.Errorf("read %s in %s: %w", f.Name, zipPath, err)
		}
		if int64(len(data)) > limit {
			return string(data[:limit]), true, true, nil
		}
		return string(data), false, true, nil
	}
	return "", false, false, nil
}

// readEntry reads at most n bytes of one zip entry.
func readEntry(f *zip.File, n int64) ([]byte, error) {
	rc, err := f.Open()
	if err != nil {
		return nil, fmt.Errorf("open: %w", err)
	}
	defer func() { _ = rc.Close() }()
	data, err := io.ReadAll(io.LimitReader(rc, n))
	if err != nil {
		return nil, fmt.Errorf("read: %w", err)
	}
	return data, nil
}
