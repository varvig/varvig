// Package worktree materializes Loom objects to a plain working tree on a real
// filesystem and back. A checkout is a real directory of real files a plain
// filesystem (and plain git, after export) can read — design §2's
// non-negotiable "plain working tree" property.
//
// File modes are stored in tree entries using git's mode vocabulary
// (100644/100755/120000/40000) so that Git export is a straight translation
// with no lossy remapping.
package worktree

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/store"
)

// Git-style mode values, stored as the numeric value of their octal form.
const (
	modeFile    = 0o100644
	modeExec    = 0o100755
	modeSymlink = 0o120000
	modeTree    = 0o40000
)

// skipName is the metadata directory that is never part of a tree.
const skipName = ".loom"

// WriteTree walks dir, stores every file as a blob and every subdirectory as a
// tree, and returns the identity of the root tree. Regular files, executable
// files, symlinks, and nested directories are supported; the .loom directory
// (and any .git directory) is skipped.
func WriteTree(s *store.Store, dir string) (multihash.Multihash, error) {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var entries []object.Entry
	for _, de := range ents {
		name := de.Name()
		if name == skipName || name == ".git" {
			continue
		}
		full := filepath.Join(dir, name)
		info, err := de.Info()
		if err != nil {
			return nil, err
		}
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			target, err := os.Readlink(full)
			if err != nil {
				return nil, err
			}
			id, err := s.Put(object.NewBlob([]byte(target)))
			if err != nil {
				return nil, err
			}
			entries = append(entries, object.Entry{Name: name, Mode: modeSymlink, Kind: object.TypeBlob, ID: id})
		case de.IsDir():
			id, err := WriteTree(s, full)
			if err != nil {
				return nil, err
			}
			entries = append(entries, object.Entry{Name: name, Mode: modeTree, Kind: object.TypeTree, ID: id})
		case info.Mode().IsRegular():
			content, err := os.ReadFile(full)
			if err != nil {
				return nil, err
			}
			id, err := s.Put(object.NewBlob(content))
			if err != nil {
				return nil, err
			}
			mode := modeFile
			if info.Mode()&0o111 != 0 {
				mode = modeExec
			}
			entries = append(entries, object.Entry{Name: name, Mode: uint32(mode), Kind: object.TypeBlob, ID: id})
		default:
			return nil, fmt.Errorf("worktree: unsupported file type for %q", full)
		}
	}
	return s.Put(object.NewTree(entries))
}

// Checkout materializes the tree identified by treeID into dir, creating
// directories, regular/executable files, and symlinks as recorded.
func Checkout(s *store.Store, treeID multihash.Multihash, dir string) error {
	obj, err := s.Get(treeID)
	if err != nil {
		return err
	}
	entries, err := obj.TreeEntries()
	if err != nil {
		return err
	}
	// Stable order keeps checkout deterministic and dirs created before files.
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	for _, e := range entries {
		full := filepath.Join(dir, e.Name)
		switch {
		case e.Kind == object.TypeTree:
			if err := Checkout(s, e.ID, full); err != nil {
				return err
			}
		case e.Mode == modeSymlink:
			blob, err := s.Get(e.ID)
			if err != nil {
				return err
			}
			target, _ := blob.BlobContent()
			_ = os.Remove(full)
			if err := os.Symlink(string(target), full); err != nil {
				return err
			}
		default:
			blob, err := s.Get(e.ID)
			if err != nil {
				return err
			}
			content, _ := blob.BlobContent()
			perm := os.FileMode(0o644)
			if e.Mode == modeExec {
				perm = 0o755
			}
			if err := os.WriteFile(full, content, perm); err != nil {
				return err
			}
		}
	}
	return nil
}
