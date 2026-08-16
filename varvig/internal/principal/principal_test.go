package principal

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/attest"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

func newRepo(t *testing.T) *repo.Repo {
	t.Helper()
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	return r
}

type signer struct {
	priv ed25519.PrivateKey
	pub  ed25519.PublicKey
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	return signer{priv: priv, pub: pub}
}

func (s signer) Public() ed25519.PublicKey     { return s.pub }
func (s signer) Sign(b []byte) ([]byte, error) { return ed25519.Sign(s.priv, b), nil }

func TestRegistryRoundTrip(t *testing.T) {
	r := newRepo(t)
	reg := Open(r)
	s := newSigner(t)
	p := object.Principal{Key: s.pub, Name: "director", Kind: object.KindHuman}

	if _, ok, _ := reg.Get(Fingerprint(p)); ok {
		t.Fatal("empty registry returned a principal")
	}
	if err := reg.Add(p, "admin", 1); err != nil {
		t.Fatalf("Add: %v", err)
	}
	got, ok, err := reg.Get(Fingerprint(p))
	if err != nil || !ok {
		t.Fatalf("Get ok=%v err=%v", ok, err)
	}
	if got.Name != "director" || got.Kind != object.KindHuman {
		t.Fatalf("got %+v", got)
	}
	if k, ok := reg.KindOf(Fingerprint(p)); !ok || k != object.KindHuman {
		t.Fatalf("KindOf = %v, %v", k, ok)
	}
}

func TestRegistryListAndRemove(t *testing.T) {
	r := newRepo(t)
	reg := Open(r)
	a := object.Principal{Key: newSigner(t).pub, Name: "a", Kind: object.KindHuman}
	b := object.Principal{Key: newSigner(t).pub, Name: "b", Kind: object.KindBridge}
	_ = reg.Add(a, "admin", 1)
	_ = reg.Add(b, "admin", 2)

	list, err := reg.List()
	if err != nil || len(list) != 2 {
		t.Fatalf("List = %d err=%v", len(list), err)
	}
	if err := reg.Remove(Fingerprint(a), "admin"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, ok, _ := reg.Get(Fingerprint(a)); ok {
		t.Fatal("removed principal still present")
	}
	if list, _ := reg.List(); len(list) != 1 {
		t.Fatalf("after remove, List = %d, want 1", len(list))
	}
}

// TestChartIsVersioned: every Add/Remove advances the ref, and the reflog holds
// the history — the org chart is auditable (§1.4).
func TestChartIsVersioned(t *testing.T) {
	r := newRepo(t)
	reg := Open(r)
	_ = reg.Add(object.Principal{Key: newSigner(t).pub, Name: "a", Kind: object.KindHuman}, "admin", 1)
	_ = reg.Add(object.Principal{Key: newSigner(t).pub, Name: "b", Kind: object.KindAgent}, "admin", 2)

	log, err := r.Refs.ReadLog(reserved.PrincipalsRef)
	if err != nil {
		t.Fatalf("ReadLog: %v", err)
	}
	if len(log) < 2 {
		t.Fatalf("chart reflog has %d entries, want >= 2", len(log))
	}
}

// TestRepoBackedKindCheck is the point of the registry: attest.VerifyWithPrincipal
// resolves kinds from the repo, so a bridge cannot mint a strong attestation and
// a keyholder cannot mint a weak one — with the org chart as the source of truth.
func TestRepoBackedKindCheck(t *testing.T) {
	r := newRepo(t)
	reg := Open(r)
	bridge := newSigner(t)
	human := newSigner(t)
	_ = reg.Add(object.Principal{Key: bridge.pub, Name: "ext-tracker", Kind: object.KindBridge}, "admin", 1)
	_ = reg.Add(object.Principal{Key: human.pub, Name: "dir", Kind: object.KindHuman}, "admin", 2)

	target := []byte("intent-revision")

	// Bridge signing strong: rejected via the repo-backed registry.
	strongFromBridge, _ := attest.Sign(bridge, object.Attestation{
		Target: target, Decision: object.DecisionApprove, Strength: object.StrengthStrong,
	})
	if _, _, err := attest.VerifyWithPrincipal(strongFromBridge, reg); !errors.Is(err, attest.ErrStrengthKind) {
		t.Fatalf("bridge strong via registry = %v, want ErrStrengthKind", err)
	}
	// Bridge signing weak: accepted.
	weakFromBridge, _ := attest.Sign(bridge, object.Attestation{
		Target: target, Decision: object.DecisionApprove, Strength: object.StrengthWeak,
	})
	if _, _, err := attest.VerifyWithPrincipal(weakFromBridge, reg); err != nil {
		t.Fatalf("bridge weak via registry = %v, want ok", err)
	}
	// Human signing strong: accepted.
	strongFromHuman, _ := attest.Sign(human, object.Attestation{
		Target: target, Decision: object.DecisionApprove, Strength: object.StrengthStrong,
	})
	if _, _, err := attest.VerifyWithPrincipal(strongFromHuman, reg); err != nil {
		t.Fatalf("human strong via registry = %v, want ok", err)
	}
	// An unregistered signer is unknown to the chart.
	stranger := newSigner(t)
	fromStranger, _ := attest.Sign(stranger, object.Attestation{
		Target: target, Decision: object.DecisionApprove, Strength: object.StrengthStrong,
	})
	if _, _, err := attest.VerifyWithPrincipal(fromStranger, reg); !errors.Is(err, attest.ErrUnknownPrincipal) {
		t.Fatalf("stranger via registry = %v, want ErrUnknownPrincipal", err)
	}
}
