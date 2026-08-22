package object

import (
	"fmt"
	"sort"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// ArtifactRef records the identity of, and reachability to, an artifact whose
// bytes live outside the object model — a container image, an APK, an SBOM, a
// release archive (design §1.5; federation design §1). Those bytes correctly do
// not enter the content-addressed store, but GC's mark phase only walks varvig
// objects, so without a typed handle an external artifact is invisible to
// reachability: either deleted while a live change still needs it, or orphaned
// forever because nothing knew it went unreachable.
//
// An artifact-ref is that handle. A change names the artifact-refs it produced
// (Change.Artifacts); the mark phase then treats each as reachable-through, so
// the external bytes are pinned exactly as long as some reachable change refers
// to them. varvig never fetches or stores the bytes — it records identity and
// reachability only; fetching and deletion in a registry are Factory/operator
// concerns, and varvig holds no credentials there.
type ArtifactRef struct {
	// ContentHash is the multihash of the artifact bytes. It is the artifact's
	// identity: the same artifact reachable from three registries is one
	// artifact-ref with three locators, and a locator changing is not a new
	// artifact (federation design §1.3). REQUIRED.
	ContentHash multihash.Multihash
	// MediaType is an advisory content type, e.g.
	// application/vnd.oci.image.manifest.v1+json.
	MediaType string
	// Size is the artifact size in bytes.
	Size uint64
	// Locators are zero or more URIs where the bytes may be fetched. They are
	// hints, not identity; canonicalized (sorted, deduplicated) so an equal
	// locator set encodes identically.
	Locators []string
	// ProducedBy is the change (or attempt) that created the artifact, if known.
	// It is a genuine reference to a varvig object, so it participates in
	// reachability: while the artifact-ref is reachable, its producer is retained.
	ProducedBy multihash.Multihash
}

// Field tags for TypeArtifactRef. Append-only, per-type (design §4.4).
const (
	tagArtifactContentHash = 1
	tagArtifactMediaType   = 2
	tagArtifactSize        = 3
	tagArtifactLocators    = 4
	tagArtifactProducedBy  = 5
)

// NewArtifactRef builds an artifact-ref object. Locators are sorted and
// deduplicated so that the same (content_hash, locator set) always encodes to
// the same bytes and hashes identically across peers.
func NewArtifactRef(a ArtifactRef) *Object {
	locs := append([]string(nil), a.Locators...)
	sort.Strings(locs)
	locs = dedupeStrings(locs)

	fields := []field{
		{tag: tagArtifactContentHash, val: append([]byte(nil), a.ContentHash...)},
	}
	if a.MediaType != "" {
		fields = append(fields, field{tag: tagArtifactMediaType, val: []byte(a.MediaType)})
	}
	if a.Size != 0 {
		fields = append(fields, field{tag: tagArtifactSize, val: appendUvarint(nil, a.Size)})
	}
	if len(locs) > 0 {
		fields = append(fields, field{tag: tagArtifactLocators, val: encodeStringList(locs)})
	}
	if a.ProducedBy != nil {
		fields = append(fields, field{tag: tagArtifactProducedBy, val: append([]byte(nil), a.ProducedBy...)})
	}
	return newObject(TypeArtifactRef, fields)
}

// AsArtifactRef decodes the typed view of an artifact-ref object.
func (o *Object) AsArtifactRef() (ArtifactRef, error) {
	if o.typ != TypeArtifactRef {
		return ArtifactRef{}, fmt.Errorf("object: not an artifact-ref (%s)", o.typ)
	}
	var a ArtifactRef
	if v, ok := o.Field(tagArtifactContentHash); ok {
		a.ContentHash = multihash.Multihash(append([]byte(nil), v...))
	}
	if v, ok := o.Field(tagArtifactMediaType); ok {
		a.MediaType = string(v)
	}
	if v, ok := o.Field(tagArtifactSize); ok {
		n, m, err := readUvarint(v)
		if err != nil || m != len(v) {
			return ArtifactRef{}, fmt.Errorf("%w: bad artifact-ref size", ErrMalformed)
		}
		a.Size = n
	}
	if v, ok := o.Field(tagArtifactLocators); ok {
		locs, err := decodeStringList(v)
		if err != nil {
			return ArtifactRef{}, err
		}
		a.Locators = locs
	}
	if v, ok := o.Field(tagArtifactProducedBy); ok {
		a.ProducedBy = multihash.Multihash(append([]byte(nil), v...))
	}
	return a, nil
}

// dedupeStrings removes adjacent duplicates from a sorted slice.
func dedupeStrings(sorted []string) []string {
	if len(sorted) < 2 {
		return sorted
	}
	out := sorted[:1]
	for _, s := range sorted[1:] {
		if s != out[len(out)-1] {
			out = append(out, s)
		}
	}
	return out
}
