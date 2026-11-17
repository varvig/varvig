package refs

import (
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/varvig/varvig/varvig/internal/multihash"
)

func newStore(t *testing.T) *Store {
	t.Helper()
	dir := t.TempDir()
	s, err := Open(
		filepath.Join(dir, "refs"),
		filepath.Join(dir, "logs"),
		filepath.Join(dir, "refs.lock"),
	)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	return s
}

func id(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	mh, err := multihash.Sum(multihash.BLAKE3, []byte(s))
	if err != nil {
		t.Fatalf("Sum: %v", err)
	}
	return mh
}

func TestCreateAndResolve(t *testing.T) {
	s := newStore(t)
	v := id(t, "a")
	if err := s.Create("refs/heads/main", v, "agent", "create"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Resolve("refs/heads/main")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !got.Equal(v) {
		t.Fatal("resolved value mismatch")
	}
}

func TestCreateRejectsExisting(t *testing.T) {
	s := newStore(t)
	if err := s.Create("refs/heads/main", id(t, "a"), "x", "c"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Create("refs/heads/main", id(t, "b"), "x", "c"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
}

func TestCASSuccessAndConflict(t *testing.T) {
	s := newStore(t)
	a, b, c := id(t, "a"), id(t, "b"), id(t, "c")
	if err := s.Create("refs/heads/main", a, "x", "c"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.CompareAndSwap("refs/heads/main", a, b, "x", "advance"); err != nil {
		t.Fatalf("CAS a->b: %v", err)
	}
	// Stale expectation (still a) must conflict.
	if err := s.CompareAndSwap("refs/heads/main", a, c, "x", "stale"); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}
	got, _ := s.Resolve("refs/heads/main")
	if !got.Equal(b) {
		t.Fatal("value changed despite conflict")
	}
}

func TestDelete(t *testing.T) {
	s := newStore(t)
	a := id(t, "a")
	if err := s.Create("refs/heads/tmp", a, "x", "c"); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.Delete("refs/heads/tmp", a, "x", "delete"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Resolve("refs/heads/tmp"); !errors.Is(err, ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
}

// TestConcurrentCASSerializes launches many goroutines all trying to advance
// the same ref from a->b. Exactly one must win; the rest must see a conflict.
// This is the atomic compare-and-swap primitive of design §1.4 / §2.
func TestConcurrentCASSerializes(t *testing.T) {
	s := newStore(t)
	a, b := id(t, "a"), id(t, "b")
	if err := s.Create("refs/heads/race", a, "x", "c"); err != nil {
		t.Fatalf("Create: %v", err)
	}

	const n = 32
	var wg sync.WaitGroup
	var mu sync.Mutex
	wins := 0
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func() {
			defer wg.Done()
			err := s.CompareAndSwap("refs/heads/race", a, b, "x", "advance")
			if err == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			} else if !errors.Is(err, ErrConflict) {
				t.Errorf("unexpected error: %v", err)
			}
		}()
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("winners = %d, want exactly 1", wins)
	}
}

func TestInvalidNames(t *testing.T) {
	s := newStore(t)
	v := id(t, "a")
	for _, bad := range []string{"", "/leading", "trailing/", "a//b", "../escape", "a/../b"} {
		if err := s.Create(bad, v, "x", "c"); !errors.Is(err, ErrInvalidName) {
			t.Errorf("name %q: err = %v, want ErrInvalidName", bad, err)
		}
	}
}

func TestList(t *testing.T) {
	s := newStore(t)
	_ = s.Create("refs/heads/main", id(t, "a"), "x", "c")
	_ = s.Create("refs/tags/v1", id(t, "b"), "x", "c")
	names, err := s.List()
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(names) != 2 {
		t.Fatalf("names = %v, want 2", names)
	}
}

// TestCASRefusesACorruptRefForEveryOldval pins the invariant that
// spec.PromoteSelection relies on: CompareAndSwap re-reads the ref under its
// lock and refuses a ref it cannot parse, whatever oldval the caller passed —
// including nil.
//
// spec.PromoteSelection resolves the ref first, and on any read error passes
// nil as oldval ("expect absent"). That is only safe because CompareAndSwap
// re-checks and refuses a corrupt ref itself. A comment in spec says so, but a
// comment cannot fail a build — deleting the corrupt-ref guard in
// CompareAndSwap turns this red, which is the point of pinning it here, in the
// package whoever deletes it is reading.
func TestCASRefusesACorruptRefForEveryOldval(t *testing.T) {
	newval := id(t, "new")

	cases := []struct {
		name   string
		oldval multihash.Multihash
	}{
		{"nil oldval (expect-absent / creation intent)", nil},
		{"a plausible oldval (swap intent)", id(t, "guess")},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newStore(t)

			// Bytes that are not a parseable multihash, at the exact path the
			// store reads for this ref — s.refPath, so the fixture cannot land
			// somewhere the store never looks and pass vacuously.
			path := s.refPath("refs/heads/corrupt")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("not-a-hash\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Guard the fixture: Resolve must see corrupt, not absent, or this
			// proves nothing.
			if _, err := s.Resolve("refs/heads/corrupt"); err == nil || errors.Is(err, ErrNotExist) {
				t.Fatalf("fixture does not model a corrupt ref; Resolve = %v", err)
			}

			err := s.CompareAndSwap("refs/heads/corrupt", tc.oldval, newval, "x", "msg")
			switch {
			case err == nil:
				t.Fatal("CompareAndSwap moved a corrupt ref")
			case errors.Is(err, ErrConflict):
				t.Fatalf("a corrupt ref was reported as a mere CAS conflict: %v", err)
			case errors.Is(err, ErrNotExist):
				t.Fatalf("a corrupt ref was treated as absent: %v", err)
			}
			// And nothing was written over it.
			if got, rerr := s.Resolve("refs/heads/corrupt"); rerr == nil && got.Equal(newval) {
				t.Fatal("CompareAndSwap wrote newval over a corrupt ref")
			}
		})
	}
}
