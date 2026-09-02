package ticket

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// implCommit puts a change that fulfills `fulfills` and advances refs/heads/main
// onto it (parent = the current main head), returning its id.
func implCommit(t *testing.T, r *repo.Repo, fulfills multihash.Multihash, arts ...multihash.Multihash) multihash.Multihash {
	t.Helper()
	prev, _ := r.Refs.Resolve("refs/heads/main")
	ch := object.Change{Message: "impl", Timestamp: 9, Author: "eng", Fulfills: fulfills, Artifacts: arts}
	if prev != nil {
		ch.Parents = []multihash.Multihash{prev}
	}
	id, err := r.Objects.Put(object.NewChange(ch))
	if err != nil {
		t.Fatalf("put commit: %v", err)
	}
	if err := r.Refs.CompareAndSwap("refs/heads/main", prev, id, "eng", "commit"); err != nil {
		t.Fatalf("advance main: %v", err)
	}
	return id
}

func TestRevisionsChain(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	id, _ := New(r, "spec v1", priv, "dir", 1)
	rev2, _ := Revise(r, id, "spec v2", priv, "dir", 2)

	revs, err := Revisions(r, id)
	if err != nil {
		t.Fatalf("Revisions: %v", err)
	}
	if len(revs) != 2 || !revs[0].Equal(rev2) || !revs[1].Equal(id) {
		t.Fatalf("Revisions = %v, want [rev2, genesis]", revs)
	}
}

func TestImplementationOpenStaleImplemented(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	id, _ := New(r, "spec v1", priv, "dir", 1)

	// No commits yet -> open.
	if s, _, _ := Implementation(r, id); s != ImplOpen {
		t.Fatalf("fresh ticket impl = %q, want open", s)
	}

	// A commit fulfilling the head revision -> implemented.
	c1 := implCommit(t, r, id)
	s, commits, _ := Implementation(r, id)
	if s != ImplImplemented || len(commits) != 1 || !commits[0].Equal(c1) {
		t.Fatalf("after impl commit: state=%q commits=%v", s, commits)
	}

	// Revise the spec: the existing commit now implements a superseded revision.
	rev2, _ := Revise(r, id, "spec v2", priv, "dir", 2)
	if s, _, _ := Implementation(r, id); s != ImplStale {
		t.Fatalf("after revise impl = %q, want stale", s)
	}

	// A commit fulfilling the new head -> implemented again.
	implCommit(t, r, rev2)
	if s, _, _ := Implementation(r, id); s != ImplImplemented {
		t.Fatalf("after re-impl = %q, want implemented", s)
	}
}

// A commit that fulfills a *different* ticket must not count.
func TestImplementationIgnoresOtherTickets(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	a, _ := New(r, "ticket A", priv, "dir", 1)
	b, _ := New(r, "ticket B", priv, "dir", 2)
	implCommit(t, r, b) // implements B, on main

	if s, _, _ := Implementation(r, a); s != ImplOpen {
		t.Fatalf("ticket A impl = %q, want open (B's commit must not count)", s)
	}
	if s, _, _ := Implementation(r, b); s != ImplImplemented {
		t.Fatalf("ticket B impl = %q, want implemented", s)
	}
}

// Artifacts surfaces the artifacts a fulfilling commit produced, no manual attach.
func TestArtifactsFromFulfillingCommit(t *testing.T) {
	r := newRepo(t)
	priv := key(t)
	id, _ := New(r, "spec", priv, "dir", 1)

	art := putRef(t, r, object.ArtifactRef{ContentHash: hashOf(t, "built"), MediaType: "application/octet-stream"})
	implCommit(t, r, id, art)

	arts, err := Artifacts(r, id)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 1 || !arts[0].ContentHash.Equal(hashOf(t, "built")) {
		t.Fatalf("Artifacts = %+v, want the commit-produced one", arts)
	}
}
