package repo

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

func TestInitAndOpen(t *testing.T) {
	dir := t.TempDir()
	if _, err := Init(dir); err != nil {
		t.Fatalf("Init: %v", err)
	}
	// Re-init must refuse.
	if _, err := Init(dir); !errors.Is(err, ErrExists) {
		t.Fatalf("re-Init err = %v, want ErrExists", err)
	}
	// Open from a nested subdirectory must walk upward to find .loom.
	sub := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	r, err := Open(sub)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	if r.Root() != dir {
		t.Fatalf("root = %q, want %q", r.Root(), dir)
	}
	head, err := r.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if head != "refs/heads/main" {
		t.Fatalf("head = %q", head)
	}
}

func TestOpenNonRepo(t *testing.T) {
	if _, err := Open(t.TempDir()); !errors.Is(err, ErrNotRepo) {
		t.Fatalf("err = %v, want ErrNotRepo", err)
	}
}

// TestMerkleDAGThroughRepo builds blob -> tree -> change, stores each in the
// object store, points a ref at the change via CAS, and reads the whole graph
// back — exercising the entire step-1 substrate end to end.
func TestMerkleDAGThroughRepo(t *testing.T) {
	r, err := Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}

	// Two blobs.
	helloID, err := r.Objects.Put(object.NewBlob([]byte("hello\n")))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}
	readmeID, err := r.Objects.Put(object.NewBlob([]byte("# project\n")))
	if err != nil {
		t.Fatalf("put blob: %v", err)
	}

	// A tree referencing them (Merkle: tree id depends on blob ids).
	treeID, err := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "hello.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: helloID},
		{Name: "README.md", Mode: 0o100644, Kind: object.TypeBlob, ID: readmeID},
	}))
	if err != nil {
		t.Fatalf("put tree: %v", err)
	}

	// A root change pointing at the tree.
	changeID, err := r.Objects.Put(object.NewChange(object.Change{
		Tree:      treeID,
		Message:   "initial import",
		Timestamp: 1723100000,
		Author:    "agent-0",
	}))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}

	// Point the default branch at the change.
	if err := r.Refs.Create("refs/heads/main", changeID, "agent-0", "commit"); err != nil {
		t.Fatalf("create ref: %v", err)
	}

	// Now traverse the DAG back from the ref.
	head, err := r.Refs.Resolve("refs/heads/main")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	changeObj, err := r.Objects.Get(head)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	ch, err := changeObj.AsChange()
	if err != nil {
		t.Fatalf("as change: %v", err)
	}
	treeObj, err := r.Objects.Get(ch.Tree)
	if err != nil {
		t.Fatalf("get tree: %v", err)
	}
	entries, err := treeObj.TreeEntries()
	if err != nil {
		t.Fatalf("tree entries: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// README.md sorts before hello.txt.
	if entries[0].Name != "README.md" || entries[1].Name != "hello.txt" {
		t.Fatalf("entries out of order: %+v", entries)
	}
	blobObj, err := r.Objects.Get(entries[1].ID)
	if err != nil {
		t.Fatalf("get blob: %v", err)
	}
	content, _ := blobObj.BlobContent()
	if string(content) != "hello\n" {
		t.Fatalf("content = %q", content)
	}
}
