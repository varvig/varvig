package txn

import (
	"context"
	"testing"

	"github.com/varvig/varvig/varvig/internal/multihash"
	"github.com/varvig/varvig/varvig/internal/object"
	"github.com/varvig/varvig/varvig/internal/repo"
)

// putProvenance stores a provenance object and returns its id — standing in for
// the evidence an executor produces before a transaction runs.
func putProvenance(t *testing.T, r *repo.Repo, p object.Provenance) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewProvenance(p))
	if err != nil {
		t.Fatalf("put provenance: %v", err)
	}
	return id
}

// changeProvenanceOf reads the provenance id stamped on a committed change.
func changeProvenanceOf(t *testing.T, r *repo.Repo, change multihash.Multihash) multihash.Multihash {
	t.Helper()
	obj, err := r.Objects.Get(change)
	if err != nil {
		t.Fatalf("get change: %v", err)
	}
	c, err := obj.AsChange()
	if err != nil {
		t.Fatalf("as change: %v", err)
	}
	return c.Provenance
}

// TestSchedulerAttachesProvenance is the core of §10b item 2: the autonomous
// commit path stamps the declared provenance on the change it writes, so a
// change produced without going through core.Commit still points at its
// evidence.
func TestSchedulerAttachesProvenance(t *testing.T) {
	r := newRepo(t)
	prov := putProvenance(t, r, object.Provenance{Authority: "agent-a", Model: "m", Scope: "x"})

	s := NewScheduler(r, mainRef)
	res := s.Run(context.Background(), []*Txn{{
		Name:       "evidenced",
		Writes:     []string{"x"},
		Provenance: prov,
		Apply:      func(ws *Workspace) error { return ws.Write("x", []byte("v")) },
	}})
	if res[0].Err != nil {
		t.Fatalf("run: %v", res[0].Err)
	}
	got := changeProvenanceOf(t, r, res[0].Change)
	if !got.Equal(prov) {
		t.Fatalf("committed change provenance = %v; want %v", got, prov)
	}
}

// TestSchedulerNoProvenanceByDefault keeps the field additive: a transaction
// that declares no provenance commits a change with none — byte-identical to
// before this field existed.
func TestSchedulerNoProvenanceByDefault(t *testing.T) {
	r := newRepo(t)
	s := NewScheduler(r, mainRef)
	res := s.Run(context.Background(), []*Txn{{
		Name:   "plain",
		Writes: []string{"x"},
		Apply:  func(ws *Workspace) error { return ws.Write("x", []byte("v")) },
	}})
	if res[0].Err != nil {
		t.Fatalf("run: %v", res[0].Err)
	}
	if got := changeProvenanceOf(t, r, res[0].Change); got != nil {
		t.Fatalf("a transaction with no declared provenance committed one: %v", got)
	}
}

// TestLandCarriesProposalProvenance: landing records a new change against the
// current tip, and that change describes the same work the proposal did, so it
// must point at the same evidence.
func TestLandCarriesProposalProvenance(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	base := tip(t, r)
	prov := putProvenance(t, r, object.Provenance{Authority: "agent-a", Model: "m"})

	p := proposalWithProvenance(t, r, base, prov, map[string]string{"a": "a2"})
	res, err := LandProposal(context.Background(), r, mainRef, p, nil)
	if err != nil {
		t.Fatalf("land: %v", err)
	}
	got := changeProvenanceOf(t, r, res.Change)
	if !got.Equal(prov) {
		t.Fatalf("landed change provenance = %v; want the proposal's %v", got, prov)
	}
}

// TestLandWithoutProvenanceCarriesNone: a proposal with no evidence lands a
// change with none — landing never fabricates provenance.
func TestLandWithoutProvenanceCarriesNone(t *testing.T) {
	r := newRepo(t)
	seed(t, r, "a", "a1")
	base := tip(t, r)

	p := proposalFrom(t, r, base, map[string]string{"a": "a2"}) // no provenance
	res, err := LandProposal(context.Background(), r, mainRef, p, nil)
	if err != nil {
		t.Fatalf("land: %v", err)
	}
	if got := changeProvenanceOf(t, r, res.Change); got != nil {
		t.Fatalf("landing fabricated provenance %v for a proposal that had none", got)
	}
}

// proposalWithProvenance is proposalFrom plus a provenance id on the change — an
// executor's proposal that names the evidence it produced.
func proposalWithProvenance(t *testing.T, r *repo.Repo, base, prov multihash.Multihash, writes map[string]string, removes ...string) multihash.Multihash {
	t.Helper()
	baseTree, err := changeTree(r.Objects, base)
	if err != nil {
		t.Fatalf("changeTree: %v", err)
	}
	files, err := flatten(r.Objects, baseTree)
	if err != nil {
		t.Fatalf("flatten base: %v", err)
	}
	nf := map[string]fileEnt{}
	for k, v := range files {
		nf[k] = v
	}
	for p, c := range writes {
		id, err := r.Objects.Put(object.NewBlob([]byte(c)))
		if err != nil {
			t.Fatalf("put blob: %v", err)
		}
		nf[p] = fileEnt{ID: id, Mode: modeFile}
	}
	for _, p := range removes {
		delete(nf, p)
	}
	tree, err := buildTree(r.Objects, nf)
	if err != nil {
		t.Fatalf("buildTree: %v", err)
	}
	var parents []multihash.Multihash
	if base != nil {
		parents = []multihash.Multihash{base}
	}
	ch := object.NewChange(object.Change{Tree: tree, Parents: parents, Message: "proposal", Author: "test", Provenance: prov})
	id, err := r.Objects.Put(ch)
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	return id
}
