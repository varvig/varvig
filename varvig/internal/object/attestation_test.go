package object

import (
	"bytes"
	"testing"
)

func TestAttestationRoundTrip(t *testing.T) {
	target := mustID(t, NewBlob([]byte("intent revision")))
	policy := mustID(t, NewBlob([]byte("policy module")))
	a := NewAttestation(Attestation{
		Target:    target,
		Decision:  DecisionApprove,
		Strength:  StrengthStrong,
		Timestamp: 1723100000,
		Rationale: "scope looks right",
		Policy:    policy,
	})
	got, err := Decode(a.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if got.Type() != TypeAttestation {
		t.Fatalf("type = %v", got.Type())
	}
	dec, err := got.AsAttestation()
	if err != nil {
		t.Fatalf("AsAttestation: %v", err)
	}
	if !dec.Target.Equal(target) || dec.Decision != DecisionApprove || dec.Strength != StrengthStrong {
		t.Fatalf("mismatch: %+v", dec)
	}
	if dec.Rationale != "scope looks right" || !dec.Policy.Equal(policy) || dec.Timestamp != 1723100000 {
		t.Fatalf("optional fields lost: %+v", dec)
	}
	if !bytes.Equal(got.Encode(), a.Encode()) {
		t.Fatal("attestation not byte-identical on round-trip")
	}
}

func TestAttestationMinimalOmitsOptional(t *testing.T) {
	target := mustID(t, NewBlob([]byte("x")))
	a := NewAttestation(Attestation{Target: target, Decision: DecisionVeto, Strength: StrengthWeak})
	if _, ok := a.Field(tagAttestRationale); ok {
		t.Fatal("empty rationale should be omitted")
	}
	if _, ok := a.Field(tagAttestPolicy); ok {
		t.Fatal("nil policy should be omitted")
	}
}

func TestStrengthOrdering(t *testing.T) {
	// weak never satisfies a strong requirement; there is no upgrade path.
	if StrengthWeak.Satisfies(StrengthStrong) {
		t.Fatal("weak satisfied strong")
	}
	if StrengthDelegated.Satisfies(StrengthStrong) {
		t.Fatal("delegated satisfied strong")
	}
	if !StrengthStrong.Satisfies(StrengthStrong) || !StrengthStrong.Satisfies(StrengthWeak) {
		t.Fatal("strong should satisfy strong and weak")
	}
	if !StrengthDelegated.Satisfies(StrengthWeak) {
		t.Fatal("delegated should satisfy weak")
	}
	if StrengthUnknown.Satisfies(StrengthWeak) || StrengthWeak.Satisfies(StrengthUnknown) {
		t.Fatal("unknown strength must satisfy nothing")
	}
}

func TestPrincipalRoundTrip(t *testing.T) {
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i)
	}
	p := NewPrincipal(Principal{Key: key, Name: "director", Kind: KindHuman})
	got, err := Decode(p.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	dec, err := got.AsPrincipal()
	if err != nil {
		t.Fatalf("AsPrincipal: %v", err)
	}
	if dec.Name != "director" || dec.Kind != KindHuman || !bytes.Equal(dec.Key, key) {
		t.Fatalf("mismatch: %+v", dec)
	}
	if !bytes.Equal(got.Encode(), p.Encode()) {
		t.Fatal("principal not byte-identical on round-trip")
	}
}

// TestAttestationLinksPinTargetAndPolicy guards the GC-root property (D4): an
// attestation object links to the intent revision it signed and the policy in
// force, so neither is reclaimed while the attestation survives.
func TestAttestationLinksPinTargetAndPolicy(t *testing.T) {
	target := mustID(t, NewBlob([]byte("intent")))
	policy := mustID(t, NewBlob([]byte("policy")))
	a := NewAttestation(Attestation{Target: target, Decision: DecisionApprove, Strength: StrengthStrong, Policy: policy})
	links, err := a.Links()
	if err != nil {
		t.Fatalf("Links: %v", err)
	}
	want := map[string]bool{target.Hex(): false, policy.Hex(): false}
	for _, l := range links {
		if _, ok := want[l.Hex()]; ok {
			want[l.Hex()] = true
		}
	}
	for id, seen := range want {
		if !seen {
			t.Fatalf("attestation did not link %s", id)
		}
	}
}
