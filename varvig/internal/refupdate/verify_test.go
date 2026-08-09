package refupdate

import (
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
)

type harness struct {
	repo   *repo.Repo
	signer identity.Signer
	fp     string
	newID  []byte
	verify *Verifier
	now    int64
}

func newHarness(t *testing.T, allowedKeys string) *harness {
	t.Helper()
	r, err := repo.Init(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	priv, pub := testSignerKey(t)
	signer := identity.FromPrivateKey(priv)
	fp := sshkey.PublicKey{Key: pub}.Fingerprint()

	// A real object to serve as the new tip.
	blob := object.NewBlob([]byte("promoted content"))
	newID, err := r.Objects.Put(blob)
	if err != nil {
		t.Fatal(err)
	}

	tf := trust.Parse([]byte(allowedKeys))
	h := &harness{
		repo:   r,
		signer: signer,
		fp:     fp,
		newID:  newID,
		now:    1000,
	}
	h.verify = &Verifier{
		Trust:   tf,
		Objects: r.Objects,
		Refs:    r.Refs,
		Replay:  NewMemoryGuard(),
		Now:     func() int64 { return h.now },
	}
	return h
}

// sign builds a fresh signed creation of refs/heads/main -> newID at the given
// scope and expiry.
func (h *harness) sign(t *testing.T, scope string, notAfter int64) *SignedUpdate {
	t.Helper()
	nonce, err := NewNonce()
	if err != nil {
		t.Fatal(err)
	}
	su, err := Sign(h.signer, Params{
		Ref:      "refs/heads/main",
		New:      h.newID,
		Scope:    scope,
		Nonce:    nonce,
		NotAfter: notAfter,
	})
	if err != nil {
		t.Fatal(err)
	}
	return su
}

func promoteAll(fp string) string {
	return "# key\n" + fp + " jan / promote\n"
}

func TestVerifyAccept(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))

	res, err := h.verify.Verify(h.sign(t, "/", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if !res.Accepted {
		t.Fatalf("expected accept, got %q", res.Reason)
	}
	got, err := h.repo.Refs.Resolve("refs/heads/main")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Equal(h.newID) {
		t.Fatal("ref did not advance to the new tip")
	}
	// The successful move is in the reflog.
	log, _ := h.repo.Refs.ReadLog("refs/heads/main")
	if len(log) != 1 || !log[0].New.Equal(h.newID) {
		t.Fatalf("reflog missing the accepted update: %+v", log)
	}
}

func TestVerifyRejectUnauthorized(t *testing.T) {
	h := newHarness(t, "") // empty trust store: nobody may promote
	res, err := h.verify.Verify(h.sign(t, "/", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted {
		t.Fatal("expected rejection for unauthorized signer")
	}
	if _, err := h.repo.Refs.Resolve("refs/heads/main"); err == nil {
		t.Fatal("ref must not exist after a rejected update")
	}
	// The authenticated-but-rejected attempt is audited.
	log, _ := h.repo.Refs.ReadLog("refs/heads/main")
	if len(log) != 1 {
		t.Fatalf("expected one audit entry, got %d", len(log))
	}
}

func TestVerifyRejectOutOfScope(t *testing.T) {
	h := newHarness(t, "")
	// Signer may promote only within src/web/, but the update claims root.
	h.verify.Trust = trust.Parse([]byte(h.fp + " jan src/web/ promote\n"))
	res, _ := h.verify.Verify(h.sign(t, "/", 5000))
	if res.Accepted {
		t.Fatal("root-scoped update must be rejected for a subtree-scoped signer")
	}
	// But an update within the signer's scope is accepted.
	res, _ = h.verify.Verify(h.sign(t, "src/web/app", 5000))
	if !res.Accepted {
		t.Fatalf("in-scope update should be accepted: %q", res.Reason)
	}
}

func TestVerifyRejectExpired(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))
	h.now = 10000 // well past not_after + skew
	res, _ := h.verify.Verify(h.sign(t, "/", 5000))
	if res.Accepted {
		t.Fatal("expired update must be rejected")
	}
}

func TestVerifyExpiryWithinSkew(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))
	// now is just past not_after but within the default 5-minute skew.
	h.now = 5000 + 100
	res, _ := h.verify.Verify(h.sign(t, "/", 5000))
	if !res.Accepted {
		t.Fatalf("update just past expiry but within skew should be accepted: %q", res.Reason)
	}
}

func TestVerifyRejectReplay(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))
	su := h.sign(t, "/", 5000)
	if res, _ := h.verify.Verify(su); !res.Accepted {
		t.Fatalf("first use should accept: %q", res.Reason)
	}
	// Replaying the identical signed update is rejected by the nonce guard.
	res, _ := h.verify.Verify(su)
	if res.Accepted || res.Reason == "" {
		t.Fatal("replayed update must be rejected")
	}
}

func TestVerifyRejectTamperedSignature(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))
	su := h.sign(t, "/", 5000)
	su.Sig[0] ^= 0xff // corrupt the signature
	res, _ := h.verify.Verify(su)
	if res.Accepted {
		t.Fatal("tampered signature must be rejected")
	}
	// A rejected *signature* is not an authenticated request: no audit entry.
	log, _ := h.repo.Refs.ReadLog("refs/heads/main")
	if len(log) != 0 {
		t.Fatalf("unauthenticated update should not be audited, got %d entries", len(log))
	}
}

func TestVerifyRejectTamperedPayload(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))
	su := h.sign(t, "/", 5000)
	// Re-encode with a mutated ref: the signature no longer covers these bytes.
	su.Payload.fields[0].val = []byte("refs/heads/evil")
	res, _ := h.verify.Verify(su)
	if res.Accepted {
		t.Fatal("payload tampering must break signature verification")
	}
}

func TestVerifyCASConflict(t *testing.T) {
	h := newHarness(t, "")
	h.verify.Trust = trust.Parse([]byte(promoteAll(h.fp)))
	// Pre-seed the ref to some other value so the creation precondition fails.
	other := mustHash(t, "someone-elses-tip")
	if err := h.repo.Refs.Create("refs/heads/main", other, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	res, err := h.verify.Verify(h.sign(t, "/", 5000))
	if err != nil {
		t.Fatal(err)
	}
	if res.Accepted {
		t.Fatal("expected CAS conflict")
	}
	if res.Current == nil || !res.Current.Equal(other) {
		t.Fatalf("conflict should report current head, got %v", res.Current)
	}
}

func TestSignedUpdateWireRoundTrip(t *testing.T) {
	priv, _ := testSignerKey(t)
	signer := identity.FromPrivateKey(priv)
	nonce, _ := NewNonce()
	su, err := Sign(signer, Params{
		Ref: "refs/heads/main", New: mustHash(t, "x"), Scope: "/",
		Nonce: nonce, NotAfter: 123,
	})
	if err != nil {
		t.Fatal(err)
	}
	wire := su.Encode()
	got, err := DecodeSigned(wire)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.VerifySignature(); err != nil {
		t.Fatalf("round-tripped signature should verify: %v", err)
	}
}
