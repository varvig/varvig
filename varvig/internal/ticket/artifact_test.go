package ticket

import (
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

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

// TestAttachAndRead: an attached artifact is readable back with identity, media
// type, size and locators intact, and the artifact-ref object is stored.
func TestAttachAndRead(t *testing.T) {
	r := newRepo(t)
	id, err := New(r, "ship it", key(t), "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	artID, err := AttachArtifact(r, id, object.ArtifactRef{
		ContentHash: hashOf(t, "image-bytes"),
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		Size:        4096,
		Locators:    []string{"oci://registry.example/app@sha256:abc"},
	}, "ci", 2)
	if err != nil {
		t.Fatalf("AttachArtifact: %v", err)
	}
	if !r.Objects.Has(artID) {
		t.Fatalf("artifact-ref object %s not stored", artID.Hex())
	}
	arts, err := Artifacts(r, id)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 1 {
		t.Fatalf("want 1 artifact, got %d", len(arts))
	}
	a := arts[0]
	if !a.ContentHash.Equal(hashOf(t, "image-bytes")) || a.Size != 4096 ||
		a.MediaType != "application/vnd.oci.image.manifest.v1+json" ||
		len(a.Locators) != 1 || a.Locators[0] != "oci://registry.example/app@sha256:abc" {
		t.Fatalf("artifact not round-tripped: %+v", a)
	}
}

// TestAttachManyDeduped: attaching several artifacts lists them all, and the
// same artifact-ref attached twice is deduped.
func TestAttachManyDeduped(t *testing.T) {
	r := newRepo(t)
	id, err := New(r, "ship it", key(t), "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	img := object.ArtifactRef{ContentHash: hashOf(t, "img"), MediaType: "application/vnd.oci.image.manifest.v1+json"}
	sbom := object.ArtifactRef{ContentHash: hashOf(t, "sbom"), MediaType: "application/spdx+json"}
	if _, err := AttachArtifact(r, id, img, "ci", 2); err != nil {
		t.Fatal(err)
	}
	if _, err := AttachArtifact(r, id, sbom, "ci", 3); err != nil {
		t.Fatal(err)
	}
	if _, err := AttachArtifact(r, id, img, "ci", 4); err != nil { // identical -> same object id
		t.Fatal(err)
	}
	arts, err := Artifacts(r, id)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("want 2 distinct artifacts after a duplicate attach, got %d", len(arts))
	}
}

// TestAttachDoesNotTouchIntentChain: attaching an artifact must not move the
// ticket head — approvals bind to the head hash (§2.2), so a moved head would
// silently un-approve the ticket. This is the whole reason attach uses notes.
func TestAttachDoesNotTouchIntentChain(t *testing.T) {
	r := newRepo(t)
	id, err := New(r, "ship it", key(t), "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	before, err := Head(r, id)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if _, err := AttachArtifact(r, id, object.ArtifactRef{ContentHash: hashOf(t, "x")}, "ci", 2); err != nil {
		t.Fatalf("AttachArtifact: %v", err)
	}
	after, err := Head(r, id)
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if !before.Equal(after) {
		t.Fatalf("attach moved the head: %s -> %s", before.Hex(), after.Hex())
	}
}

// TestAttachRequiresContentHashAndTicket: guardrails.
func TestAttachRequiresContentHashAndTicket(t *testing.T) {
	r := newRepo(t)
	id, err := New(r, "ship it", key(t), "director", 1)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := AttachArtifact(r, id, object.ArtifactRef{}, "ci", 2); err == nil {
		t.Error("attach accepted an empty content hash")
	}
	if _, err := AttachArtifact(r, hashOf(t, "not-a-ticket"), object.ArtifactRef{ContentHash: hashOf(t, "x")}, "ci", 2); err == nil {
		t.Error("attach accepted a nonexistent ticket")
	}
}

// TestArtifactsIncludeHeadChangeRefs: the read still unions artifacts named on
// the head change (the materialized-change path), alongside attached ones.
func TestArtifactsIncludeHeadChangeRefs(t *testing.T) {
	r := newRepo(t)
	art := putRef(t, r, object.ArtifactRef{ContentHash: hashOf(t, "built"), MediaType: "application/octet-stream"})
	changeID, err := r.Objects.Put(object.NewChange(object.Change{
		Message: "materialized", Author: "ci", Timestamp: 1, Artifacts: []multihash.Multihash{art},
	}))
	if err != nil {
		t.Fatalf("put change: %v", err)
	}
	if err := r.Refs.Create(Ref(changeID), changeID, "ci", "ticket new"); err != nil {
		t.Fatalf("create ticket ref: %v", err)
	}
	// Also attach one via notes.
	if _, err := AttachArtifact(r, changeID, object.ArtifactRef{ContentHash: hashOf(t, "attached")}, "ci", 2); err != nil {
		t.Fatalf("AttachArtifact: %v", err)
	}
	arts, err := Artifacts(r, changeID)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}
	if len(arts) != 2 {
		t.Fatalf("want 2 (one change-named, one attached), got %d", len(arts))
	}
}

// TestArtifactsMissingObjectIsError: a named artifact-ref that isn't present is a
// loud error, not a silent drop.
func TestArtifactsMissingObjectIsError(t *testing.T) {
	r := newRepo(t)
	changeID, err := r.Objects.Put(object.NewChange(object.Change{
		Message: "ship it", Author: "ci", Timestamp: 1,
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

func putRef(t *testing.T, r *repo.Repo, a object.ArtifactRef) multihash.Multihash {
	t.Helper()
	id, err := r.Objects.Put(object.NewArtifactRef(a))
	if err != nil {
		t.Fatalf("put artifact-ref: %v", err)
	}
	return id
}
