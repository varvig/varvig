package readapi

import (
	"path/filepath"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/ticket"
)

func TestTicketArtifactsView(t *testing.T) {
	r, err := repo.Init(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(o *object.Object) multihash.Multihash {
		id, err := r.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	sum := func(s string) multihash.Multihash {
		h, err := multihash.Sum(multihash.SHA2_256, []byte(s))
		if err != nil {
			t.Fatal(err)
		}
		return h
	}

	producer := put(object.NewChange(object.Change{Tree: nil, Message: "build", Author: "ci", Timestamp: 1}))
	art := put(object.NewArtifactRef(object.ArtifactRef{
		ContentHash: sum("image"),
		MediaType:   "application/vnd.oci.image.manifest.v1+json",
		Size:        2048,
		Locators:    []string{"oci://reg.example/app@sha256:deadbeef"},
		ProducedBy:  producer,
	}))
	head := put(object.NewChange(object.Change{
		Message: "ship", Author: "ci", Timestamp: 2, Artifacts: []multihash.Multihash{art},
	}))
	if err := r.Refs.Create(ticket.Ref(head), head, "ci", "ticket new"); err != nil {
		t.Fatal(err)
	}

	views, err := New(r).TicketArtifacts(head)
	if err != nil {
		t.Fatalf("TicketArtifacts: %v", err)
	}
	if len(views) != 1 {
		t.Fatalf("want 1 view, got %d", len(views))
	}
	v := views[0]
	if v.ContentHash != sum("image").Hex() {
		t.Errorf("content_hash = %q", v.ContentHash)
	}
	if v.MediaType != "application/vnd.oci.image.manifest.v1+json" || v.Size != 2048 {
		t.Errorf("media/size = %q/%d", v.MediaType, v.Size)
	}
	if len(v.Locators) != 1 || v.Locators[0] != "oci://reg.example/app@sha256:deadbeef" {
		t.Errorf("locators = %v", v.Locators)
	}
	if v.ProducedBy != producer.Hex() {
		t.Errorf("produced_by = %q, want %q", v.ProducedBy, producer.Hex())
	}
}
