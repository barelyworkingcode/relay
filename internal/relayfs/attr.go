package relayfs

import (
	"os"
	"syscall"

	"github.com/hugelgupf/p9/p9"
)

// qidFromInfo builds a QID from an os.FileInfo. Path is the raw inode
// number: this Root is always a single directory tree on one filesystem
// (never a view that spans multiple devices the way localfs's general
// dev+ino packing scheme has to handle), so the inode alone is already a
// unique, stable identifier within it.
func qidFromInfo(fi os.FileInfo) p9.QID {
	st := fi.Sys().(*syscall.Stat_t)
	return p9.QID{
		Type: p9.ModeFromOS(fi.Mode()).QIDType(),
		Path: st.Ino,
	}
}

// attrFromInfo builds the full Attr set from an os.FileInfo's underlying
// syscall.Stat_t. Every field GetAttr might be asked for is filled in
// (server-side files are expected to answer everything; p9.Attr.WithMask
// trims it to what the caller actually asked for).
func attrFromInfo(fi os.FileInfo) p9.Attr {
	st := fi.Sys().(*syscall.Stat_t)
	return p9.Attr{
		Mode:             p9.ModeFromOS(fi.Mode()),
		UID:              p9.UID(st.Uid),
		GID:              p9.GID(st.Gid),
		NLink:            p9.NLink(st.Nlink),
		RDev:             p9.Dev(st.Rdev),
		Size:             uint64(st.Size),
		BlockSize:        uint64(st.Blksize),
		Blocks:           uint64(st.Blocks),
		ATimeSeconds:     uint64(st.Atimespec.Sec),
		ATimeNanoSeconds: uint64(st.Atimespec.Nsec),
		MTimeSeconds:     uint64(st.Mtimespec.Sec),
		MTimeNanoSeconds: uint64(st.Mtimespec.Nsec),
		CTimeSeconds:     uint64(st.Ctimespec.Sec),
		CTimeNanoSeconds: uint64(st.Ctimespec.Nsec),
	}
}
