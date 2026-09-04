package core

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// proposeTouching builds a tree containing exactly the given paths and proposes
// it (no parent, so every path counts as written), returning the change id and
// the signer's derived authority. The provenance carries scope verbatim, which is
// what VerifyTaskScope re-checks against the scheduler's record.
func proposeTouching(t *testing.T, r *repo.Repo, signer ed25519.PrivateKey, scope string, paths ...string) multihash.Multihash {
	t.Helper()
	states := map[string]worktree.FileState{}
	for _, p := range paths {
		h, err := PutBlob(r, []byte("content of "+p))
		if err != nil {
			t.Fatal(err)
		}
		states[p] = worktree.FileState{Hash: h, Mode: 0o644}
	}
	tree, err := worktree.BuildTree(r.Objects, states)
	if err != nil {
		t.Fatal(err)
	}
	res, err := Propose(r, CLICapabilities(), ProposeParams{
		Tree: tree, Message: "m", Author: "a", Scope: scope, Signer: signer, SpecTask: "t", Now: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	return res.Change
}

// TestVerifyTaskScope_MatchingAndInScope: a change whose recorded task scope
// matches its self-description and whose writes fall within scope promotes.
func TestVerifyTaskScope_MatchingAndInScope(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	fp := DerivedAuthority(priv)
	if err := RecordTask(r, TaskRecord{Fingerprint: fp, Scope: "src/auth", Base: ""}); err != nil {
		t.Fatal(err)
	}
	id := proposeTouching(t, r, priv, "src/auth", "src/auth/rate.go", "src/auth/token.go")
	if err := VerifyTaskScope(r, id); err != nil {
		t.Fatalf("in-scope change from a recorded task should verify: %v", err)
	}
}

// TestVerifyTaskScope_ScopeDisagreesWithRecord: a change that widened its own
// self-described scope beyond what the scheduler granted is rejected even if its
// writes happen to fit — the record, not the self-description, is authoritative.
func TestVerifyTaskScope_ScopeDisagreesWithRecord(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	fp := DerivedAuthority(priv)
	if err := RecordTask(r, TaskRecord{Fingerprint: fp, Scope: "src/auth", Base: ""}); err != nil {
		t.Fatal(err)
	}
	id := proposeTouching(t, r, priv, "src/auth,src/api", "src/auth/rate.go")
	if err := VerifyTaskScope(r, id); err == nil {
		t.Fatal("a change claiming a wider scope than its task record should be rejected")
	}
}

// TestVerifyTaskScope_WroteOutsideScope: a change whose self-description agrees
// with the record but which wrote a path outside that scope is rejected.
func TestVerifyTaskScope_WroteOutsideScope(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	fp := DerivedAuthority(priv)
	if err := RecordTask(r, TaskRecord{Fingerprint: fp, Scope: "src/auth", Base: ""}); err != nil {
		t.Fatal(err)
	}
	id := proposeTouching(t, r, priv, "src/auth", "src/auth/rate.go", "src/payments/charge.go")
	if err := VerifyTaskScope(r, id); err == nil {
		t.Fatal("a change writing outside its task scope should be rejected")
	}
}

// TestVerifyTaskScope_NoRecordedTask: a change whose authority names no recorded
// task (a human commit, a foreign import) is unaffected — the check only binds
// changes the scheduler itself minted.
func TestVerifyTaskScope_NoRecordedTask(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	// Deliberately record nothing.
	id := proposeTouching(t, r, priv, "src/anywhere", "src/anywhere/x.go")
	if err := VerifyTaskScope(r, id); err != nil {
		t.Fatalf("a change with no recorded task must not be gated by task scope: %v", err)
	}
}

// TestRecordTaskRoundTrip: a recorded task is retrievable by fingerprint, and an
// unrecorded fingerprint reports absent.
func TestRecordTaskRoundTrip(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	want := TaskRecord{Fingerprint: "SHA256:abc", Scope: "src/auth", Base: "deadbeef"}
	if err := RecordTask(r, want); err != nil {
		t.Fatal(err)
	}
	got, ok, err := LookupTask(r, "SHA256:abc")
	if err != nil || !ok {
		t.Fatalf("recorded task not found: ok=%v err=%v", ok, err)
	}
	if got != want {
		t.Fatalf("round-trip mismatch: got %+v want %+v", got, want)
	}
	if _, ok, _ := LookupTask(r, "SHA256:missing"); ok {
		t.Fatal("unrecorded fingerprint reported present")
	}
}
