//go:build !windows

package worktree

import (
	"os"
	"syscall"
)

// sysInode returns the file's inode number, used only as a cheap change hint in
// the working-tree staleness cache — a fast "did this path change?" probe before
// rehashing. Zero when the platform does not expose it via os.FileInfo.Sys.
func sysInode(info os.FileInfo) uint64 {
	if st, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(st.Ino)
	}
	return 0
}
