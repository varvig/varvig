// Package conformance is Loom's frozen-format conformance suite (design §4.7).
// It pins the parts that must be readable in thirty years — object encodings,
// their identities, multihash framing, unknown-field preservation, and the wire
// frame format — as golden byte vectors. Every build must reproduce them
// exactly; any drift is a format-compatibility break and fails loudly.
//
// The suite is itself a content-addressed artifact: the canonical JSON of the
// golden vectors has a stable multihash (see SuiteID), so a cross-version
// interop matrix is simply "every version agrees on this artifact." The two
// guarantees a matrix must check — that an old build still reads new bytes, and
// that unknown fields round-trip untouched — are encoded as the RoundTrip
// vectors here (design §4.4, §4.7).
package conformance

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"fmt"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
	"github.com/dividebyzero/claude-experiments/loom/internal/wire"
)

// ObjVector is a frozen object: its canonical bytes and identity.
type ObjVector struct {
	Name string `json:"name"`
	Type uint64 `json:"type"`
	Hex  string `json:"hex"` // canonical LOM1 bytes
	ID   string `json:"id"`  // blake3 multihash of the bytes
}

// MHVector is a frozen (algorithm, input) -> multihash mapping.
type MHVector struct {
	Algo     string `json:"algo"`
	Code     uint64 `json:"code"`
	InputHex string `json:"input_hex"`
	MH       string `json:"mh"`
}

// WireVector is a frozen wire frame or handshake byte string.
type WireVector struct {
	Name string `json:"name"`
	Hex  string `json:"hex"`
}

// Generated holds the vectors a build derives from current code; a build's
// output must equal the golden Generated section byte-for-byte.
type Generated struct {
	Objects   []ObjVector  `json:"objects"`
	Multihash []MHVector   `json:"multihash"`
	Wire      []WireVector `json:"wire"`
}

// Vectors is the full suite: generated vectors plus round-trip-only objects
// (unknown fields / unknown types) that must decode and re-encode unchanged.
type Vectors struct {
	Generated Generated   `json:"generated"`
	RoundTrip []ObjVector `json:"round_trip"`
}

// fixedID returns a deterministic stand-in object id (never hashed content, so
// vectors have no external dependencies).
func fixedID(seed string) multihash.Multihash {
	mh, _ := multihash.Sum(multihash.BLAKE3, []byte(seed))
	return mh
}

func objVector(name string, o *object.Object) ObjVector {
	enc := o.Encode()
	id, _ := multihash.Sum(multihash.BLAKE3, enc)
	return ObjVector{
		Name: name,
		Type: uint64(o.Type()),
		Hex:  hex.EncodeToString(enc),
		ID:   id.Hex(),
	}
}

// GenerateObjects builds the canonical objects from fixed inputs.
func generateObjects() []ObjVector {
	tree := fixedID("tree")
	blobA := fixedID("blob-a")
	blobB := fixedID("blob-b")
	parent := fixedID("parent")
	prov := fixedID("provenance")
	toolHash := fixedID("tool")

	changeSigned := object.NewChange(object.Change{
		Tree:       tree,
		Parents:    []multihash.Multihash{parent},
		Message:    "did the thing",
		Timestamp:  1723100000,
		Author:     "agent-7",
		Provenance: prov,
	})
	changeSigned.SetSignature([]byte{0x01, 0xde, 0xad, 0xbe, 0xef})

	return []ObjVector{
		objVector("blob/hello", object.NewBlob([]byte("hello, agents\n"))),
		objVector("blob/empty", object.NewBlob(nil)),
		objVector("tree/two-entries", object.NewTree([]object.Entry{
			{Name: "README.md", Mode: 0o100644, Kind: object.TypeBlob, ID: blobB},
			{Name: "run.sh", Mode: 0o100755, Kind: object.TypeBlob, ID: blobA},
		})),
		objVector("change/minimal", object.NewChange(object.Change{
			Tree: tree, Message: "initial", Timestamp: 1723100000, Author: "agent-0",
		})),
		objVector("change/signed-with-provenance", changeSigned),
		objVector("provenance/full", object.NewProvenance(object.Provenance{
			Authority: "alice", Model: "claude", ModelVersion: "x",
			Sampling: "temperature=0.7", ToolPermissions: []string{"read", "write"},
			ToolHash: toolHash, TaskSpec: "task", ContextRead: "ctx", Reasoning: "plan",
		})),
		objVector("note/review", object.NewNote(object.Note{
			Target: tree, Namespace: "review", Payload: []byte("LGTM"),
			Parent: parent, Timestamp: 1723100000, Author: "bob",
		})),
		objVector("hookconfig/one", object.NewHookConfig(object.HookConfig{
			Entries: []object.HookEntry{{Event: "pre-commit", Module: blobA}},
		})),
	}
}

func generateMultihash() []MHVector {
	out := []MHVector{}
	for _, c := range []multihash.Code{multihash.SHA2_256, multihash.BLAKE3} {
		for _, in := range [][]byte{[]byte("loom"), {}} {
			mh, _ := multihash.Sum(c, in)
			out = append(out, MHVector{
				Algo: multihash.Name(c), Code: uint64(c),
				InputHex: hex.EncodeToString(in), MH: mh.Hex(),
			})
		}
	}
	return out
}

func generateWire() []WireVector {
	var out []WireVector

	var hs bytes.Buffer
	_ = wire.NewConn(&hs).WriteHandshake(wire.Hello{
		Proto: wire.Proto, Caps: []string{wire.CapDeflate}, Hashes: []uint64{0x1e, 0x12},
	})
	out = append(out, WireVector{Name: "handshake", Hex: hex.EncodeToString(hs.Bytes())})

	var refs bytes.Buffer
	rc := wire.NewConn(&refs)
	_ = rc.WriteRefs([]wire.Ref{{Name: "refs/heads/main", ID: []byte{0x1e, 0x20, 0x01, 0x02}}})
	_ = rc.Flush()
	out = append(out, WireVector{Name: "refs-frame", Hex: hex.EncodeToString(refs.Bytes())})

	return out
}

// Generate derives the current build's generated vectors.
func Generate() Generated {
	return Generated{
		Objects:   generateObjects(),
		Multihash: generateMultihash(),
		Wire:      generateWire(),
	}
}

// roundTripVectors are hand-authored bytes that a compliant build must decode
// and re-encode unchanged: an unknown field on a known type, and an object of
// an entirely unknown type. These are the §4.4 preserve-or-refuse guarantees.
func roundTripVectors() []ObjVector {
	// A blob (type 1) carrying an unknown field with tag 99.
	blob := object.NewBlob([]byte("payload"))
	blob.SetField(99, []byte("provenance-from-the-future"))
	unknownField := objVector("blob/unknown-field", blob)

	// An object of an unknown type (9999) with one field, hand-built.
	var raw []byte
	raw = append(raw, object.Magic[:]...)
	raw = appendUvarint(raw, 9999)
	raw = appendUvarint(raw, 1) // one field
	raw = appendUvarint(raw, 7) // tag 7
	raw = appendUvarint(raw, 3) // len 3
	raw = append(raw, 'a', 'b', 'c')
	id, _ := multihash.Sum(multihash.BLAKE3, raw)
	unknownType := ObjVector{Name: "opaque/unknown-type", Type: 9999, Hex: hex.EncodeToString(raw), ID: id.Hex()}

	return []ObjVector{unknownField, unknownType}
}

// Build assembles the complete suite from current code.
func Build() Vectors {
	return Vectors{Generated: Generate(), RoundTrip: roundTripVectors()}
}

// CanonicalJSON returns the suite serialized deterministically. Its multihash
// is the suite's content-addressed identity (SuiteID).
func CanonicalJSON(v Vectors) ([]byte, error) {
	return json.MarshalIndent(v, "", "  ")
}

// SuiteID is the content-addressed identity of a suite artifact.
func SuiteID(jsonBytes []byte) multihash.Multihash {
	mh, _ := multihash.Sum(multihash.BLAKE3, jsonBytes)
	return mh
}

// Verify checks current code against a golden suite: the generated section must
// match byte-for-byte (no format drift), every object vector must decode and
// re-encode to its exact bytes with its recorded id (canonical-form and
// integrity), and round-trip vectors must survive unchanged (unknown-field
// preservation). It returns the list of failures (empty means conformant).
func Verify(golden Vectors) []string {
	var fails []string

	gen := Generate()
	if diff := diffGenerated(gen, golden.Generated); diff != "" {
		fails = append(fails, "generated vectors drifted from golden: "+diff)
	}

	all := append([]ObjVector{}, golden.Generated.Objects...)
	all = append(all, golden.RoundTrip...)
	for _, v := range all {
		if err := checkObjVector(v); err != nil {
			fails = append(fails, fmt.Sprintf("object %q: %v", v.Name, err))
		}
	}
	for _, v := range golden.Generated.Multihash {
		if err := checkMHVector(v); err != nil {
			fails = append(fails, fmt.Sprintf("multihash %q: %v", v.Algo, err))
		}
	}
	return fails
}

func checkObjVector(v ObjVector) error {
	raw, err := hex.DecodeString(v.Hex)
	if err != nil {
		return fmt.Errorf("bad hex: %w", err)
	}
	obj, err := object.Decode(raw)
	if err != nil {
		return fmt.Errorf("decode: %w", err)
	}
	if uint64(obj.Type()) != v.Type {
		return fmt.Errorf("type = %d, want %d", obj.Type(), v.Type)
	}
	reEnc := obj.Encode()
	if !bytes.Equal(reEnc, raw) {
		return fmt.Errorf("re-encode not byte-identical (round-trip broken)")
	}
	id, _ := multihash.Sum(multihash.BLAKE3, raw)
	if id.Hex() != v.ID {
		return fmt.Errorf("id = %s, want %s", id.Hex(), v.ID)
	}
	return nil
}

func checkMHVector(v MHVector) error {
	in, err := hex.DecodeString(v.InputHex)
	if err != nil {
		return err
	}
	mh, err := multihash.Sum(multihash.Code(v.Code), in)
	if err != nil {
		return err
	}
	if mh.Hex() != v.MH {
		return fmt.Errorf("mh = %s, want %s", mh.Hex(), v.MH)
	}
	return nil
}

func diffGenerated(got, want Generated) string {
	gj, _ := json.Marshal(got)
	wj, _ := json.Marshal(want)
	if bytes.Equal(gj, wj) {
		return ""
	}
	return fmt.Sprintf("\n got: %s\nwant: %s", gj, wj)
}

// appendUvarint mirrors the object layer's minimal varint encoding, used only
// to hand-build the unknown-type vector.
func appendUvarint(b []byte, v uint64) []byte {
	for v >= 0x80 {
		b = append(b, byte(v)|0x80)
		v >>= 7
	}
	return append(b, byte(v))
}
