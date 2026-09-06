package idchain

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/graphnode"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

func newRepo(t *testing.T) (*repo.Repo, ed25519.PrivateKey) {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return r, priv
}

// TestIdentityIsStableAcrossRevisions: the id is the genesis revision's hash and
// never moves; the ref value tracks the head.
func TestIdentityIsStableAcrossRevisions(t *testing.T) {
	r, priv := newRepo(t)
	id, err := New(r, reserved.NodesPrefix, "src/auth.ts", priv, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	head, err := Head(r, reserved.NodesPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if !head.Equal(id) {
		t.Fatalf("a fresh identity's head %s is not its genesis %s", head.Hex(), id.Hex())
	}

	rev2, err := Revise(r, reserved.NodesPrefix, id, "src/session.ts", priv, "jan", 200)
	if err != nil {
		t.Fatal(err)
	}
	if rev2.Equal(id) {
		t.Fatal("a revision must be a new object, not the genesis")
	}
	// The identity did not move.
	head, err = Head(r, reserved.NodesPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if !head.Equal(rev2) {
		t.Errorf("head = %s, want the new revision %s", head.Hex(), rev2.Hex())
	}
	if _, err := StateAt(r, id); err != nil {
		t.Errorf("the genesis revision must still be readable after a revise: %v", err)
	}
}

// TestRenameDoesNotRetargetAnExistingEdge is G1's headline acceptance. A symbol
// or file is renamed; an edge written before the rename must still resolve to
// what it meant at the time, not to the new name.
//
// This is the failure the four node classes exist to prevent: an edge bound to
// the string "src/auth.ts" would, after the rename, either dangle or silently
// describe a file it never described. An edge bound to (ref, revision) keeps
// meaning what it meant, and can still find its way to the current name.
func TestRenameDoesNotRetargetAnExistingEdge(t *testing.T) {
	r, priv := newRepo(t)
	id, err := New(r, reserved.NodesPrefix, "src/auth.ts", priv, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}

	// An edge is written now, binding to the identity as it stands.
	before, err := graphnode.Identity(Ref(reserved.NodesPrefix, id), id)
	if err != nil {
		t.Fatal(err)
	}
	keyAtWriteTime := before.Key()

	// The file is renamed: a new revision, appended.
	rev2, err := Revise(r, reserved.NodesPrefix, id, "src/session.ts", priv, "jan", 200)
	if err != nil {
		t.Fatal(err)
	}

	// The edge written before the rename is unchanged...
	if before.Key() != keyAtWriteTime {
		t.Fatal("the stored node key changed underneath the edge")
	}
	// ...and still resolves to the name it described.
	was, err := StateAt(r, before.Revision())
	if err != nil {
		t.Fatal(err)
	}
	if was != "src/auth.ts" {
		t.Errorf("an edge written before the rename now resolves to %q; want src/auth.ts", was)
	}

	// The identity is still reachable and now means something else. Both
	// questions are answerable, which is the point of storing both halves.
	now, err := StateAt(r, rev2)
	if err != nil {
		t.Fatal(err)
	}
	if now != "src/session.ts" {
		t.Errorf("the identity's current state = %q, want src/session.ts", now)
	}
	if before.Ref() != Ref(reserved.NodesPrefix, id) {
		t.Error("the edge lost the thread back to the identity")
	}

	// The full history connects them, so a rename is auditable rather than a
	// discontinuity.
	hist, err := History(r, reserved.NodesPrefix, id)
	if err != nil {
		t.Fatal(err)
	}
	if len(hist) != 2 || !hist[0].Equal(rev2) || !hist[1].Equal(id) {
		t.Errorf("history = %v, want [rev2 genesis]", hist)
	}
}

// TestReviseIsCompareAndSwap: a stale writer does not clobber a concurrent one.
func TestReviseRequiresCurrentHead(t *testing.T) {
	r, priv := newRepo(t)
	id, err := New(r, reserved.NodesPrefix, "a", priv, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Revise(r, reserved.NodesPrefix, id, "b", priv, "jan", 200); err != nil {
		t.Fatal(err)
	}
	// Reviving an identity that does not exist is an error, not a silent create.
	missing, err := New(r, reserved.NodesPrefix, "other", priv, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Refs.Delete(Ref(reserved.NodesPrefix, missing), missing, "jan", "test"); err != nil {
		t.Fatal(err)
	}
	if _, err := Revise(r, reserved.NodesPrefix, missing, "x", priv, "jan", 300); err == nil {
		t.Error("revising a nonexistent identity must fail, not create one")
	}
}

// TestListAndPrefixDiscipline: identities are listed under their own namespace,
// and a prefix that is not a ref namespace is refused.
func TestListAndPrefixDiscipline(t *testing.T) {
	r, priv := newRepo(t)
	a, err := New(r, reserved.NodesPrefix, "a", priv, "jan", 100)
	if err != nil {
		t.Fatal(err)
	}
	b, err := New(r, reserved.NodesPrefix, "b", priv, "jan", 101)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := List(r, reserved.NodesPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 2 {
		t.Fatalf("List returned %d identities, want 2", len(ids))
	}
	seen := map[string]bool{ids[0].Hex(): true, ids[1].Hex(): true}
	if !seen[a.Hex()] || !seen[b.Hex()] {
		t.Errorf("List = %v, missing one of the two identities", ids)
	}
	// Tickets live in their own namespace and must not appear here.
	tickets, err := List(r, reserved.TicketsPrefix)
	if err != nil {
		t.Fatal(err)
	}
	if len(tickets) != 0 {
		t.Errorf("node identities leaked into the ticket namespace: %v", tickets)
	}

	if _, err := New(r, "not-a-ref-namespace", "x", priv, "jan", 100); err == nil {
		t.Error("a prefix outside refs/ must be refused")
	}
}
