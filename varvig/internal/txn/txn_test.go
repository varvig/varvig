package txn

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

const mainRef = "refs/heads/main"

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

// readFile resolves the ref tip and reads a path from its tree.
func readFile(t *testing.T, r *repo.Repo, path string) (string, bool) {
	t.Helper()
	tip, err := r.Refs.Resolve(mainRef)
	if err != nil {
		return "", false
	}
	obj, err := r.Objects.Get(tip)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	c, _ := obj.AsChange()
	flat, err := flatten(r.Objects, c.Tree)
	if err != nil {
		t.Fatalf("flatten: %v", err)
	}
	fe, ok := flat[path]
	if !ok {
		return "", false
	}
	bl, _ := r.Objects.Get(fe.ID)
	content, _ := bl.BlobContent()
	return string(content), true
}

func historyLen(t *testing.T, r *repo.Repo) int {
	t.Helper()
	tip, err := r.Refs.Resolve(mainRef)
	if err != nil {
		return 0
	}
	n := 0
	for tip != nil {
		obj, err := r.Objects.Get(tip)
		if err != nil {
			t.Fatalf("get: %v", err)
		}
		c, _ := obj.AsChange()
		n++
		if len(c.Parents) == 0 {
			break
		}
		tip = c.Parents[0]
	}
	return n
}

// TestDisjointTransactionsAllCommit runs many transactions with disjoint write
// sets; all must commit and every file must be present, producing a linear
// history of exactly N changes.
func TestDisjointTransactionsAllCommit(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)

	const n = 8
	var txns []*Txn
	for i := 0; i < n; i++ {
		path := fmt.Sprintf("f%d.txt", i)
		txns = append(txns, &Txn{
			Name:   path,
			Writes: []string{path},
			Apply:  func(ws *Workspace) error { return ws.Write(path, []byte("v")) },
		})
	}
	for _, res := range s.Run(context.Background(), txns) {
		if res.Err != nil {
			t.Fatalf("txn %s failed: %v", res.Name, res.Err)
		}
	}
	for i := 0; i < n; i++ {
		if _, ok := readFile(t, r, fmt.Sprintf("f%d.txt", i)); !ok {
			t.Fatalf("f%d.txt missing", i)
		}
	}
	if got := historyLen(t, r); got != n {
		t.Fatalf("history length = %d, want %d", got, n)
	}
}

// TestConflictingTransactionsNoLostUpdate has many transactions read-modify-
// write the SAME file. Correct serialization means every contribution survives.
func TestConflictingTransactionsNoLostUpdate(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)

	const n = 6
	var txns []*Txn
	for i := 0; i < n; i++ {
		token := fmt.Sprintf("[%d]", i)
		txns = append(txns, &Txn{
			Name:   token,
			Reads:  []string{"shared.txt"},
			Writes: []string{"shared.txt"},
			Apply: func(ws *Workspace) error {
				cur, err := ws.Read("shared.txt")
				if err != nil && !errors.Is(err, ErrNotExist) {
					return err
				}
				return ws.Write("shared.txt", append(cur, token...))
			},
		})
	}
	for _, res := range s.Run(context.Background(), txns) {
		if res.Err != nil {
			t.Fatalf("txn %s failed: %v", res.Name, res.Err)
		}
	}
	final, ok := readFile(t, r, "shared.txt")
	if !ok {
		t.Fatal("shared.txt missing")
	}
	for i := 0; i < n; i++ {
		if !strings.Contains(final, fmt.Sprintf("[%d]", i)) {
			t.Fatalf("lost update: %q missing token [%d]", final, i)
		}
	}
}

// TestConflictingSerializeToOne asserts that conflicting transactions never run
// their Apply concurrently, while disjoint ones do.
func TestConflictingSerializeToOne(t *testing.T) {
	// Conflicting: overlapping writes -> max concurrency must be 1.
	t.Run("conflicting", func(t *testing.T) {
		r := newRepo(t)
		s := NewScheduler(r, mainRef)
		var active, max int32
		mkTxn := func(name string) *Txn {
			return &Txn{
				Name:   name,
				Writes: []string{"same.txt"},
				Apply: func(ws *Workspace) error {
					cur := atomic.AddInt32(&active, 1)
					for {
						old := atomic.LoadInt32(&max)
						if cur <= old || atomic.CompareAndSwapInt32(&max, old, cur) {
							break
						}
					}
					time.Sleep(30 * time.Millisecond)
					atomic.AddInt32(&active, -1)
					v, _ := ws.Read("same.txt")
					return ws.Write("same.txt", append(v, name...))
				},
			}
		}
		s.Run(context.Background(), []*Txn{mkTxn("a"), mkTxn("b"), mkTxn("c")})
		if max != 1 {
			t.Fatalf("conflicting max concurrency = %d, want 1", max)
		}
	})

	// Disjoint: distinct writes -> Apply runs concurrently. Each Apply holds
	// briefly so the first round overlaps; Apply may re-run on a lost CAS, but
	// the peak concurrency is already captured, so no barrier is used.
	t.Run("disjoint", func(t *testing.T) {
		r := newRepo(t)
		s := NewScheduler(r, mainRef)
		var active, max int32
		mkTxn := func(name string) *Txn {
			return &Txn{
				Name:   name,
				Writes: []string{name},
				Apply: func(ws *Workspace) error {
					cur := atomic.AddInt32(&active, 1)
					for {
						old := atomic.LoadInt32(&max)
						if cur <= old || atomic.CompareAndSwapInt32(&max, old, cur) {
							break
						}
					}
					time.Sleep(100 * time.Millisecond)
					atomic.AddInt32(&active, -1)
					return ws.Write(name, []byte("v"))
				},
			}
		}
		s.Run(context.Background(), []*Txn{mkTxn("a"), mkTxn("b"), mkTxn("c"), mkTxn("d")})
		if max < 2 {
			t.Fatalf("disjoint max concurrency = %d, want >= 2", max)
		}
	})
}

func TestCapabilityWriteOutsideScope(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)
	res := s.Run(context.Background(), []*Txn{{
		Name:   "rogue",
		Writes: []string{"allowed"},
		Apply:  func(ws *Workspace) error { return ws.Write("secret/keys.txt", []byte("stolen")) },
	}})
	if !errors.Is(res[0].Err, ErrOutOfScope) {
		t.Fatalf("err = %v, want ErrOutOfScope", res[0].Err)
	}
	if _, err := r.Refs.Resolve(mainRef); err == nil {
		t.Fatal("ref advanced despite capability violation")
	}
}

func TestCapabilityReadOutsideScope(t *testing.T) {
	r := newRepo(t)
	// Seed a file the transaction is not allowed to read.
	seed(t, r, "secret/keys.txt", "topsecret")

	s := NewScheduler(r, mainRef)
	res := s.Run(context.Background(), []*Txn{{
		Name:   "peeker",
		Reads:  []string{"public"},
		Writes: []string{"public/out.txt"},
		Apply: func(ws *Workspace) error {
			_, err := ws.Read("secret/keys.txt")
			return err
		},
	}})
	if !errors.Is(res[0].Err, ErrOutOfScope) {
		t.Fatalf("err = %v, want ErrOutOfScope", res[0].Err)
	}
}

func TestNoOpTransaction(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)
	res := s.Run(context.Background(), []*Txn{{
		Name:   "readonly",
		Reads:  []string{"x"},
		Writes: []string{"x"},
		Apply:  func(ws *Workspace) error { return nil },
	}})
	if res[0].Err != nil || !res[0].NoOp {
		t.Fatalf("result = %+v, want NoOp", res[0])
	}
	if _, err := r.Refs.Resolve(mainRef); err == nil {
		t.Fatal("ref advanced for a no-op transaction")
	}
}

// seed commits a single file directly so tests can establish a base state.
func seed(t *testing.T, r *repo.Repo, path, content string) {
	t.Helper()
	s := NewScheduler(r, mainRef)
	res := s.Run(context.Background(), []*Txn{{
		Name:   "seed",
		Writes: []string{path},
		Apply:  func(ws *Workspace) error { return ws.Write(path, []byte(content)) },
	}})
	if res[0].Err != nil {
		t.Fatalf("seed: %v", res[0].Err)
	}
}
