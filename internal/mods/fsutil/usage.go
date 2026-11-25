package fsutil

import (
	"io/fs"
	"syscall"
	"time"
)

// Usage is what one file costs on disk: its allocated size, its device and inode, how many
// names link to it, and its times. Modified is the mtime, which a write and a chtimes move.
// Changed is the ctime, which also moves when a hard link to the inode is made or removed.
type Usage struct {
	SizeBytes int64
	Device    uint64
	Inode     uint64
	Links     uint64
	Modified  time.Time
	Changed   time.Time
}

// StatUsage reads info's Usage. A FileInfo without a Unix stat falls back to the apparent size,
// one link and the mtime for both times.
func StatUsage(info fs.FileInfo) Usage {
	st, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return Usage{SizeBytes: info.Size(), Links: 1, Modified: info.ModTime(), Changed: info.ModTime()}
	}
	return Usage{
		SizeBytes: st.Blocks * 512,
		Device:    uint64(st.Dev),
		Inode:     st.Ino,
		Links:     uint64(st.Nlink),
		Modified:  info.ModTime(),
		Changed:   time.Unix(st.Ctim.Sec, st.Ctim.Nsec),
	}
}

// FreedBytes is the space removing this name frees: all of it for the last link to the inode,
// nothing while another name still holds it.
func (u *Usage) FreedBytes() int64 {
	if u.Links > 1 {
		return 0
	}
	return u.SizeBytes
}
