package readapi

import (
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

type fixture struct {
	q       *Query
	repo    *repo.Repo
	blobID  multihash.Multihash
	treeID  multihash.Multihash
	rootID  multihash.Multihash // root change
	childID multihash.Multihash // change adding a second file
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	r, err := repo.Init(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(o *object.Object) multihash.Multihash {
		id, err := r.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	blobID := put(object.NewBlob([]byte("hello\n")))
	// A tree with a file and a subdirectory.
	subBlob := put(object.NewBlob([]byte("nested\n")))
	subTree := put(object.NewTree([]object.Entry{
		{Name: "inner.txt", Mode: 0o644, Kind: object.TypeBlob, ID: subBlob},
	}))
	tree := object.NewTree([]object.Entry{
		{Name: "file.txt", Mode: 0o644, Kind: object.TypeBlob, ID: blobID},
		{Name: "dir", Mode: 0o040000, Kind: object.TypeTree, ID: subTree},
	})
	treeID := put(tree)
	root := put(object.NewChange(object.Change{Tree: treeID, Message: "add file and dir", Author: "jan", Timestamp: 100}))

	// A child change that adds a second top-level file.
	blob2 := put(object.NewBlob([]byte("second\n")))
	tree2 := put(object.NewTree([]object.Entry{
		{Name: "file.txt", Mode: 0o644, Kind: object.TypeBlob, ID: blobID},
		{Name: "dir", Mode: 0o040000, Kind: object.TypeTree, ID: subTree},
		{Name: "new.txt", Mode: 0o644, Kind: object.TypeBlob, ID: blob2},
	}))
	child := put(object.NewChange(object.Change{
		Tree: tree2, Parents: []multihash.Multihash{root}, Message: "add new.txt", Author: "mira", Timestamp: 200,
	}))

	if err := r.Refs.Create("refs/heads/main", child, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	return &fixture{q: New(r), repo: r, blobID: blobID, treeID: treeID, rootID: root, childID: child}
}

func TestQueryObject(t *testing.T) {
	f := newFixture(t)
	info, err := f.q.Object(f.childID)
	if err != nil {
		t.Fatal(err)
	}
	if info.Type != "change" {
		t.Fatalf("type=%q", info.Type)
	}
	if info.Hash != f.childID.Hex() || info.Size == 0 {
		t.Fatalf("bad info %+v", info)
	}
	if len(info.Links) == 0 {
		t.Fatal("change should link to its tree and parent")
	}
}

func TestQueryTree(t *testing.T) {
	f := newFixture(t)
	// A change hash resolves to its tree.
	listing, err := f.q.Tree(f.childID, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(listing.Entries) != 3 {
		t.Fatalf("want 3 entries, got %d", len(listing.Entries))
	}
	// Descend into the subdirectory.
	sub, err := f.q.Tree(f.childID, "dir")
	if err != nil {
		t.Fatal(err)
	}
	if len(sub.Entries) != 1 || sub.Entries[0].Name != "inner.txt" {
		t.Fatalf("subtree listing wrong: %+v", sub.Entries)
	}
	// A missing path is ErrNotFound.
	if _, err := f.q.Tree(f.childID, "nope"); err == nil {
		t.Fatal("expected not-found for missing path")
	}
}

func TestQueryBlob(t *testing.T) {
	f := newFixture(t)
	content, err := f.q.Blob(f.blobID)
	if err != nil {
		t.Fatal(err)
	}
	if string(content) != "hello\n" {
		t.Fatalf("blob content=%q", content)
	}
	// A non-blob id is rejected.
	if _, err := f.q.Blob(f.treeID); err == nil {
		t.Fatal("expected error reading a tree as a blob")
	}
}

func TestQueryChangeIntentAndDiff(t *testing.T) {
	f := newFixture(t)
	view, err := f.q.Change(f.childID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Intent != "add new.txt" {
		t.Fatalf("intent=%q", view.Intent)
	}
	if len(view.ChangedAdd) != 1 || view.ChangedAdd[0] != "new.txt" {
		t.Fatalf("expected new.txt added, got %+v", view.ChangedAdd)
	}
	// The root change (no parent) reports every file as added.
	root, err := f.q.Change(f.rootID)
	if err != nil {
		t.Fatal(err)
	}
	if len(root.ChangedAdd) != 2 { // file.txt and dir/inner.txt
		t.Fatalf("root added=%+v", root.ChangedAdd)
	}
}

func TestQueryLog(t *testing.T) {
	f := newFixture(t)
	entries, err := f.q.Log(f.childID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("want 2 log entries, got %d", len(entries))
	}
	if entries[0].Hash != f.childID.Hex() {
		t.Fatal("log should start at the requested change")
	}
	// Limit is honored.
	limited, err := f.q.Log(f.childID, 1)
	if err != nil {
		t.Fatal(err)
	}
	if len(limited) != 1 {
		t.Fatalf("limit not honored: %d", len(limited))
	}
}

func TestQueryRefsAndResolve(t *testing.T) {
	f := newFixture(t)
	refs, err := f.q.Refs()
	if err != nil {
		t.Fatal(err)
	}
	if len(refs) != 1 || refs[0].Name != "refs/heads/main" {
		t.Fatalf("refs=%+v", refs)
	}
	// A branch shorthand resolves to the same hash.
	id, err := f.q.Resolve("main")
	if err != nil {
		t.Fatal(err)
	}
	if !id.Equal(f.childID) {
		t.Fatal("main should resolve to the child change")
	}
}
