package worktree

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

func hashOf(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.SHA2_256, []byte(s))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func TestCompareAddModifyDelete(t *testing.T) {
	base := map[string]FileState{
		"keep.txt": {Hash: hashOf(t, "keep"), Mode: modeFile},
		"mod.txt":  {Hash: hashOf(t, "old"), Mode: modeFile},
		"gone.txt": {Hash: hashOf(t, "gone"), Mode: modeFile},
	}
	work := map[string]FileState{
		"keep.txt": {Hash: hashOf(t, "keep"), Mode: modeFile},
		"mod.txt":  {Hash: hashOf(t, "new"), Mode: modeFile},
		"new.txt":  {Hash: hashOf(t, "fresh"), Mode: modeFile},
	}
	d := Compare(base, work)
	if len(d.Added) != 1 || d.Added[0] != "new.txt" {
		t.Errorf("Added = %v", d.Added)
	}
	if len(d.Modified) != 1 || d.Modified[0] != "mod.txt" {
		t.Errorf("Modified = %v", d.Modified)
	}
	if len(d.Removed) != 1 || d.Removed[0] != "gone.txt" {
		t.Errorf("Removed = %v", d.Removed)
	}
}

func TestCompareRename(t *testing.T) {
	moved := hashOf(t, "moved-content")
	base := map[string]FileState{"old/name.txt": {Hash: moved, Mode: modeFile}}
	work := map[string]FileState{"new/name.txt": {Hash: moved, Mode: modeFile}}
	d := Compare(base, work)
	if len(d.Renamed) != 1 || d.Renamed[0].From != "old/name.txt" || d.Renamed[0].To != "new/name.txt" {
		t.Fatalf("Renamed = %+v", d.Renamed)
	}
	if len(d.Added) != 0 || len(d.Removed) != 0 {
		t.Errorf("a rename must not also show as add/remove: %+v", d)
	}
}

func TestCompareModeChange(t *testing.T) {
	same := hashOf(t, "same-content")
	base := map[string]FileState{"run.sh": {Hash: same, Mode: modeFile}}
	work := map[string]FileState{"run.sh": {Hash: same, Mode: modeExec}}
	d := Compare(base, work)
	if len(d.ModeChanged) != 1 || d.ModeChanged[0] != "run.sh" {
		t.Fatalf("ModeChanged = %v (a permission-only change is a mode change, not a content change)", d.ModeChanged)
	}
	if len(d.Modified) != 0 {
		t.Errorf("mode change misreported as content: %v", d.Modified)
	}
}

func TestScanPureCache(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	gitDir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "alpha\n", 0o644)
	mustWrite(t, filepath.Join(dir, "run.sh"), "#!/bin/sh\n", 0o755)
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "pkg", "b.go"), "package pkg\n", 0o644)

	idx := OpenIndex(gitDir)
	warm, err := Scan(s, dir, idx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if err := idx.Save(); err != nil {
		t.Fatal(err)
	}
	// Cold: delete the index and rescan; every result must be byte-identical.
	if err := os.Remove(filepath.Join(gitDir, indexFileName)); err != nil {
		t.Fatal(err)
	}
	cold, err := Scan(s, dir, OpenIndex(gitDir))
	if err != nil {
		t.Fatalf("cold Scan: %v", err)
	}
	if len(warm) != len(cold) {
		t.Fatalf("warm/cold size mismatch: %d vs %d", len(warm), len(cold))
	}
	for p, w := range warm {
		c, ok := cold[p]
		if !ok || !w.Hash.Equal(c.Hash) || w.Mode != c.Mode {
			t.Fatalf("path %q: warm %+v cold %+v — cache is not pure", p, w, c)
		}
	}
	// The exec bit is preserved as a mode, not lost.
	if warm["run.sh"].Mode != modeExec {
		t.Errorf("run.sh mode = %o, want exec", warm["run.sh"].Mode)
	}
}

func TestScanRacyEditDetected(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	gitDir := t.TempDir()
	file := filepath.Join(dir, "a.txt")
	mustWrite(t, file, "aaaa\n", 0o644)

	idx := OpenIndex(gitDir)
	before, _ := Scan(s, dir, idx)
	_ = idx.Save()

	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	m := info.ModTime()
	// Edit in place, same size, and restore the mtime so the stat tuple matches
	// the cache — the only thing that changed is content. This is the racy-clean
	// case; the index must still detect it because the entry's mtime is within the
	// same granularity as the index write.
	mustWrite(t, file, "bbbb\n", 0o644)
	if err := os.Chtimes(file, m, m); err != nil {
		t.Fatal(err)
	}
	idx.WrittenNs = m.UnixNano() // pin the racy window to cover the restored mtime

	after, err := Scan(s, dir, idx)
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if before["a.txt"].Hash.Equal(after["a.txt"].Hash) {
		t.Fatal("a racy in-place edit with identical size and mtime went undetected")
	}
}

func TestScanSymlinkEmptyBinary(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	gitDir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "empty.txt"), "", 0o644)
	mustWrite(t, filepath.Join(dir, "bin.dat"), "ab\x00cd", 0o644)
	if err := os.Symlink("empty.txt", filepath.Join(dir, "link")); err != nil {
		t.Skipf("symlinks unsupported: %v", err)
	}
	got, err := Scan(s, dir, OpenIndex(gitDir))
	if err != nil {
		t.Fatalf("Scan: %v", err)
	}
	if _, ok := got["empty.txt"]; !ok {
		t.Error("empty file missing from scan")
	}
	if _, ok := got["bin.dat"]; !ok {
		t.Error("binary file missing from scan")
	}
	if got["link"].Mode != modeSymlink {
		t.Errorf("link mode = %o, want symlink", got["link"].Mode)
	}
}

func TestFlattenStatesMatchesScan(t *testing.T) {
	s := newStore(t)
	dir := t.TempDir()
	gitDir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "a.txt"), "alpha\n", 0o644)
	if err := os.MkdirAll(filepath.Join(dir, "pkg"), 0o755); err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(dir, "pkg", "b.go"), "package pkg\n", 0o644)

	scanned, err := Scan(s, dir, OpenIndex(gitDir))
	if err != nil {
		t.Fatal(err)
	}
	root, err := WriteTree(s, dir)
	if err != nil {
		t.Fatal(err)
	}
	flat, err := FlattenStates(s, root)
	if err != nil {
		t.Fatal(err)
	}
	if len(flat) != len(scanned) {
		t.Fatalf("flatten %d paths, scan %d", len(flat), len(scanned))
	}
	for p, sc := range scanned {
		fl, ok := flat[p]
		if !ok || !fl.Hash.Equal(sc.Hash) || fl.Mode != sc.Mode {
			t.Errorf("path %q: scan %+v flatten %+v", p, sc, fl)
		}
	}
}
