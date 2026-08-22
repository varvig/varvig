package gc

import (
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/pin"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/spec"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

// mkChange builds change(content) with an optional parent and returns the
// change id plus the ids of its unique tree and blob.
func mkChange(t *testing.T, r *repo.Repo, content string, parent multihash.Multihash) (change, tree, blob multihash.Multihash) {
	t.Helper()
	blob, err := r.Objects.Put(object.NewBlob([]byte(content)))
	if err != nil {
		t.Fatalf("blob: %v", err)
	}
	tree, err = r.Objects.Put(object.NewTree([]object.Entry{
		{Name: "f", Mode: 0o100644, Kind: object.TypeBlob, ID: blob},
	}))
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	ch := object.Change{Tree: tree, Message: content}
	if parent != nil {
		ch.Parents = []multihash.Multihash{parent}
	}
	change, err = r.Objects.Put(object.NewChange(ch))
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	return change, tree, blob
}

func TestGCKeepsReachable(t *testing.T) {
	r := newRepo(t)
	c1, _, _ := mkChange(t, r, "one", nil)
	if err := r.Refs.Create("refs/heads/main", c1, "t", "c1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	c2, _, _ := mkChange(t, r, "two", c1)
	if err := r.Refs.CompareAndSwap("refs/heads/main", c1, c2, "t", "c2"); err != nil {
		t.Fatalf("cas: %v", err)
	}
	rep, err := Collect(r, nil, true)
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if rep.Deleted != 0 {
		t.Fatalf("reachable objects marked for deletion: %d", rep.Deleted)
	}
}

func TestGCReclaimsPrunedSpeculation(t *testing.T) {
	r := newRepo(t)
	base, _, _ := mkChange(t, r, "base", nil)
	if err := r.Refs.Create("refs/heads/main", base, "t", "base"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A speculation not on any ref, with unique objects.
	specCh, specTree, specBlob := mkChange(t, r, "speculation", base)
	pool := spec.Open(r.GitDir())
	if err := pool.Add("task", specCh, 0); err != nil {
		t.Fatalf("Add: %v", err)
	}

	// While in the pool, the speculation is a root: GC keeps it.
	rep, err := Collect(r, pool, true)
	if err != nil {
		t.Fatalf("Collect dry: %v", err)
	}
	for _, id := range rep.DeletedIDs {
		if id.Equal(specCh) {
			t.Fatal("pooled speculation marked for deletion")
		}
	}

	// Retention drops it; now GC reclaims it and its unique objects.
	if _, err := pool.Prune("task", 0); err != nil {
		t.Fatalf("Prune: %v", err)
	}
	if _, err := Collect(r, pool, false); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	for _, id := range []multihash.Multihash{specCh, specTree, specBlob} {
		if r.Objects.Has(id) {
			t.Fatalf("object %s survived GC after pruning", id.Hex()[4:16])
		}
	}
	// The promoted line is untouched.
	if !r.Objects.Has(base) {
		t.Fatal("reachable base change was deleted")
	}
}

// TestGCReflogPreservesUndo: a change no longer reachable from any ref, but
// present in the reflog, must survive GC — universal undo (design §2).
func TestGCReflogPreservesUndo(t *testing.T) {
	r := newRepo(t)
	c1, _, _ := mkChange(t, r, "one", nil)
	if err := r.Refs.Create("refs/heads/main", c1, "t", "c1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	c2, _, _ := mkChange(t, r, "two", nil) // NOT a descendant of c1
	if err := r.Refs.CompareAndSwap("refs/heads/main", c1, c2, "t", "to-c2"); err != nil {
		t.Fatalf("cas c2: %v", err)
	}
	// Reset back to c1: c2 is now unreachable from the ref, only in the reflog.
	if err := r.Refs.CompareAndSwap("refs/heads/main", c2, c1, "t", "reset"); err != nil {
		t.Fatalf("cas reset: %v", err)
	}

	if _, err := Collect(r, nil, false); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if !r.Objects.Has(c2) {
		t.Fatal("reflog-reachable change was garbage collected (undo broken)")
	}
}

// TestGCReclaimsAfterReflogExpiry closes the loop from step 9: a change pinned
// ONLY by an old reflog entry survives GC until the reflog is expired, then is
// reclaimed — the resolution of the §2-vs-§1.5 tension.
func TestGCReclaimsAfterReflogExpiry(t *testing.T) {
	r := newRepo(t)
	var tick int64
	r.Refs.SetClock(func() time.Time { tick++; return time.Unix(0, tick*1000) })

	c1, _, _ := mkChange(t, r, "one", nil)
	if err := r.Refs.Create("refs/heads/main", c1, "t", "c1"); err != nil {
		t.Fatalf("create: %v", err)
	}
	// A speculation committed via a scratch ref, then the ref is deleted: the
	// change is now reachable only through the scratch reflog.
	spculch, _, _ := mkChange(t, r, "speculative", c1)
	if err := r.Refs.Create("refs/heads/scratch", spculch, "t", "spec"); err != nil {
		t.Fatalf("scratch create: %v", err)
	}
	if err := r.Refs.Delete("refs/heads/scratch", spculch, "t", "drop"); err != nil {
		t.Fatalf("scratch delete: %v", err)
	}

	// Before expiry: the reflog protects it (undo preserved).
	if _, err := Collect(r, nil, false); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !r.Objects.Has(spculch) {
		t.Fatal("speculation collected while still in the reflog")
	}

	// Expire everything (cutoff in the far future, keep none), then GC reclaims.
	if _, err := r.Refs.ExpireAll(0, 1<<62); err != nil {
		t.Fatalf("expire: %v", err)
	}
	if _, err := Collect(r, nil, false); err != nil {
		t.Fatalf("collect: %v", err)
	}
	if r.Objects.Has(spculch) {
		t.Fatal("speculation not reclaimed after reflog expiry")
	}
	// The live branch tip is untouched.
	if !r.Objects.Has(c1) {
		t.Fatal("live change reclaimed")
	}
}

// TestGCReachabilityThroughArtifactRef covers federation §1: an artifact-ref
// named by a reachable change stays reachable (its external bytes pinned), and
// once no reachable change names it, it is swept and reported exactly once by
// --report-external.
func TestGCReachabilityThroughArtifactRef(t *testing.T) {
	r := newRepo(t)

	// An external artifact and its artifact-ref handle.
	extHash, err := multihash.Sum(multihash.Default, []byte("container image bytes"))
	if err != nil {
		t.Fatalf("sum: %v", err)
	}
	artID, err := r.Objects.Put(object.NewArtifactRef(object.ArtifactRef{
		ContentHash: extHash,
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		Size:        123,
		Locators:    []string{"oci://reg.example/img@sha256:deadbeef"},
	}))
	if err != nil {
		t.Fatalf("artifact-ref: %v", err)
	}

	// A change that names the artifact-ref, kept alive by the branch.
	blob, _ := r.Objects.Put(object.NewBlob([]byte("src")))
	tree, _ := r.Objects.Put(object.NewTree([]object.Entry{{Name: "f", Mode: 0o100644, Kind: object.TypeBlob, ID: blob}}))
	ch, err := r.Objects.Put(object.NewChange(object.Change{
		Tree: tree, Message: "release", Artifacts: []multihash.Multihash{artID},
	}))
	if err != nil {
		t.Fatalf("change: %v", err)
	}
	if err := r.Refs.Create("refs/heads/main", ch, "t", "release"); err != nil {
		t.Fatalf("create: %v", err)
	}

	// While the change is reachable, the artifact-ref survives and nothing is
	// reported external.
	rep, err := Collect(r, nil, true)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if !r.Objects.Has(artID) {
		t.Fatal("artifact-ref swept while its change is reachable")
	}
	if len(rep.ExternalUnreachable) != 0 {
		t.Fatalf("reported external while reachable: %+v", rep.ExternalUnreachable)
	}

	// Drop the branch: the change (and its artifact-ref) become unreachable.
	if err := r.Refs.Delete("refs/heads/main", ch, "t", "drop"); err != nil {
		t.Fatalf("delete ref: %v", err)
	}
	if _, err := r.Refs.ExpireAll(0, 1<<62); err != nil {
		t.Fatalf("expire: %v", err)
	}
	rep, err = Collect(r, nil, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if r.Objects.Has(artID) {
		t.Fatal("artifact-ref not swept after its change went unreachable")
	}
	if len(rep.ExternalUnreachable) != 1 {
		t.Fatalf("want exactly one external-unreachable, got %d", len(rep.ExternalUnreachable))
	}
	if !rep.ExternalUnreachable[0].ContentHash.Equal(extHash) {
		t.Fatalf("reported wrong content hash: %s", rep.ExternalUnreachable[0].ContentHash.Hex())
	}

	// A second pass must not re-report it — the handle is already gone.
	rep, err = Collect(r, nil, false)
	if err != nil {
		t.Fatalf("collect: %v", err)
	}
	if len(rep.ExternalUnreachable) != 0 {
		t.Fatalf("external re-reported after removal: %+v", rep.ExternalUnreachable)
	}
}

// TestGCPinReachabilityAndExpiry covers federation §3: a live pin keeps its
// object reachable (a pin is an ordinary ref, hence a root), and once the pin
// expires the object is collectable without ceremony.
func TestGCPinReachabilityAndExpiry(t *testing.T) {
	r := newRepo(t)

	// A standalone object with no ref of its own.
	blob, err := r.Objects.Put(object.NewBlob([]byte("pinned bytes")))
	if err != nil {
		t.Fatal(err)
	}

	// A live pin (far-future expiry) as an ordinary ref. Expire reflogs so the
	// pin ref is the only thing that could keep the object alive.
	livePinName := pin.RefName("peer-A", 1<<40, blob)
	if err := r.Refs.Create(livePinName, blob, "peer-A", "pin"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refs.ExpireAll(0, 1<<62); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(r, nil, false); err != nil {
		t.Fatal(err)
	}
	if !r.Objects.Has(blob) {
		t.Fatal("object with a live pin was collected")
	}

	// Now expire the pin: replace it with a past-expiry pin ref. With reflogs
	// expired, the expired pin is not a root, so the object is reclaimed.
	if err := r.Refs.Delete(livePinName, blob, "peer-A", "unpin"); err != nil {
		t.Fatal(err)
	}
	expiredName := pin.RefName("peer-A", 1, blob) // not_after = 1s past epoch
	if err := r.Refs.Create(expiredName, blob, "peer-A", "pin"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Refs.ExpireAll(0, 1<<62); err != nil {
		t.Fatal(err)
	}
	if _, err := Collect(r, nil, false); err != nil {
		t.Fatal(err)
	}
	if r.Objects.Has(blob) {
		t.Fatal("object behind an expired pin was not collected")
	}
}
