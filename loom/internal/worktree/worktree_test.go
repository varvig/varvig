package worktree

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/dividebyzero/claude-experiments/loom/internal/store"
)

func newStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestWriteTreeCheckoutRoundTrip(t *testing.T) {
	src := t.TempDir()
	// A representative working tree: regular file, executable, nested dir.
	mustWrite(t, filepath.Join(src, "README.md"), "# hi\n", 0o644)
	mustWrite(t, filepath.Join(src, "run.sh"), "#!/bin/sh\necho hi\n", 0o755)
	if err := os.MkdirAll(filepath.Join(src, "pkg", "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, "pkg", "a.go"), "package pkg\n", 0o644)
	mustWrite(t, filepath.Join(src, "pkg", "sub", "b.txt"), "deep\n", 0o644)
	// A .loom directory must be ignored.
	if err := os.MkdirAll(filepath.Join(src, ".loom", "objects"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(src, ".loom", "HEAD"), "ref: x\n", 0o644)

	s := newStore(t)
	treeID, err := WriteTree(s, src)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}

	dst := t.TempDir()
	if err := Checkout(s, treeID, dst); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	// Re-hashing the checked-out tree must yield the same identity.
	rehashed, err := WriteTree(s, dst)
	if err != nil {
		t.Fatalf("WriteTree(dst): %v", err)
	}
	if !rehashed.Equal(treeID) {
		t.Fatal("checkout did not reproduce the tree identity")
	}

	// .loom must not have been captured.
	if _, err := os.Stat(filepath.Join(dst, ".loom")); !os.IsNotExist(err) {
		t.Fatal(".loom leaked into the tree")
	}
	// Executable bit must survive.
	fi, err := os.Stat(filepath.Join(dst, "run.sh"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&0o100 == 0 {
		t.Fatal("executable bit lost on checkout")
	}
	assertContent(t, filepath.Join(dst, "pkg", "sub", "b.txt"), "deep\n")
}

func TestWriteTreeSymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks require privilege on windows")
	}
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "target.txt"), "data\n", 0o644)
	if err := os.Symlink("target.txt", filepath.Join(src, "link.txt")); err != nil {
		t.Fatal(err)
	}
	s := newStore(t)
	treeID, err := WriteTree(s, src)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	dst := t.TempDir()
	if err := Checkout(s, treeID, dst); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	target, err := os.Readlink(filepath.Join(dst, "link.txt"))
	if err != nil {
		t.Fatalf("Readlink: %v", err)
	}
	if target != "target.txt" {
		t.Fatalf("symlink target = %q", target)
	}
}

func mustWrite(t *testing.T, path, content string, perm os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), perm); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("%s = %q, want %q", path, b, want)
	}
}
