package ticket

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

func key(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return priv
}

// TestNewTicketIdentity: a new ticket gets a stable ref whose value is the
// genesis revision, and that revision is unmaterialized and signed.
func TestNewTicketIdentity(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	id, err := New(r, "add rate limiting", priv, "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	head, err := Head(r, id)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if !head.Equal(id) {
		t.Fatalf("a fresh ticket's head should equal its id: head=%s id=%s", head.Hex(), id.Hex())
	}
	obj, err := r.Objects.Get(head)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	c, err := obj.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if c.Materialized() {
		t.Fatal("a ticket revision must be unmaterialized (no tree)")
	}
	if c.Message != "add rate limiting" {
		t.Fatalf("spec = %q", c.Message)
	}
	if _, err := provenance.Verify(obj); err != nil {
		t.Fatalf("ticket revision is not validly signed: %v", err)
	}
}

// TestReviseAppendsRevisionAndMovesRef: revising appends a child revision,
// advances the ref, and keeps the ticket id stable.
func TestReviseAppendsRevisionAndMovesRef(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	id, err := New(r, "v1 spec", priv, "d", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	rev2, err := Revise(r, id, "v2 spec", priv, "d", 2)
	if err != nil {
		t.Fatalf("Revise: %v", err)
	}
	if rev2.Equal(id) {
		t.Fatal("revision produced the same hash as the genesis")
	}
	head, _ := Head(r, id)
	if !head.Equal(rev2) {
		t.Fatalf("head = %s, want the new revision %s", head.Hex(), rev2.Hex())
	}
	// The new revision's parent is the genesis; the ticket id is unchanged.
	obj, _ := r.Objects.Get(rev2)
	c, _ := obj.AsChange()
	if len(c.Parents) != 1 || !c.Parents[0].Equal(id) {
		t.Fatalf("revision parent = %v, want genesis %s", c.Parents, id.Hex())
	}
	info, err := Get(r, id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if info.Spec != "v2 spec" || !info.ID.Equal(id) || !info.Head.Equal(rev2) {
		t.Fatalf("info = %+v", info)
	}
}

// TestReviseIsReflogRecoverable: a bad revision is recoverable through the
// ticket ref's reflog — universal undo (§1.2).
func TestReviseIsReflogRecoverable(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	id, _ := New(r, "good spec", priv, "d", 1)
	if _, err := Revise(r, id, "bad reprioritization", priv, "d", 2); err != nil {
		t.Fatalf("Revise: %v", err)
	}
	log, err := r.Refs.ReadLog(Ref(id))
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(log) < 2 {
		t.Fatalf("reflog has %d entries, want >= 2 (create + revise)", len(log))
	}
	// The pre-revise value is the genesis, still recoverable from the log.
	last := log[len(log)-1]
	if last.Old == nil || !last.Old.Equal(id) {
		t.Fatalf("reflog does not record the recoverable prior head: %+v", last)
	}
}

// TestList enumerates tickets and skips non-ticket refs.
func TestList(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	a, _ := New(r, "ticket a", priv, "d", 1)
	b, _ := New(r, "ticket b", priv, "d", 2)
	_ = r.Refs.Create("refs/heads/main", a, "d", "branch") // a non-ticket ref

	list, err := List(r)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("List = %d tickets, want 2", len(list))
	}
	seen := map[string]bool{}
	for _, info := range list {
		seen[info.ID.Hex()] = true
	}
	if !seen[a.Hex()] || !seen[b.Hex()] {
		t.Fatalf("List missing a ticket: %+v", list)
	}
}
