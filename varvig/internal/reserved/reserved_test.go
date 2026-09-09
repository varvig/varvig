package reserved

import (
	"strings"
	"testing"

	"github.com/varvig/varvig/varvig/internal/notes"
	"github.com/varvig/varvig/varvig/internal/object"
	"github.com/varvig/varvig/varvig/internal/repo"
)

func TestIsTicketRef(t *testing.T) {
	cases := map[string]bool{
		"refs/varvig/tickets/abc":      true,
		"refs/varvig/tickets/abc/spec": true,
		"refs/heads/main":              false,
		"refs/notes/varvig/attest/x":   false,
	}
	for name, want := range cases {
		if got := IsTicketRef(name); got != want {
			t.Errorf("IsTicketRef(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsReservedNoteNamespace(t *testing.T) {
	cases := map[string]bool{
		NoteAttest:          true,
		NoteExternal:        true,
		NoteScore:           true,
		"varvig/attest/sub": true,
		"review":            false,
		"varvig":            false,
		"varvig/other":      false,
	}
	for ns, want := range cases {
		if got := IsReservedNoteNamespace(ns); got != want {
			t.Errorf("IsReservedNoteNamespace(%q) = %v, want %v", ns, got, want)
		}
	}
}

// TestReservedNoteNamespacesAreUsable proves the reservation is real: the notes
// layer accepts the hierarchical governance namespaces and round-trips a note
// through them (decision D6 + the §1.3 namespace shapes).
func TestReservedNoteNamespacesAreUsable(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	target, err := r.Objects.Put(object.NewBlob([]byte("intent revision")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	s := notes.New(r)
	for _, ns := range NoteNamespaces() {
		if _, err := s.Add(ns, target, []byte("decision"), "director", 1); err != nil {
			t.Fatalf("Add(%q): %v", ns, err)
		}
		entries, err := s.List(ns, target)
		if err != nil {
			t.Fatalf("List(%q): %v", ns, err)
		}
		if len(entries) != 1 || string(entries[0].Note.Payload) != "decision" {
			t.Fatalf("List(%q) = %+v, want one 'decision' note", ns, entries)
		}
	}
}

// TestReservedNamespacesCopy guards against callers mutating the reservation.
func TestReservedNamespacesCopy(t *testing.T) {
	ns := NoteNamespaces()
	ns[0] = "tampered"
	if NoteNamespaces()[0] == "tampered" {
		t.Fatal("NoteNamespaces returned a shared, mutable slice")
	}
}

func TestIsFactoryRef(t *testing.T) {
	cases := map[string]bool{
		"refs/factory/cells/mini-a/capabilities":    true,
		"refs/factory/attempts/mini-a/abc/1":        true,
		"refs/factory/claims/mini-a/abc":            true,
		"refs/factory/envelopes/overseer-a":         true,
		"refs/factory/leases/mini-a/7063622d666162": true,
		"refs/factory/reservations/mini-a/deadbeef": true,
		"refs/heads/main":                           false,
		"refs/varvig/tickets/abc":                   false,
		"refs/pins/aabb/0000000000000000/1e20ff":    false,
		// The flat names Factory used before nesting. They are somebody else's
		// now, and must not be claimed by this predicate.
		"refs/attempts/mini-a/abc/1":  false,
		"refs/leases/mini-a/abc":      false,
		"refs/reservations/mini-a/ab": false,
		// A name that merely begins with the same letters is not nested under
		// the namespace: refs/factories/ is somebody else's.
		"refs/factories/other": false,
	}
	for name, want := range cases {
		if got := IsFactoryRef(name); got != want {
			t.Errorf("IsFactoryRef(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestIsPinRef(t *testing.T) {
	cases := map[string]bool{
		"refs/pins/aabb/0000000000000000/1e20ff": true,
		"refs/pins/":                             true,
		"refs/heads/main":                        false,
		"refs/factory/leases/mini-a/abc":         false,
		"refs/pinned/x":                          false,
	}
	for name, want := range cases {
		if got := IsPinRef(name); got != want {
			t.Errorf("IsPinRef(%q) = %v, want %v", name, got, want)
		}
	}
}

// TestEveryFactoryNamespaceNestsUnderOneRoot is the property the nesting exists
// for: one prefix classifies the whole layer, so a reader never has to know the
// list, and a Factory concern added later cannot land outside it by accident.
func TestEveryFactoryNamespaceNestsUnderOneRoot(t *testing.T) {
	for _, p := range FactoryPrefixes() {
		if !strings.HasPrefix(p, FactoryPrefix) {
			t.Errorf("factory namespace %q does not nest under %q", p, FactoryPrefix)
		}
		if !IsFactoryRef(p) {
			t.Errorf("IsFactoryRef(%q) is false for a reserved factory namespace", p)
		}
	}
}

// TestFactoryNamespacesAreDistinctFromTheCore proves the reservation does not
// overlap the core's own space. Factory is another layer, so its names sit
// beside refs/varvig/ rather than inside it — and a core ref must never be
// mistaken for a Factory one, or an audit asking "what is governance?" would
// get the wrong answer.
func TestFactoryNamespacesAreDistinctFromTheCore(t *testing.T) {
	for _, p := range FactoryPrefixes() {
		if strings.HasPrefix(p, "refs/varvig/") {
			t.Errorf("factory namespace %q is nested in the core's own space", p)
		}
		if !strings.HasPrefix(p, "refs/") || !strings.HasSuffix(p, "/") {
			t.Errorf("factory namespace %q is not a refs/ prefix ending in a slash", p)
		}
	}
	if IsFactoryRef(TicketsPrefix + "abc") {
		t.Error("a ticket ref was reported as a Factory ref")
	}
	if IsTicketRef(EnvelopesPrefix + "overseer-a") {
		t.Error("a Factory ref was reported as a ticket ref")
	}
	// Pins are the core's own, and stay top-level: GC and the p2p handlers act
	// on that name, so it cannot move under another layer's root.
	if IsFactoryRef(PinsPrefix + "aabb/0000000000000000/1e20ff") {
		t.Error("a pin ref was reported as a Factory ref")
	}
	if strings.HasPrefix(PinsPrefix, FactoryPrefix) {
		t.Error("the pin namespace was nested under the Factory root")
	}
}

// TestFactoryPrefixesAreACopy keeps the reservation immutable from outside, the
// same property NoteNamespaces has: a caller that mutates what it is handed must
// not be able to rewrite what the repository reserved.
func TestFactoryPrefixesAreACopy(t *testing.T) {
	got := FactoryPrefixes()
	if len(got) == 0 {
		t.Fatal("no factory prefixes reserved")
	}
	got[0] = "refs/tampered/"
	if FactoryPrefixes()[0] == "refs/tampered/" {
		t.Error("FactoryPrefixes returned the reservation itself, not a copy")
	}
}

// TestReplicationCatalogueCoversEveryLayerBuiltOnARef asserts the two namespaces
// whose *identity* is a ref replicate. A ticket ref that does not travel means
// each peer has a private queue; a lease ref that does not travel means a cell
// cannot see what it may spend.
func TestReplicationCatalogueCoversEveryLayerBuiltOnARef(t *testing.T) {
	for _, name := range []string{
		TicketsPrefix + "1e20aa",
		TicketsPrefix + "1e20aa/spec",
		FactoryPrefix + "anything",
		LeasesPrefix + "mini-a/deploy",
		EnvelopesPrefix + "overseer-1",
		ReservationsPrefix + "mini-a/key1",
		ClaimsPrefix + "mini-a/task-1",
		AttemptsPrefix + "mini-a/task-1/1",
		CellsPrefix + "mini-a/capabilities",
	} {
		if !IsReplicatedRef(name) {
			t.Errorf("%s must replicate between peers", name)
		}
	}
}

// TestReplicationExclusionsAreDeliberate pins the boundary. Each of these is
// excluded for its own reason — authority-bearing singleton, another peer's
// retention obligation, or a namespace with its own sync path — and widening the
// catalogue by accident is exactly what this test exists to catch.
func TestReplicationExclusionsAreDeliberate(t *testing.T) {
	for _, name := range []string{
		PolicyRef,
		PrincipalsRef,
		PinsPrefix + "aabb/0000000000000001/deadbeef",
		"refs/heads/main",
		"refs/remotes/origin/main",
		"refs/notes/varvig/attest/1e20aa",
		"refs/myteam/scratch",
	} {
		if IsReplicatedRef(name) {
			t.Errorf("%s is outside the replication catalogue and must not replicate by default", name)
		}
	}
}

// TestReplicatedPrefixesAreACopy: the reservation is not the caller's to edit.
func TestReplicatedPrefixesAreACopy(t *testing.T) {
	got := ReplicatedPrefixes()
	if len(got) == 0 {
		t.Fatal("the catalogue is empty")
	}
	got[0] = "refs/tampered/"
	if ReplicatedPrefixes()[0] == "refs/tampered/" {
		t.Error("mutating the returned slice changed the reservation")
	}
}

// TestEveryReplicatedPrefixIsReserved: replication is a property of reserved
// names. A prefix nobody reserved has no agreed spelling, so replicating it
// would be moving refs by pattern-guess.
func TestEveryReplicatedPrefixIsReserved(t *testing.T) {
	for _, p := range ReplicatedPrefixes() {
		if !IsTicketRef(p) && !IsFactoryRef(p) {
			t.Errorf("%s replicates but is not one of the reserved ref namespaces", p)
		}
	}
}
