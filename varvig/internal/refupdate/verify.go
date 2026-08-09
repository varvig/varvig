package refupdate

import (
	"fmt"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
)

// ObjectPresence reports whether an object is present locally. The store
// guarantees any object it holds verifies against its identity on read, so
// presence is sufficient for step 6.
type ObjectPresence interface {
	Has(multihash.Multihash) bool
}

// RefStore is the ref surface verification drives: read the current value,
// attempt the atomic compare-and-swap, and record audit entries. *refs.Store
// satisfies it.
type RefStore interface {
	Resolve(name string) (multihash.Multihash, error)
	CompareAndSwap(name string, old, newval multihash.Multihash, actor, msg string) error
	AppendLog(name string, old, newval multihash.Multihash, actor, msg string) error
}

// DefaultSkew is the clock-skew tolerance for not_after. The auth design asks
// for a small window rather than requiring NTP (§12, "clock skew").
const DefaultSkew = 5 * time.Minute

// Verifier applies the signed-ref-update pipeline (auth design §5.2).
type Verifier struct {
	Trust   *trust.File
	Objects ObjectPresence
	Refs    RefStore
	Replay  ReplayGuard

	// Now returns the current unix time in seconds; nil defaults to time.Now.
	Now func() int64
	// Skew is the allowed clock skew; zero defaults to DefaultSkew.
	Skew time.Duration
}

// Result is the outcome of verification. Accepted reports whether the ref moved;
// Reason explains a rejection. Current carries the ref's present value after a
// CAS conflict so the caller can rebase and retry (§5.2 step 7).
type Result struct {
	Accepted bool
	Reason   string
	Current  multihash.Multihash
}

func rejected(reason string) *Result { return &Result{Accepted: false, Reason: reason} }
func accepted() *Result              { return &Result{Accepted: true} }

// Verify runs the pipeline against su and, on success, performs the ref update.
// It returns a Result for policy outcomes and a non-nil error only for
// infrastructure failures (I/O, lock timeouts).
//
// Order note: the auth design lists nonce and trust checks ahead of signature
// verification, but this implementation verifies the signature first and defers
// every side effect (recording the nonce, the CAS) to the end. Doing so means
// an unauthenticated blob never consumes a nonce or pollutes the audit log —
// the checks are otherwise independent, so reordering the pure ones is safe.
func (v *Verifier) Verify(su *SignedUpdate) (*Result, error) {
	now := time.Now().Unix()
	if v.Now != nil {
		now = v.Now()
	}
	skew := v.Skew
	if skew == 0 {
		skew = DefaultSkew
	}
	p := su.Payload
	fp := p.Fingerprint()
	ref := p.Ref()

	// (1) Signature — verified first, before any side effect. An update whose
	// signature does not verify is not an authenticated request at all and is
	// rejected without touching the audit log.
	if err := su.VerifySignature(); err != nil {
		return rejected("signature does not verify"), nil
	}

	// From here the update is authenticated: every outcome is auditable.
	audit := func(reason string) {
		cur, _ := v.Refs.Resolve(ref)
		_ = v.Refs.AppendLog(ref, cur, cur, fp, "refupdate rejected: "+reason)
	}

	// (2) Expiry, with skew tolerance.
	if na := p.NotAfter(); na != 0 && now > na+int64(skew.Seconds()) {
		audit("expired")
		return rejected(fmt.Sprintf("expired at %d (now %d)", na, now)), nil
	}

	// (3) Authority: the signer must hold `promote` at a scope covering the
	// update's declared scope (§5.2 step 4). The trust store is consulted as it
	// stands for the caller; verification against a specific ref state is the
	// caller's responsibility when it loads the store.
	if !v.Trust.Authorized(fp, trust.RightPromote, p.Scope()) {
		audit("not authorized to promote at scope " + p.Scope())
		return rejected("signer lacks promote at scope " + p.Scope()), nil
	}

	// (4) Referenced objects must be present locally (§5.2 step 6). Checked
	// before the nonce is consumed so a not-yet-transferred closure does not
	// burn a single-use nonce.
	if newv := p.New(); newv != nil && !v.Objects.Has(newv) {
		audit("missing new object")
		return rejected("new object not present: " + newv.Hex()), nil
	}
	evidence, err := p.Evidence()
	if err != nil {
		return nil, err
	}
	for _, e := range evidence {
		if !v.Objects.Has(e) {
			audit("missing evidence object")
			return rejected("evidence object not present: " + e.Hex()), nil
		}
	}

	// (5) Replay: consume the nonce, keyed by (signer, ref) (§5.2 step 3).
	seen, err := v.Replay.Check(fp, ref, p.Nonce(), p.NotAfter(), now)
	if err != nil {
		return nil, err
	}
	if seen {
		audit("nonce replay")
		return rejected("nonce already used for this signer and ref"), nil
	}

	// (6) Atomic compare-and-swap (§5.2 step 7). Success appends its own reflog
	// entry; a conflict returns the current head so the caller can rebase.
	msg := fmt.Sprintf("promote by %s", fp)
	if err := v.Refs.CompareAndSwap(ref, p.ExpectedOld(), p.New(), fp, msg); err != nil {
		cur, _ := v.Refs.Resolve(ref)
		audit("CAS conflict")
		return &Result{Accepted: false, Reason: "compare-and-swap conflict", Current: cur}, nil
	}
	return accepted(), nil
}
