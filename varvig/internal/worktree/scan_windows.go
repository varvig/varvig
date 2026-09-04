//go:build windows

package worktree

import "os"

// sysInode has no portable inode on Windows, so the staleness cache falls back to
// size + mtime alone. Returning zero simply skips the inode hint; correctness is
// unaffected because a proposal always rehashes and diffs against the base.
func sysInode(info os.FileInfo) uint64 { return 0 }
