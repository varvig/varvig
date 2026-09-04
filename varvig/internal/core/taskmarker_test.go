package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/provenance"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// TestSealedCheckoutCommitCarriesTaskProvenance is the F4 acceptance "a commit in
// a task checkout carries full task provenance": a checkout sealed with the task
// key produces, on an ordinary commit, a change whose provenance authority is the
// task's fingerprint and whose scope is the task's scope — end to end, without the
// shell setting the authority itself.
func TestSealedCheckoutCommitCarriesTaskProvenance(t *testing.T) {
	// A one-file base in the source repo.
	src, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	blob, _ := PutBlob(src, []byte("hello"))
	baseTree, _ := worktree.BuildTree(src.Objects, map[string]worktree.FileState{"README": {Hash: blob, Mode: 0o644}})
	baseChange, _, err := attachAndSign(src, object.Provenance{}, object.Change{Tree: baseTree, Message: "base", Timestamp: 1}, mustKey(t))
	if err != nil {
		t.Fatal(err)
	}

	// Provision and seal a task checkout with a known task key.
	dst, _, err := ProvisionCheckout(src, t.TempDir(), baseChange)
	if err != nil {
		t.Fatal(err)
	}
	taskKey := mustKey(t)
	fp := DerivedAuthority(taskKey)
	marker := TaskMarker{Fingerprint: fp, Scope: "src/auth", Base: baseChange.Hex()}
	if err := SealTaskCheckout(dst, marker, taskKey); err != nil {
		t.Fatal(err)
	}

	// The marker reads back, and the seeded identity is the task key — the key the
	// checkout's `commit` shell loads.
	if m, ok, err := ReadTaskMarker(dst); err != nil || !ok || m.Scope != "src/auth" {
		t.Fatalf("marker not readable: %+v ok=%v err=%v", m, ok, err)
	}
	signer, err := provenance.LoadOrCreateIdentity(dst.GitDir())
	if err != nil {
		t.Fatal(err)
	}
	if DerivedAuthority(signer) != fp {
		t.Fatal("seeded checkout identity is not the task key")
	}

	// An in-checkout commit, the way the CLI does it: stamp the marker's scope and
	// sign with the loaded (seeded) identity. The authority comes out as the task
	// fingerprint though we never set it.
	tip, _ := TreeOf(dst, baseChange)
	cr, err := Commit(dst, CLICapabilities(), CommitParams{
		Ref: "refs/heads/main", ExpectedOld: baseChange, Tree: tip,
		Parents: []multihash.Multihash{baseChange}, Message: "work", Author: "agent",
		Provenance: object.Provenance{Scope: marker.Scope}, Signer: signer, Now: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	pv := storedProvenance(t, dst, cr.Provenance)
	if pv.Authority != fp {
		t.Fatalf("commit authority = %q, want task fingerprint %q", pv.Authority, fp)
	}
	if pv.Scope != "src/auth" {
		t.Fatalf("commit scope = %q, want %q", pv.Scope, "src/auth")
	}

	// The loop closes: with the scheduler's record present, the committed change
	// re-verifies against it.
	if err := RecordTask(dst, TaskRecord{Fingerprint: fp, Scope: "src/auth", Base: baseChange.Hex()}); err != nil {
		t.Fatal(err)
	}
	if err := VerifyTaskScope(dst, cr.Change); err != nil {
		t.Fatalf("a task-authored, in-scope commit should re-verify: %v", err)
	}
}

// TestUnsealedRepoHasNoMarker: a plain repository reports no task marker, so the
// CLI verbs behave exactly as they do outside a checkout.
func TestUnsealedRepoHasNoMarker(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if _, ok, err := ReadTaskMarker(r); err != nil || ok {
		t.Fatalf("plain repo reported a marker: ok=%v err=%v", ok, err)
	}
}

func mustKey(t *testing.T) ed25519.PrivateKey {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	return priv
}

func storedProvenance(t *testing.T, r *repo.Repo, provID multihash.Multihash) object.Provenance {
	t.Helper()
	obj, err := r.Objects.Get(provID)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := obj.AsProvenance()
	if err != nil {
		t.Fatal(err)
	}
	return pv
}
