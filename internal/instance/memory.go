package instance

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

const meminfoPath = "/proc/meminfo"

// AvailableMemoryMB is the memory the host reports as available to start a new process
// without swapping, from /proc/meminfo.
func AvailableMemoryMB() (int, error) {
	f, err := os.Open(meminfoPath)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", meminfoPath, err)
	}
	defer func() { _ = f.Close() }()
	return parseAvailableMemoryMB(f)
}

// parseAvailableMemoryMB reads the MemAvailable line of a /proc/meminfo listing, which is in kB.
func parseAvailableMemoryMB(r io.Reader) (int, error) {
	sc := bufio.NewScanner(r)
	for sc.Scan() {
		rest, ok := strings.CutPrefix(sc.Text(), "MemAvailable:")
		if !ok {
			continue
		}
		fields := strings.Fields(rest)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("unexpected MemAvailable line %q", sc.Text())
		}
		kb, err := strconv.Atoi(fields[0])
		if err != nil {
			return 0, fmt.Errorf("parse MemAvailable: %w", err)
		}
		return kb / 1024, nil
	}
	if err := sc.Err(); err != nil {
		return 0, fmt.Errorf("read meminfo: %w", err)
	}
	return 0, fmt.Errorf("meminfo has no MemAvailable line")
}
