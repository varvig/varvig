// Package repo wires the object store, refs, and reflog into a repository
// rooted at a .varvig directory. Layout:
//
//	.varvig/
//	  format          identifies the repository format (frozen; §4.1)
//	  objects/        content-addressed object store
//	  refs/           named pointers (refs/heads/..., etc.)
//	  logs/           append-only reflogs, mirroring refs/
//	  HEAD            symbolic ref naming the current branch
//	  refs.lock       ref mutation lock (transient)
package repo

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/store"
)

// Dir is the repository metadata directory name.
const Dir = ".varvig"

// formatMarker is written to .varvig/format. It pins the repository layout
// version; the object format itself is self-describing and frozen separately.
const formatMarker = "varvig repository format 1\n"

// ErrNotRepo is returned when no repository is found.
var ErrNotRepo = errors.New("repo: not a varvig repository (no .varvig directory found)")

// ErrExists is returned when initializing over an existing repository.
var ErrExists = errors.New("repo: repository already exists")

// Repo is an open repository.
type Repo struct {
	root    string
	Objects *store.Store
	Refs    *refs.Store
}

// Root returns the working-tree root (the parent of .varvig).
func (r *Repo) Root() string { return r.root }

// GitDir returns the path to the .varvig metadata directory.
func (r *Repo) GitDir() string { return filepath.Join(r.root, Dir) }

// Init creates a new repository at root.
func Init(root string) (*Repo, error) {
	gitDir := filepath.Join(root, Dir)
	if _, err := os.Stat(gitDir); err == nil {
		return nil, ErrExists
	}
	for _, d := range []string{
		gitDir,
		filepath.Join(gitDir, "objects"),
		filepath.Join(gitDir, "refs"),
		filepath.Join(gitDir, "logs"),
	} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	if err := os.WriteFile(filepath.Join(gitDir, "format"), []byte(formatMarker), 0o644); err != nil {
		return nil, err
	}
	// HEAD points at the default branch, which need not exist yet.
	if err := os.WriteFile(filepath.Join(gitDir, "HEAD"), []byte("ref: refs/heads/main\n"), 0o644); err != nil {
		return nil, err
	}
	return open(root)
}

// Open finds and opens the repository containing dir, walking upward until a
// .varvig directory is found.
func Open(dir string) (*Repo, error) {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	for {
		if fi, err := os.Stat(filepath.Join(abs, Dir)); err == nil && fi.IsDir() {
			return open(abs)
		}
		parent := filepath.Dir(abs)
		if parent == abs {
			return nil, ErrNotRepo
		}
		abs = parent
	}
}

func open(root string) (*Repo, error) {
	gitDir := filepath.Join(root, Dir)
	objs, err := store.Open(filepath.Join(gitDir, "objects"))
	if err != nil {
		return nil, err
	}
	rf, err := refs.Open(
		filepath.Join(gitDir, "refs"),
		filepath.Join(gitDir, "logs"),
		filepath.Join(gitDir, "refs.lock"),
	)
	if err != nil {
		return nil, err
	}
	return &Repo{root: root, Objects: objs, Refs: rf}, nil
}

// Head returns the ref name that HEAD currently points at (e.g.
// "refs/heads/main"). Detached HEAD (a direct object id) is not modeled yet.
func (r *Repo) Head() (string, error) {
	b, err := os.ReadFile(filepath.Join(r.GitDir(), "HEAD"))
	if err != nil {
		return "", err
	}
	s := strings.TrimSpace(string(b))
	if rest, ok := strings.CutPrefix(s, "ref:"); ok {
		return strings.TrimSpace(rest), nil
	}
	return "", fmt.Errorf("repo: unsupported HEAD form %q", s)
}
