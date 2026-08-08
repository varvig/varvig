package object

import (
	"bytes"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

func TestProvenanceRoundTrip(t *testing.T) {
	toolHash, _ := multihash.Sum(multihash.BLAKE3, []byte("varvig-binary"))
	p := Provenance{
		Authority:       "alice",
		Model:           "claude-opus",
		ModelVersion:    "4.8",
		Sampling:        "temperature=0.7",
		ToolPermissions: []string{"read:auth", "write:auth"},
		ToolHash:        toolHash,
		TaskSpec:        "add rate limiting",
		ContextRead:     "auth/*.go",
		Reasoning:       "guard the public interface",
	}
	obj := NewProvenance(p)
	got, err := Decode(obj.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	back, err := got.AsProvenance()
	if err != nil {
		t.Fatalf("AsProvenance: %v", err)
	}
	if back.Authority != p.Authority || back.Model != p.Model || back.ModelVersion != p.ModelVersion {
		t.Fatalf("identity fields lost: %+v", back)
	}
	if len(back.ToolPermissions) != 2 || back.ToolPermissions[1] != "write:auth" {
		t.Fatalf("tool perms lost: %v", back.ToolPermissions)
	}
	if !back.ToolHash.Equal(toolHash) {
		t.Fatal("tool hash lost")
	}
	if back.TaskSpec != p.TaskSpec || back.Reasoning != p.Reasoning {
		t.Fatalf("intent fields lost: %+v", back)
	}
}

func TestChangeCarriesProvenance(t *testing.T) {
	tree, _ := multihash.Sum(multihash.BLAKE3, []byte("tree"))
	prov, _ := multihash.Sum(multihash.BLAKE3, []byte("prov"))
	ch := NewChange(Change{Tree: tree, Provenance: prov, Message: "m", Timestamp: 1})
	got, err := Decode(ch.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	c, err := got.AsChange()
	if err != nil {
		t.Fatalf("AsChange: %v", err)
	}
	if !c.Provenance.Equal(prov) {
		t.Fatal("provenance id not round-tripped")
	}
	// Provenance must appear in the change's links so sync replicates it.
	links, _ := got.Links()
	found := false
	for _, l := range links {
		if l.Equal(prov) {
			found = true
		}
	}
	if !found {
		t.Fatal("provenance id missing from Links")
	}
}

func TestChangeWithoutProvenanceEncodesAsBefore(t *testing.T) {
	tree, _ := multihash.Sum(multihash.BLAKE3, []byte("tree"))
	ch := NewChange(Change{Tree: tree, Message: "m", Timestamp: 1})
	if _, ok := ch.Field(tagChangeProvenance); ok {
		t.Fatal("provenance field emitted when unset")
	}
}

func TestSignableBytesExcludesSignature(t *testing.T) {
	tree, _ := multihash.Sum(multihash.BLAKE3, []byte("tree"))
	prov, _ := multihash.Sum(multihash.BLAKE3, []byte("prov"))
	ch := NewChange(Change{Tree: tree, Provenance: prov, Message: "m", Timestamp: 1})

	before := ch.SignableBytes()
	ch.SetSignature([]byte("a-signature-blob"))
	after := ch.SignableBytes()

	if !bytes.Equal(before, after) {
		t.Fatal("attaching a signature changed SignableBytes")
	}
	// The full encoding does include the signature, so identity changes.
	if bytes.Equal(ch.Encode(), before) {
		t.Fatal("Encode() unexpectedly equals SignableBytes (signature not stored)")
	}
	// And the signed object still round-trips (unknown-field discipline).
	got, err := Decode(ch.Encode())
	if err != nil {
		t.Fatalf("Decode: %v", err)
	}
	if !bytes.Equal(got.Encode(), ch.Encode()) {
		t.Fatal("signed change did not round-trip")
	}
	if sig, ok := got.RawSignature(); !ok || string(sig) != "a-signature-blob" {
		t.Fatalf("signature not preserved: %q ok=%v", sig, ok)
	}
}
