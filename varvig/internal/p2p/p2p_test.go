package p2p

import (
	"net"
	"strings"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/store"
)

// seedServer builds a repo with two changes:
//
//	c1: {a.txt: "A"}
//	c2: {a.txt: "A", b.txt: "B"}  (parent c1)
//
// and points refs/heads/main at c2. It returns the repo and both change ids.
func seedServer(t *testing.T) (*repo.Repo, multihash.Multihash, multihash.Multihash) {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	put := func(o *object.Object) multihash.Multihash {
		id, err := r.Objects.Put(o)
		if err != nil {
			t.Fatalf("Put: %v", err)
		}
		return id
	}
	blobA := put(object.NewBlob([]byte("A")))
	tree1 := put(object.NewTree([]object.Entry{{Name: "a.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blobA}}))
	c1 := put(object.NewChange(object.Change{Tree: tree1, Message: "c1", Timestamp: 1}))
	if err := r.Refs.Create("refs/heads/main", c1, "seed", "c1"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	blobB := put(object.NewBlob([]byte("B")))
	tree2 := put(object.NewTree([]object.Entry{
		{Name: "a.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blobA},
		{Name: "b.txt", Mode: 0o100644, Kind: object.TypeBlob, ID: blobB},
	}))
	c2 := put(object.NewChange(object.Change{Tree: tree2, Parents: []multihash.Multihash{c1}, Message: "c2", Timestamp: 2}))
	if err := r.Refs.CompareAndSwap("refs/heads/main", c1, c2, "seed", "c2"); err != nil {
		t.Fatalf("CAS: %v", err)
	}
	return r, c1, c2
}

// countStore counts PutVerified calls so a test can observe how many objects a
// fetch actually transferred.
type countStore struct {
	*store.Store
	puts int
}

func (c *countStore) PutVerified(id multihash.Multihash, b []byte) error {
	c.puts++
	return c.Store.PutVerified(id, b)
}

// dialServe wires a client to a server over an in-memory pipe.
func dialServe(t *testing.T, server *repo.Repo) *Client {
	t.Helper()
	a, b := net.Pipe()
	t.Cleanup(func() { a.Close(); b.Close() })
	go func() { _ = Serve(server, b) }()
	client, err := Dial(a)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	return client
}

func TestFetchReplicatesClosure(t *testing.T) {
	server, _, c2 := seedServer(t)
	client := dialServe(t, server)

	dst, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init dst: %v", err)
	}
	if err := client.Fetch(dst.Objects, []multihash.Multihash{c2}, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !hasClosure(dst.Objects, c2) {
		t.Fatal("closure of c2 not fully replicated")
	}
	// Spot-check content came across intact.
	ch, err := dst.Objects.Get(c2)
	if err != nil {
		t.Fatalf("Get c2: %v", err)
	}
	change, _ := ch.AsChange()
	treeObj, _ := dst.Objects.Get(change.Tree)
	entries, _ := treeObj.TreeEntries()
	if len(entries) != 2 {
		t.Fatalf("tree entries = %d, want 2", len(entries))
	}
	blob, _ := dst.Objects.Get(entries[1].ID)
	content, _ := blob.BlobContent()
	if string(content) != "B" {
		t.Fatalf("b.txt content = %q", content)
	}
}

// TestFetchPrunesHaves proves the have-set prunes the transfer: fetching c2
// when the client already holds c1's closure moves strictly fewer objects than
// a cold fetch.
func TestFetchPrunesHaves(t *testing.T) {
	server, c1, c2 := seedServer(t)

	// Cold fetch of c2 into a fresh client: full closure.
	cold, _ := repo.Init(t.TempDir())
	coldCount := &countStore{Store: cold.Objects}
	client := dialServe(t, server)
	if err := client.Fetch(coldCount, []multihash.Multihash{c2}, nil); err != nil {
		t.Fatalf("cold Fetch: %v", err)
	}

	// Warm client: already has c1's closure; fetch c2 with have=[c1].
	warm, _ := repo.Init(t.TempDir())
	client2 := dialServe(t, server)
	if err := client2.Fetch(warm.Objects, []multihash.Multihash{c1}, nil); err != nil {
		t.Fatalf("warm preload Fetch: %v", err)
	}
	warmCount := &countStore{Store: warm.Objects}
	client3 := dialServe(t, server)
	if err := client3.Fetch(warmCount, []multihash.Multihash{c2}, []multihash.Multihash{c1}); err != nil {
		t.Fatalf("warm Fetch: %v", err)
	}

	if warmCount.puts == 0 {
		t.Fatal("warm fetch transferred nothing")
	}
	if warmCount.puts >= coldCount.puts {
		t.Fatalf("pruning did not reduce transfer: cold=%d warm=%d", coldCount.puts, warmCount.puts)
	}
	if !hasClosure(warm.Objects, c2) {
		t.Fatal("warm fetch did not complete c2 closure")
	}
}

// TestFetchReplicatesProvenance guards that a change's provenance object is part
// of its closure and therefore replicates during sync.
func TestFetchReplicatesProvenance(t *testing.T) {
	server, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	provID, err := server.Objects.Put(object.NewProvenance(object.Provenance{Authority: "alice", Model: "m"}))
	if err != nil {
		t.Fatalf("Put provenance: %v", err)
	}
	blob, _ := server.Objects.Put(object.NewBlob([]byte("x")))
	tree, _ := server.Objects.Put(object.NewTree([]object.Entry{{Name: "x", Mode: 0o100644, Kind: object.TypeBlob, ID: blob}}))
	change, err := server.Objects.Put(object.NewChange(object.Change{Tree: tree, Provenance: provID, Message: "signed", Timestamp: 1}))
	if err != nil {
		t.Fatalf("Put change: %v", err)
	}

	client := dialServe(t, server)
	dst, _ := repo.Init(t.TempDir())
	if err := client.Fetch(dst.Objects, []multihash.Multihash{change}, nil); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if !dst.Objects.Has(provID) {
		t.Fatal("provenance object was not replicated with the change")
	}
}

func TestPushWithCASLease(t *testing.T) {
	// Server starts empty; client seeds locally and pushes.
	server, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init server: %v", err)
	}
	source, c1, c2 := seedServer(t)

	// Push c1 first (server has no ref yet: old = nil).
	client := dialServe(t, server)
	if err := client.Push(source.Objects, "refs/heads/main", nil, c1); err != nil {
		t.Fatalf("push c1: %v", err)
	}
	if got, _ := server.Refs.Resolve("refs/heads/main"); !got.Equal(c1) {
		t.Fatal("server ref not advanced to c1")
	}

	// Advance to c2 with the correct lease (old = c1).
	client2 := dialServe(t, server)
	if err := client2.Push(source.Objects, "refs/heads/main", c1, c2); err != nil {
		t.Fatalf("push c2: %v", err)
	}
	if got, _ := server.Refs.Resolve("refs/heads/main"); !got.Equal(c2) {
		t.Fatal("server ref not advanced to c2")
	}
	if !hasClosure(server.Objects, c2) {
		t.Fatal("server missing c2 closure after push")
	}
}

func TestPushStaleLeaseRejected(t *testing.T) {
	server, _ := repo.Init(t.TempDir())
	source, c1, c2 := seedServer(t)

	client := dialServe(t, server)
	if err := client.Push(source.Objects, "refs/heads/main", nil, c1); err != nil {
		t.Fatalf("push c1: %v", err)
	}
	// Push c2 but claim the server is still empty (stale lease old=nil).
	client2 := dialServe(t, server)
	err := client2.Push(source.Objects, "refs/heads/main", nil, c2)
	if err == nil {
		t.Fatal("stale-lease push unexpectedly succeeded")
	}
	if !strings.Contains(err.Error(), "reject") && !strings.Contains(err.Error(), "conflict") {
		t.Fatalf("error = %v, want a lease rejection", err)
	}
	// The ref must be unchanged at c1.
	if got, _ := server.Refs.Resolve("refs/heads/main"); !got.Equal(c1) {
		t.Fatal("ref advanced despite rejected push")
	}
}

func TestObjectPayloadCodec(t *testing.T) {
	raw := []byte("some canonical object bytes")
	// Without the capability the payload is passed through untouched.
	if got := encodeObjPayload(map[string]bool{}, raw); string(got) != string(raw) {
		t.Fatal("raw path altered payload")
	}
	// With deflate it changes on the wire but decodes back exactly.
	enc := encodeObjPayload(map[string]bool{"deflate": true}, raw)
	dec, err := decodeObjPayload(map[string]bool{"deflate": true}, enc)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if string(dec) != string(raw) {
		t.Fatalf("deflate round-trip mismatch: %q", dec)
	}
}

func TestNegotiatedCapsIncludeDeflate(t *testing.T) {
	server, _, _ := seedServer(t)
	client := dialServe(t, server)
	if !client.Caps()[capDeflate()] {
		t.Fatal("expected deflate to be negotiated between two identical builds")
	}
}

// capDeflate indirection keeps the test independent of the constant's package.
func capDeflate() string { return "deflate" }
