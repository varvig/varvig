package txn

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
)

// TestDeclaredOverlapsAndSerialization: overlapping declared write sets are
// reported and serialized; disjoint ones report nothing and run in parallel.
func TestDeclaredOverlapsAndSerialization(t *testing.T) {
	overlapping := []*Txn{
		{Name: "a", Writes: []string{"src/auth"}},
		{Name: "b", Writes: []string{"src/auth/login.go"}}, // covered by a
		{Name: "c", Writes: []string{"src/web"}},           // disjoint
	}
	ov := DeclaredOverlaps(overlapping)
	if len(ov) != 1 {
		t.Fatalf("declared overlaps = %+v, want exactly a×b", ov)
	}
	if ov[0].A != "a" || ov[0].B != "b" || !reflect.DeepEqual(ov[0].Paths, []string{"src/auth/login.go"}) {
		t.Fatalf("overlap = %+v, want a×b on the shared region src/auth/login.go", ov[0])
	}

	// Disjoint declared writes run in parallel; overlapping ones serialize. Prove
	// serialization by asserting the two conflicting txns are never in Apply at
	// once, while the disjoint one is unconstrained.
	r := newRepo(t)
	s := NewScheduler(r, mainRef)
	var inAuth int32
	mk := func(name, path string) *Txn {
		return &Txn{
			Name:   name,
			Writes: []string{path},
			Apply: func(ws *Workspace) error {
				if path == "shared.txt" {
					if atomic.AddInt32(&inAuth, 1) > 1 {
						t.Errorf("two conflicting txns entered Apply concurrently")
					}
					defer atomic.AddInt32(&inAuth, -1)
				}
				return ws.Write(path, []byte(name))
			},
		}
	}
	// a and b both write shared.txt (conflict); c writes elsewhere (disjoint).
	s.Run(context.Background(), []*Txn{mk("a", "shared.txt"), mk("b", "shared.txt"), mk("c", "other.txt")})
}

// TestObservedOverlapAndDrift: overlap detection uses declared sets before and
// observed sets after, and a declared/observed disagreement is reported.
func TestObservedOverlapAndDrift(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)

	// "wide" declares src/a.txt plus docs but writes only src/a.txt; "narrow"
	// declares and writes src/a.txt. They collide on the concrete path, and
	// "wide" over-declared: its docs prefix matched nothing it wrote.
	txns := []*Txn{
		{Name: "wide", Writes: []string{"src/a.txt", "docs"}, Apply: func(ws *Workspace) error {
			return ws.Write("src/a.txt", []byte("x"))
		}},
		{Name: "narrow", Writes: []string{"src/a.txt"}, Apply: func(ws *Workspace) error {
			return ws.Write("src/a.txt", []byte("y"))
		}},
	}

	// Before execution: declared sets overlap (src covers src/a.txt).
	if d := DeclaredOverlaps(txns); len(d) != 1 {
		t.Fatalf("declared overlaps = %+v, want wide×narrow", d)
	}

	results := s.Run(context.Background(), txns)

	// After execution: observed sets share the concrete path src/a.txt.
	obs := ObservedOverlaps(results)
	if len(obs) != 1 || !reflect.DeepEqual(obs[0].Paths, []string{"src/a.txt"}) {
		t.Fatalf("observed overlaps = %+v, want a collision on src/a.txt", obs)
	}

	// Disagreement is reported: wide declared "docs" but never wrote under it, so
	// that declared prefix is unused — reported as drift.
	drift := Reconcile(txns, results)
	var wideDrift *ScopeDrift
	for i := range drift {
		if drift[i].Name == "wide" {
			wideDrift = &drift[i]
		}
	}
	if wideDrift == nil {
		t.Fatal("wide over-declared its write set; the disagreement must be reported")
	}
	if !reflect.DeepEqual(wideDrift.DeclaredUnused, []string{"docs"}) {
		t.Fatalf("wide drift = %+v, want DeclaredUnused [docs]", *wideDrift)
	}
	if len(wideDrift.WroteUndeclared) != 0 {
		t.Errorf("wide wrote only within scope; nothing should be undeclared: %+v", wideDrift.WroteUndeclared)
	}
}

// TestDriftUndeclaredWrite: an observed path no declared prefix covers is
// reported as an undeclared write (the under-declaration direction).
func TestDriftUndeclaredWrite(t *testing.T) {
	d := Drift("t", []string{"src/auth"}, []string{"src/auth/ok.go", "src/web/sneak.go"})
	if !reflect.DeepEqual(d.WroteUndeclared, []string{"src/web/sneak.go"}) {
		t.Fatalf("undeclared = %+v, want [src/web/sneak.go]", d.WroteUndeclared)
	}
}

// TestOverlapExposesOnlyPaths is the isolation assertion (build spec P2.1): the
// exposed overlap type carries path strings and nothing that could smuggle
// content across a task boundary.
func TestOverlapExposesOnlyPaths(t *testing.T) {
	ty := reflect.TypeOf(Overlap{})
	want := map[string]reflect.Kind{"A": reflect.String, "B": reflect.String, "Paths": reflect.Slice}
	if ty.NumField() != len(want) {
		t.Fatalf("Overlap has %d fields; a content field must never be added", ty.NumField())
	}
	for i := 0; i < ty.NumField(); i++ {
		f := ty.Field(i)
		k, ok := want[f.Name]
		if !ok || f.Type.Kind() != k {
			t.Fatalf("Overlap.%s is %s; only path metadata may be exposed", f.Name, f.Type.Kind())
		}
		if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() != reflect.String {
			t.Fatalf("Overlap.%s must be a path-string slice, not %s", f.Name, f.Type.Elem().Kind())
		}
	}
}
