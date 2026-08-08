package notes

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
)

func TestAttachDoesNotChangeTarget(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	target, err := r.Objects.Put(object.NewBlob([]byte("the code")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	before, err := r.Objects.GetRaw(target)
	if err != nil {
		t.Fatalf("GetRaw: %v", err)
	}

	s := New(r)
	if _, err := s.Add("review", target, []byte("LGTM"), "alice", 1); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// The target object's bytes and identity are untouched.
	after, err := r.Objects.GetRaw(target)
	if err != nil {
		t.Fatalf("GetRaw after: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("attaching a note changed the target object's bytes")
	}
}

func TestNotesAccrete(t *testing.T) {
	r, _ := repo.Init(t.TempDir())
	target, _ := r.Objects.Put(object.NewBlob([]byte("x")))
	s := New(r)

	if _, err := s.Add("deploy", target, []byte("staging ok"), "ci", 1); err != nil {
		t.Fatalf("Add 1: %v", err)
	}
	if _, err := s.Add("deploy", target, []byte("prod ok"), "ci", 2); err != nil {
		t.Fatalf("Add 2: %v", err)
	}

	entries, err := s.List("deploy", target)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 2 {
		t.Fatalf("entries = %d, want 2", len(entries))
	}
	// Newest first: the chain head is the most recent note.
	if string(entries[0].Note.Payload) != "prod ok" || string(entries[1].Note.Payload) != "staging ok" {
		t.Fatalf("accretion order wrong: %q, %q", entries[0].Note.Payload, entries[1].Note.Payload)
	}
	// The older note is the newer note's parent.
	if !entries[0].Note.Parent.Equal(entries[1].ID) {
		t.Fatal("note chain not linked")
	}
}

func TestNamespaces(t *testing.T) {
	r, _ := repo.Init(t.TempDir())
	target, _ := r.Objects.Put(object.NewBlob([]byte("x")))
	s := New(r)
	_, _ = s.Add("review", target, []byte("a"), "u", 1)
	_, _ = s.Add("test-results", target, []byte("b"), "u", 1)

	ns, err := s.Namespaces(target)
	if err != nil {
		t.Fatalf("Namespaces: %v", err)
	}
	if len(ns) != 2 {
		t.Fatalf("namespaces = %v, want 2", ns)
	}
}

func TestListEmpty(t *testing.T) {
	r, _ := repo.Init(t.TempDir())
	target, _ := r.Objects.Put(object.NewBlob([]byte("x")))
	entries, err := New(r).List("none", target)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(entries) != 0 {
		t.Fatalf("entries = %v, want empty", entries)
	}
}
