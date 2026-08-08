package object

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

func TestNoteRoundTripAndLinks(t *testing.T) {
	target, _ := multihash.Sum(multihash.BLAKE3, []byte("target"))
	parent, _ := multihash.Sum(multihash.BLAKE3, []byte("parent-note"))
	obj := NewNote(Note{
		Target:    target,
		Namespace: "test-results",
		Payload:   []byte("passed 42/42"),
		Parent:    parent,
		Timestamp: 99,
		Author:    "ci",
	})
	got, err := Decode(obj.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	n, err := got.AsNote()
	if err != nil {
		t.Fatalf("AsNote: %v", err)
	}
	if !n.Target.Equal(target) || n.Namespace != "test-results" || string(n.Payload) != "passed 42/42" {
		t.Fatalf("note fields wrong: %+v", n)
	}
	if !n.Parent.Equal(parent) || n.Author != "ci" || n.Timestamp != 99 {
		t.Fatalf("note fields wrong: %+v", n)
	}
	links, _ := got.Links()
	if len(links) != 2 {
		t.Fatalf("links = %d, want target+parent", len(links))
	}
}

func TestNoteWithoutParent(t *testing.T) {
	target, _ := multihash.Sum(multihash.BLAKE3, []byte("t"))
	obj := NewNote(Note{Target: target, Namespace: "x", Payload: []byte("y"), Timestamp: 1})
	if _, ok := obj.Field(tagNoteParent); ok {
		t.Fatal("parent field emitted when unset")
	}
	links, _ := obj.Links()
	if len(links) != 1 {
		t.Fatalf("links = %d, want 1 (target only)", len(links))
	}
}

func TestHookConfigRoundTripAndLinks(t *testing.T) {
	m1, _ := multihash.Sum(multihash.BLAKE3, []byte("mod1"))
	m2, _ := multihash.Sum(multihash.BLAKE3, []byte("mod2"))
	obj := NewHookConfig(HookConfig{Entries: []HookEntry{
		{Event: "pre-commit", Module: m1},
		{Event: "post-commit", Module: m2},
	}})
	got, err := Decode(obj.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	cfg, err := got.AsHookConfig()
	if err != nil {
		t.Fatalf("AsHookConfig: %v", err)
	}
	if len(cfg.Entries) != 2 || cfg.Entries[0].Event != "pre-commit" || !cfg.Entries[1].Module.Equal(m2) {
		t.Fatalf("entries wrong: %+v", cfg.Entries)
	}
	links, _ := got.Links()
	if len(links) != 2 {
		t.Fatalf("links = %d, want 2 modules", len(links))
	}
}
