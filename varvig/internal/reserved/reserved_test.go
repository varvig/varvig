package reserved

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
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
