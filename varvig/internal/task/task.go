// Package task implements task credentials for agents (auth design §6). Per
// task, an ephemeral Ed25519 keypair is minted *inside the sandbox*: the private
// key is held only in memory and never touches disk (§6.1). The key is then
// granted a scoped, expiring right.
//
// The three defining properties (§6.2):
//
//   - Scope equals the read set. A grant's Scope is simultaneously the sparse
//     checkout, the API's visibility, and the capability boundary — one thing
//     expressed three ways (design §1.4).
//   - Propose-only. A task key can create objects and propose a speculative
//     state; it can never move a ref. Promotion is a separate, human-gated step.
//   - Expiry does the revocation work. A short TTL means the common case needs
//     no revocation infrastructure; the grant simply stops being valid.
//
// At v1 the grant registry is an in-memory Table held by the local daemon
// (§6.1). When tasks need to propose to *remote* peers, a grant is promoted to a
// short-lived certificate signed by a human key — additive, and nothing here
// changes below it.
package task

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
	"github.com/dividebyzero/claude-experiments/varvig/internal/trust"
)

// Grant is a scoped, expiring capability for one task. The ephemeral private key
// lives only in this process; it is never serialized, so there is no long-lived
// credential to leak and cleanup is "the process ends" (§6.2).
type Grant struct {
	// ID is a short handle for the task (the low bytes of the fingerprint),
	// used to name the per-task socket and to look the grant up in the Table.
	ID string
	// Scope is the path prefix the task may read and propose within — the read
	// set (§6.2). "/" is the whole repo; a narrower scope is a path prefix.
	Scope trust.Scope
	// ProposeOnly is always true at v1: a task key may propose, never promote
	// (§6.2). The field is explicit so the invariant is visible at every call
	// site rather than assumed.
	ProposeOnly bool
	// NotAfter is the expiry in unix seconds. Expiry does the revocation work.
	NotAfter int64

	pub  sshkey.PublicKey
	priv ed25519.PrivateKey // in-memory only; never persisted

	// Reads accumulates every hash the task resolves through the gate, so the
	// gate can fold the read set into the change's provenance (§8.2).
	Reads *ReadSet
}

// New mints a fresh task grant: an ephemeral key from the OS CSPRNG, scoped to
// scope, expiring ttl from now. proposeOnly must be true at v1; a false value is
// rejected rather than silently granting more than the design allows.
func New(scope string, proposeOnly bool, ttl time.Duration, now time.Time) (*Grant, error) {
	if !proposeOnly {
		return nil, errors.New("task: v1 grants are propose-only; a task key can never promote")
	}
	if ttl <= 0 {
		return nil, errors.New("task: ttl must be positive")
	}
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	pk := sshkey.PublicKey{Key: pub, Comment: "task"}
	return &Grant{
		ID:          shortID(pub),
		Scope:       trust.NormalizeScope(scope),
		ProposeOnly: proposeOnly,
		NotAfter:    now.Add(ttl).Unix(),
		pub:         pk,
		priv:        priv,
		Reads:       NewReadSet(),
	}, nil
}

// Fingerprint returns the task key's standard SSH SHA256 fingerprint — the same
// identity form the trust store and `whoami` speak (§2.2).
func (g *Grant) Fingerprint() string { return g.pub.Fingerprint() }

// PublicKey returns the grant's ephemeral public key.
func (g *Grant) PublicKey() sshkey.PublicKey { return g.pub }

// Signer produces signatures with the ephemeral key. It exists so a proposal can
// be attributed to the task key, and so the short-lived-certificate upgrade
// (§6.1, remote propose) has a signer to certify — additive, no format change.
func (g *Grant) Signer() identity.Signer { return identity.FromPrivateKey(g.priv) }

// PrivateKey returns the ephemeral signing key for in-process use by the gate
// (e.g. signing a proposed change). The key is handed out only within the
// sandbox process; it is never serialized to disk and never leaves the machine,
// which is the property that matters (§6.1).
func (g *Grant) PrivateKey() ed25519.PrivateKey { return g.priv }

// Valid reports whether the grant has not yet expired at now.
func (g *Grant) Valid(now time.Time) bool { return now.Unix() <= g.NotAfter }

// Covers reports whether path falls within the grant's scope — the single check
// the gate makes on every read and every proposed path (§8.1, "the capability is
// the read set"). The root scope covers everything.
func (g *Grant) Covers(path string) bool { return g.Scope.Covers(path) }

// shortID derives a stable short handle from the public key: the first four
// bytes of the raw key, hex-encoded (eight hex chars). It is used only for
// naming and lookup, never for authorization — the fingerprint is the identity.
func shortID(pub ed25519.PublicKey) string {
	n := 4
	if len(pub) < n {
		n = len(pub)
	}
	return hex.EncodeToString(pub[:n])
}

// Table is the in-memory grant registry the local daemon holds (§6.1). It is
// safe for concurrent use. There is no persistence: grants live and die with the
// process, which is the point — no credential store to leak.
type Table struct {
	mu     sync.Mutex
	grants map[string]*Grant
}

// NewTable returns an empty grant table.
func NewTable() *Table { return &Table{grants: map[string]*Grant{}} }

// Add registers a grant under its ID.
func (t *Table) Add(g *Grant) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.grants[g.ID] = g
}

// Get returns the grant with id if it is present and still valid at now. An
// expired grant is dropped and reported as absent — expiry does the revocation.
func (t *Table) Get(id string, now time.Time) (*Grant, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	g, ok := t.grants[id]
	if !ok {
		return nil, false
	}
	if !g.Valid(now) {
		delete(t.grants, id)
		return nil, false
	}
	return g, true
}

// Remove drops a grant by id, reporting whether it was present. It is how a
// daemon revokes a task early (before expiry) — closing its socket and forgetting
// its key.
func (t *Table) Remove(id string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if _, ok := t.grants[id]; !ok {
		return false
	}
	delete(t.grants, id)
	return true
}

// Active returns every still-valid grant, pruning expired ones as it goes.
func (t *Table) Active(now time.Time) []*Grant {
	t.mu.Lock()
	defer t.mu.Unlock()
	var out []*Grant
	for id, g := range t.grants {
		if g.Valid(now) {
			out = append(out, g)
		} else {
			delete(t.grants, id)
		}
	}
	return out
}
