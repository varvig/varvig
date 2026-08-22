package ticket

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// putArtifactRef stores an artifact-ref object and returns its id.
func putArtifactRef(t *testing.T, r *repo.Repo, a object.ArtifactRef) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewArtifactRef(a))
	if err != nil {
		t.Fatalf("put artifact-ref: %v", err)
	}
	return id
}

func hashOf(t *testing.T, s string) multihash.Multihash {
	t.Helper()
	h, err := multihash.Sum(multihash.SHA2_256, []byte(s))
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	return h
}

// TestArtifactsEmptyByDefault: a plain ticket names no artifacts.
func TestArtifactsEmptyByDefault(t *testing.T) {
	r := newRepo(t)
	id, err := New(r, "add rate limiting", key(t), "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	arts, err := Artifacts(r, id)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 0 {
		t.Fatalf("a fresh ticket should name no artifacts, got %d", len(arts))
	}
}

// TestArtifactsResolvesHeadRefs: a ticket whose head change names artifact-refs
// resolves each to its ArtifactRef, with identity/media type/size/locators intact.
func TestArtifactsResolvesHeadRefs(t *testing.T) {
	r := newRepo(t)

	img := putArtifactRef(t, r, object.ArtifactRef{
		ContentHash: hashOf(t, "image-bytes"),
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		Size:        4096,
		Locators:    []string{"oci://registry.example/app@sha256:abc"},
	})
	sbom := putArtifactRef(t, r, object.ArtifactRef{
		ContentHash: hashOf(t, "sbom-bytes"),
		MediaType:   "application/spdx+json",
	})

	// Craft a ticket head change that names both artifact-refs, and a ref for it.
	changeID, err := r.Objects.Put(object.NewChange(object.Change{
		Message:   "ship it",
		Timestamp: 1,
		Author:    "ci",
		Artifacts: []multihash.Multihash{img, sbom},
	}))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	if err := r.Refs.Create(Ref(changeID), changeID, "ci", "ticket new"); err != nil {
		t.Fatalf("create ticket ref: %v", err)
	}

	arts, err := Artifacts(r, changeID)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("want 2 artifact-refs, got %d", len(arts))
	}
	// Order is the change's canonical (sorted-by-id) order; find each by media type.
	byType := map[string]object.ArtifactRef{}
	for _, a := range arts {
		byType[a.MediaType] = a
	}
	oci := byType["application/vnd.oci.image.manifest.v1+json"]
	if !oci.ContentHash.Equal(hashOf(t, "image-bytes")) || oci.Size != 4096 ||
		len(oci.Locators) != 1 || oci.Locators[0] != "oci://registry.example/app@sha256:abc" {
		t.Fatalf("oci artifact-ref not round-tripped: %+v", oci)
	}
	if _, ok := byType["application/spdx+json"]; !ok {
		t.Fatalf("sbom artifact-ref missing: %+v", arts)
	}
}

// TestArtifactsMissingObjectIsError: a named artifact-ref that isn't present is a
// loud error, not a silent drop.
func TestArtifactsMissingObjectIsError(t *testing.T) {
	r := newRepo(t)
	changeID, err := r.Objects.Put(object.NewChange(object.Change{
		Message:   "ship it",
		Timestamp: 1,
		Author:    "ci",
		Artifacts: []multihash.Multihash{hashOf(t, "never-stored")},
	}))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	if err := r.Refs.Create(Ref(changeID), changeID, "ci", "ticket new"); err != nil {
		t.Fatalf("create ticket ref: %v", err)
	}
	if _, err := Artifacts(r, changeID); err == nil {
		t.Fatal("expected an error for a missing artifact-ref object")
	}
}
