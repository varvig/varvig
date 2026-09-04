package ticket

import "github.com/dividebyzero/claude-experiments/varvig/internal/object"

// ArtifactRef is the attached-artifact descriptor, re-exported so a caller builds
// one as `ticket.ArtifactRef` rather than naming the wire-format package (design
// addendum, U1). It is a type alias, so AttachArtifact and Artifacts accept and
// return these unchanged.
type ArtifactRef = object.ArtifactRef
