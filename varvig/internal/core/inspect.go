package core

import (
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
)

// ErrUnmaterialized is re-exported so a shell can recognize an unmaterialized
// change (a ticket intent with no tree) without importing the object package
// (design addendum, U1).
var ErrUnmaterialized = object.ErrUnmaterialized

// IsChange reports whether a stored object is a change. It lets a shell branch on
// object kind without naming the object type constants.
func IsChange(o *object.Object) bool { return o.Type() == object.TypeChange }

// ProvenanceSummary renders a one-line provenance summary — authority, model,
// tool hash — the form `verify` prints beside each signed change.
func ProvenanceSummary(p object.Provenance) string {
	var parts []string
	if p.Authority != "" {
		parts = append(parts, "authority="+p.Authority)
	}
	if p.Model != "" {
		m := p.Model
		if p.ModelVersion != "" {
			m += "@" + p.ModelVersion
		}
		parts = append(parts, "model="+m)
	}
	if p.ToolHash != nil {
		parts = append(parts, "tool="+p.ToolHash.Hex()[4:16])
	}
	return strings.Join(parts, " ")
}
