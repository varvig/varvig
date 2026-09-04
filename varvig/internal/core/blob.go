package core

import (
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// PutBlob stores raw bytes as a blob and returns its content id. It is the core
// primitive behind the plumbing verbs that store content (hash-object, setting a
// policy module), so a shell need not construct or store an object itself (design
// addendum, U1).
func PutBlob(r *repo.Repo, data []byte) (multihash.Multihash, error) {
	return r.Objects.Put(object.NewBlob(data))
}

// HashBlob returns the content id bytes would have as a blob, without storing
// them — the compute-only half of hash-object.
func HashBlob(data []byte) (multihash.Multihash, error) {
	return object.NewBlob(data).ID(multihash.Default)
}
