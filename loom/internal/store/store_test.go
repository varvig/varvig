package store

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func TestPutGetRoundTrip(t *testing.T) {
	s := newStore(t)
	blob := object.NewBlob([]byte("content-addressed"))
	id, err := s.Put(blob)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if !s.Has(id) {
		t.Fatal("Has = false after Put")
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	content, _ := got.BlobContent()
	if string(content) != "content-addressed" {
		t.Fatalf("content = %q", content)
	}
}

func TestPutIsIdempotent(t *testing.T) {
	s := newStore(t)
	blob := object.NewBlob([]byte("same"))
	id1, err := s.Put(blob)
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	id2, err := s.Put(object.NewBlob([]byte("same")))
	if err != nil {
		t.Fatalf("Put again: %v", err)
	}
	if !id1.Equal(id2) {
		t.Fatal("identical content produced different ids")
	}
}

func TestGetMissing(t *testing.T) {
	s := newStore(t)
	id, _ := object.NewBlob([]byte("never-stored")).ID(s.algo)
	if _, err := s.Get(id); err != ErrNotFound {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestGetDetectsCorruption(t *testing.T) {
	s := newStore(t)
	id, err := s.Put(object.NewBlob([]byte("trust me")))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	// Corrupt the stored bytes on disk.
	dir, file, err := s.path(id)
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	full := filepath.Join(dir, file)
	if err := os.WriteFile(full, []byte("tampered content"), 0o644); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := s.GetRaw(id); err != ErrCorrupt {
		t.Fatalf("err = %v, want ErrCorrupt", err)
	}
}
