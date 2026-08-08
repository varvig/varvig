package gitport

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// commitVarvig writes the working tree at r.Root() and records a change advancing
// refs/heads/main, returning the new change id.
func commitVarvig(t *testing.T, r *repo.Repo, msg, author string, ts int64) multihash.Multihash {
	t.Helper()
	treeID, err := worktree.WriteTree(r.Objects, r.Root())
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	var parents []multihash.Multihash
	prev, err := r.Refs.Resolve("refs/heads/main")
	if err == nil {
		parents = append(parents, prev)
	}
	change := object.NewChange(object.Change{
		Tree: treeID, Parents: parents, Message: msg, Timestamp: ts, Author: author,
	})
	id, err := r.Objects.Put(change)
	if err != nil {
		t.Fatalf("Put change: %v", err)
	}
	if err := r.Refs.CompareAndSwap("refs/heads/main", prev, id, author, "commit"); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	return id
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestVarvigRoundTripStableGitObjects proves that varvig -> git -> varvig -> git
// reproduces byte-identical git objects (and therefore identical git ids). It
// needs no external tooling.
func TestVarvigRoundTripStableGitObjects(t *testing.T) {
	srcDir := t.TempDir()
	r, err := repo.Init(srcDir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeFile(t, srcDir, "README.md", "# project\n")
	writeFile(t, srcDir, "src/main.go", "package main\n")
	commitVarvig(t, r, "initial import", "Agent <a@x>", 1723100000)
	writeFile(t, srcDir, "src/main.go", "package main\n\nfunc main() {}\n")
	head := commitVarvig(t, r, "add main", "Agent <a@x>", 1723100100)

	// First export.
	gitA := filepath.Join(t.TempDir(), ".git")
	oidA, err := Export(r, gitA, "main", head)
	if err != nil {
		t.Fatalf("Export A: %v", err)
	}

	// Import into a fresh varvig repo, then re-export.
	r2, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init r2: %v", err)
	}
	varvigHead, err := Import(r2, gitA, "main")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	gitB := filepath.Join(t.TempDir(), ".git")
	oidB, err := Export(r2, gitB, "main", varvigHead)
	if err != nil {
		t.Fatalf("Export B: %v", err)
	}

	if oidA != oidB {
		t.Fatalf("git commit id changed across round trip: %s != %s", oidA.Hex(), oidB.Hex())
	}
	assertSameObjects(t, gitA, gitB)
}

func assertSameObjects(t *testing.T, gitA, gitB string) {
	t.Helper()
	setA := objectSet(t, gitA)
	setB := objectSet(t, gitB)
	if len(setA) != len(setB) {
		t.Fatalf("object count differs: A=%d B=%d", len(setA), len(setB))
	}
	for k := range setA {
		if !setB[k] {
			t.Fatalf("object %s present in A but not B", k)
		}
	}
}

func objectSet(t *testing.T, gitDir string) map[string]bool {
	t.Helper()
	set := map[string]bool{}
	objRoot := filepath.Join(gitDir, "objects")
	err := filepath.Walk(objRoot, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		rel, _ := filepath.Rel(objRoot, path)
		set[filepath.ToSlash(rel)] = true
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	return set
}

// TestGitReadsExport proves the exported repository is valid, real git: it
// passes fsck and its tree content matches the varvig working tree.
func TestGitReadsExport(t *testing.T) {
	git := requireGit(t)

	srcDir := t.TempDir()
	r, err := repo.Init(srcDir)
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	writeFile(t, srcDir, "README.md", "# hello\n")
	writeFile(t, srcDir, "pkg/util.go", "package pkg\n")
	head := commitVarvig(t, r, "initial", "Agent <a@x>", 1723100000)

	exportDir := t.TempDir()
	gitDir := filepath.Join(exportDir, ".git")
	if _, err := Export(r, gitDir, "main", head); err != nil {
		t.Fatalf("Export: %v", err)
	}

	runGit(t, git, "", "--git-dir="+gitDir, "fsck", "--strict")

	out := runGit(t, git, "", "--git-dir="+gitDir, "log", "--format=%s")
	if strings.TrimSpace(out) != "initial" {
		t.Fatalf("git log subject = %q, want \"initial\"", strings.TrimSpace(out))
	}

	// Extract the tree with git and compare content to the varvig working tree.
	work := t.TempDir()
	runGit(t, git, "", "--git-dir="+gitDir, "--work-tree="+work, "checkout", "-f", "main")
	assertFileEqual(t, filepath.Join(work, "README.md"), "# hello\n")
	assertFileEqual(t, filepath.Join(work, "pkg", "util.go"), "package pkg\n")
}

// TestGitAuthoredRoundTrip is the strongest losslessness check: a commit
// authored by real git, imported into varvig and re-exported, must reproduce the
// original git commit SHA bit for bit — including committer identity, timezone,
// and first-parent order that varvig's native model does not carry.
func TestGitAuthoredRoundTrip(t *testing.T) {
	git := requireGit(t)

	gitRepo := t.TempDir()
	runGit(t, git, gitRepo, "init", "-q", "-b", "main", ".")
	writeFile(t, gitRepo, "a.txt", "first\n")
	runGit(t, git, gitRepo, "add", "-A")
	runGit(t, git, gitRepo, "commit", "-q", "-m", "first commit")
	writeFile(t, gitRepo, "b.txt", "second\n")
	runGit(t, git, gitRepo, "add", "-A")
	runGit(t, git, gitRepo, "commit", "-q", "-m", "second commit")

	originalHead := strings.TrimSpace(runGit(t, git, gitRepo, "rev-parse", "HEAD"))

	// Import into varvig, then re-export.
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	varvigHead, err := Import(r, filepath.Join(gitRepo, ".git"), "main")
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	exportGit := filepath.Join(t.TempDir(), ".git")
	oid, err := Export(r, exportGit, "main", varvigHead)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}

	if oid.Hex() != originalHead {
		t.Fatalf("re-exported HEAD = %s, want original %s", oid.Hex(), originalHead)
	}
	// And the round-tripped objects still satisfy git's own fsck.
	runGit(t, git, "", "--git-dir="+exportGit, "fsck", "--strict")
}

// TestImportPackedRepo proves packfile + packed-refs reading: a repo whose
// objects are packed (loose removed) and whose refs live in packed-refs imports
// and re-exports to the original commit SHA bit for bit.
func TestImportPackedRepo(t *testing.T) {
	git := requireGit(t)

	gitRepo := t.TempDir()
	runGit(t, git, gitRepo, "init", "-q", "-b", "main", ".")
	// An evolving, sizable file across commits encourages git to store deltas.
	big := strings.Repeat("line of content\n", 200)
	writeFile(t, gitRepo, "big.txt", big)
	writeFile(t, gitRepo, "a.txt", "first\n")
	runGit(t, git, gitRepo, "add", "-A")
	runGit(t, git, gitRepo, "commit", "-q", "-m", "first commit")
	writeFile(t, gitRepo, "big.txt", big+"one more line\n")
	writeFile(t, gitRepo, "b.txt", "second\n")
	runGit(t, git, gitRepo, "add", "-A")
	runGit(t, git, gitRepo, "commit", "-q", "-m", "second commit")

	originalHead := strings.TrimSpace(runGit(t, git, gitRepo, "rev-parse", "HEAD"))

	// Pack everything and move refs into packed-refs, deleting loose copies.
	runGit(t, git, gitRepo, "repack", "-ad")
	runGit(t, git, gitRepo, "pack-refs", "--all")

	// Sanity: there should now be at least one packfile.
	if packs, _ := filepath.Glob(filepath.Join(gitRepo, ".git", "objects", "pack", "*.pack")); len(packs) == 0 {
		t.Skip("git did not produce a packfile; nothing to test")
	}

	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	varvigHead, err := Import(r, filepath.Join(gitRepo, ".git"), "main")
	if err != nil {
		t.Fatalf("Import (packed): %v", err)
	}
	exportGit := filepath.Join(t.TempDir(), ".git")
	oid, err := Export(r, exportGit, "main", varvigHead)
	if err != nil {
		t.Fatalf("Export: %v", err)
	}
	if oid.Hex() != originalHead {
		t.Fatalf("re-exported HEAD = %s, want original %s", oid.Hex(), originalHead)
	}
	runGit(t, git, "", "--git-dir="+exportGit, "fsck", "--strict")
}

func requireGit(t *testing.T) string {
	t.Helper()
	path, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git not available")
	}
	return path
}

// runGit runs git deterministically: fixed identity and date, and isolated
// config so the host environment cannot influence object bytes.
func runGit(t *testing.T, git, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command(git, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=Test", "GIT_AUTHOR_EMAIL=test@example.com", "GIT_AUTHOR_DATE=1723100000 +0000",
		"GIT_COMMITTER_NAME=Test", "GIT_COMMITTER_EMAIL=test@example.com", "GIT_COMMITTER_DATE=1723100000 +0000",
		"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

func assertFileEqual(t *testing.T, path, want string) {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != want {
		t.Fatalf("%s = %q, want %q", path, b, want)
	}
}
