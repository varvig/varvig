package main

import (
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// assertRepo builds a repo with one committed file, so endpoint resolution has
// a HEAD tree to look in.
func assertRepo(t *testing.T) (*repo.Repo, multihash.Multihash) {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob, err := r.Objects.Put(object.NewBlob([]byte("export const x = 1\n")))
	if err != nil {
		t.Fatal(err)
	}
	tree, err := r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "lib.js", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	if err != nil {
		t.Fatal(err)
	}
	ch, err := r.Objects.Put(object.NewChange(object.Change{Tree: tree, Message: "seed", Timestamp: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refs.Create("refs/heads/main", ch, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	return r, blob
}

// TestResolveEndpointNeverReturnsAPath: a path is accepted for convenience and
// resolved to the content it names, because a path is a resolution and not an
// identity (§3.1). An endpoint that stayed a path would be the rename bug.
func TestResolveEndpointNeverReturnsAPath(t *testing.T) {
	r, blob := assertRepo(t)

	n, err := resolveEndpoint(r, "lib.js")
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := n.(graphnode.ObjectNode)
	if !ok {
		t.Fatalf("a path resolved to %T, want an object node", n)
	}
	if !obj.ID().Equal(blob) {
		t.Errorf("resolved to %s, want the blob %s", obj.ID().Hex(), blob.Hex())
	}
	if strings.Contains(n.Key(), "lib.js") {
		t.Errorf("the node key carries the path: %s", n.Key())
	}
}

// TestResolveEndpointForeignRow: a "system:id" spelling is a foreign row, and
// the system tag stays opaque.
func TestResolveEndpointForeignRow(t *testing.T) {
	r, _ := assertRepo(t)
	n, err := resolveEndpoint(r, "somesystem:PROJ-123")
	if err != nil {
		t.Fatal(err)
	}
	ext, ok := n.(graphnode.ExternalNode)
	if !ok {
		t.Fatalf("a system:id spelling resolved to %T, want an external node", n)
	}
	if ext.System() != "somesystem" || ext.ForeignID() != "PROJ-123" {
		t.Errorf("resolved to %s:%s", ext.System(), ext.ForeignID())
	}
	// A foreign id containing further colons keeps them.
	n, err = resolveEndpoint(r, "ci:run/8891:step:3")
	if err != nil {
		t.Fatal(err)
	}
	if got := n.(graphnode.ExternalNode).ForeignID(); got != "run/8891:step:3" {
		t.Errorf("foreign id = %q, want run/8891:step:3", got)
	}
}

// TestResolveEndpointRefusesTheUnknown: an endpoint that names nothing is an
// error, not an invented node.
func TestResolveEndpointRefusesTheUnknown(t *testing.T) {
	r, _ := assertRepo(t)
	if n, err := resolveEndpoint(r, "no/such/path.js"); err == nil {
		t.Errorf("resolved a nonexistent path to %v", n)
	}
}

// TestResolveEndpointPrefersAHashOverAForeignRow: a hash contains no colon, but
// the check must not mistake a hash-shaped argument for a system tag.
func TestResolveEndpointPrefersAHashOverAForeignRow(t *testing.T) {
	r, blob := assertRepo(t)
	n, err := resolveEndpoint(r, blob.Hex())
	if err != nil {
		t.Fatal(err)
	}
	obj, ok := n.(graphnode.ObjectNode)
	if !ok {
		t.Fatalf("a hash resolved to %T, want an object node", n)
	}
	if !obj.ID().Equal(blob) {
		t.Error("a hash endpoint did not resolve to itself")
	}
}
