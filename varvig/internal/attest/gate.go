package attest

import (
	"errors"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// ErrVetoed reports that a change is unpromotable because it, or an ancestor,
// carries a veto (tickets §2.3).
var ErrVetoed = errors.New("attest: promotion disqualified by a veto")

// ErrNotApproved reports that a change lacks an approval of sufficient strength
// (tickets §2.2, §3.3, the constraint "nothing promotes without approval").
var ErrNotApproved = errors.New("attest: promotion lacks a sufficient approval")

// VetoGate is the veto half of the promotion checkpoint (tickets M1): it
// disqualifies any change whose ancestry carries a veto. It structurally
// satisfies spec.PromotionPolicy, so it can be handed to spec.PromoteWithPolicy
// without the speculation store depending on governance.
type VetoGate struct{}

// Admit disqualifies change if it or any ancestor revision carries a veto.
func (VetoGate) Admit(r *repo.Repo, change multihash.Multihash) error {
	blocked, at, err := PromotionBlocked(r, change)
	if err != nil {
		return err
	}
	if blocked {
		return fmt.Errorf("%w: ancestor %s", ErrVetoed, at.Hex())
	}
	return nil
}

// ApprovalGate is the approval half of the promotion checkpoint: it requires an
// approval of at least Required strength on the change itself, and — like every
// gate — also enforces the veto rule over the ancestry. A director sets
// Required (e.g. strong for anything touching payments, tickets §3.3); the zero
// value requires only that no ancestor is vetoed.
type ApprovalGate struct {
	Required object.Strength
}

// Admit disqualifies change unless its ancestry is veto-free and, when a
// strength is required, the change itself derives to approved at that strength.
func (g ApprovalGate) Admit(r *repo.Repo, change multihash.Multihash) error {
	if err := (VetoGate{}).Admit(r, change); err != nil {
		return err
	}
	if g.Required == object.StrengthUnknown {
		return nil
	}
	atts, err := Attestations(r, change)
	if err != nil {
		return err
	}
	if Derive(atts, g.Required) != StatusApproved {
		return fmt.Errorf("%w: need %s", ErrNotApproved, g.Required)
	}
	return nil
}
