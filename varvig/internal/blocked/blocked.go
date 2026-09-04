// Package blocked records the third task outcome (build spec P1.2), the one
// beside a proposal and a failure: a task that could neither finish nor fail
// cleanly because it hit a scope boundary it has no authority to cross.
//
// The design constraint (§A2) is that hitting the boundary is not a failure and
// not a silent stop — it is a routable request. An agent that reaches the edge
// of its write set six times must not emit six failures and must not widen its
// own scope; it emits one blocked-on-scope report carrying all six encounters
// plus the concrete thing it needs, addressed to whoever holds the authority to
// widen scope. Widening is a decision with an author, recorded here as its own
// signed record — never applied automatically.
//
// It reuses the governance machinery rather than growing a parallel one: a
// report is a signed record bound to the intent revision it ran under and stored
// as a note (like an attestation), so it pins that revision, syncs like any other
// object, and routes to the authority along the same path an approval request
// does. The signature is over the record's canonical bytes, so the authority can
// see who is asking and that the ask was not tampered with.
package blocked

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dividebyzero/claude-experiments/varvig/internal/identity"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/notes"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// kindRequest and kindWidening discriminate the two record shapes that share the
// blocked namespace: a task's request for scope, and an authority's decision to
// widen it.
const (
	kindRequest  = "request"
	kindWidening = "widening"
)

// schemeEd25519 matches the signature scheme tag used by packages provenance and
// attest, so a blocked record is verified the same way a signed change is.
const schemeEd25519 uint64 = 1

var (
	// ErrUnsigned is returned when a stored record carries no signature.
	ErrUnsigned = errors.New("blocked: record is not signed")
	// ErrBadSignature is returned when a signature does not verify.
	ErrBadSignature = errors.New("blocked: signature does not verify")
	// ErrWrongKind is returned when a payload is decoded as the wrong record kind.
	ErrWrongKind = errors.New("blocked: unexpected record kind")
)

// Hit is one boundary encounter: a path or capability the task tried to touch
// and could not, with why it reached for it. The blocked-on-scope report carries
// every hit recorded across the run, so the authority sees the whole shape of
// what the scope was missing, not just the last refusal.
type Hit struct {
	Path   string `json:"path"`
	Reason string `json:"reason,omitempty"`
}

// Report is a blocked-on-scope outcome: the aggregated boundary hits plus the
// concrete thing the task needs to proceed, bound to the intent revision it ran
// under and to the scope it was originally granted.
type Report struct {
	Intent    string `json:"intent"` // the intent revision hash the task ran under
	Scope     string `json:"scope"`  // the task's original scope declaration, verbatim
	Need      string `json:"need"`   // the path or capability that must be added
	Why       string `json:"why"`    // why it is needed
	Unmet     string `json:"unmet"`  // the requirement that could not be met without it
	Hits      []Hit  `json:"hits"`   // every boundary hit recorded during the run
	Author    string `json:"author"` // the task fingerprint that authored the request
	Timestamp int64  `json:"timestamp"`
}

// Widening is an authority's decision to widen a task's scope in response to a
// report. It records both the scope the task originally declared and the scope
// it is being widened to, so a resumed task's provenance shows the original
// declaration and the widening decision side by side (build spec P1.2). It is
// never produced by the task; only by whoever holds scope authority.
type Widening struct {
	Intent    string `json:"intent"`
	FromScope string `json:"from_scope"` // the original declaration being widened
	ToScope   string `json:"to_scope"`   // the scope granted
	Decider   string `json:"decider"`    // who authored the widening
	Reason    string `json:"reason,omitempty"`
	Timestamp int64  `json:"timestamp"`
}

// envelope is the note payload: the record, its kind, and the detached signature
// over the record's canonical JSON. Storing the pubkey lets a reader verify
// without a registry; an authority that also wants to check the signer's kind
// resolves the fingerprint separately.
type envelope struct {
	Kind   string          `json:"kind"`
	Record json.RawMessage `json:"record"`
	Scheme uint64          `json:"scheme"`
	PubKey string          `json:"pubkey"`
	Sig    string          `json:"sig"`
}

func sign(signer identity.Signer, kind string, record any) ([]byte, error) {
	if signer == nil {
		return nil, errors.New("blocked: nil signer")
	}
	body, err := json.Marshal(record)
	if err != nil {
		return nil, err
	}
	sig, err := signer.Sign(canonicalBytes(kind, body))
	if err != nil {
		return nil, err
	}
	env := envelope{
		Kind:   kind,
		Record: body,
		Scheme: schemeEd25519,
		PubKey: hex.EncodeToString(signer.Public()),
		Sig:    hex.EncodeToString(sig),
	}
	return json.Marshal(env)
}

// canonicalBytes is the exact byte string the signature covers: the kind and the
// record body, length-framed so the kind cannot be confused with record content.
func canonicalBytes(kind string, body []byte) []byte {
	return []byte(fmt.Sprintf("varvig-blocked\x00%s\x00%d\x00%s", kind, len(body), body))
}

func verify(payload []byte, wantKind string, out any) (ed25519.PublicKey, error) {
	var env envelope
	if err := json.Unmarshal(payload, &env); err != nil {
		return nil, err
	}
	if env.Kind != wantKind {
		return nil, fmt.Errorf("%w: got %q want %q", ErrWrongKind, env.Kind, wantKind)
	}
	if env.Sig == "" || env.PubKey == "" {
		return nil, ErrUnsigned
	}
	if env.Scheme != schemeEd25519 {
		return nil, fmt.Errorf("blocked: unknown signature scheme %d", env.Scheme)
	}
	pub, err := hex.DecodeString(env.PubKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return nil, ErrBadSignature
	}
	sig, err := hex.DecodeString(env.Sig)
	if err != nil {
		return nil, ErrBadSignature
	}
	if !ed25519.Verify(pub, canonicalBytes(env.Kind, env.Record), sig) {
		return nil, ErrBadSignature
	}
	if err := json.Unmarshal(env.Record, out); err != nil {
		return nil, err
	}
	return ed25519.PublicKey(pub), nil
}

// Report/Widening signing and verification exposed for tests and callers that
// want the payload without storing it.

// SignReport builds the signed note payload for a report.
func SignReport(signer identity.Signer, rep Report) ([]byte, error) {
	return sign(signer, kindRequest, rep)
}

// VerifyReport decodes and verifies a report note payload.
func VerifyReport(payload []byte) (Report, ed25519.PublicKey, error) {
	var rep Report
	pub, err := verify(payload, kindRequest, &rep)
	return rep, pub, err
}

// Attach signs a report and stores it as a note bound to the intent revision it
// names, in the reserved blocked namespace. The note target is the intent hash,
// so the report pins that revision (it cannot be GC'd out from under an open
// request) and is listable by intent, routing to the authority the same way an
// attestation does.
func Attach(r *repo.Repo, signer identity.Signer, rep Report) (multihash.Multihash, error) {
	intent, err := multihash.ParseHex(rep.Intent)
	if err != nil {
		return nil, fmt.Errorf("blocked: bad intent hash: %w", err)
	}
	payload, err := SignReport(signer, rep)
	if err != nil {
		return nil, err
	}
	return notes.New(r).Add(reserved.NoteBlocked, intent, payload, rep.Author, rep.Timestamp)
}

// List returns the verified blocked-on-scope reports bound to intent, newest
// first. A record whose signature does not verify is skipped — the note payload
// is not authority, the signature is.
func List(r *repo.Repo, intent multihash.Multihash) ([]Report, error) {
	chain, err := notes.New(r).List(reserved.NoteBlocked, intent)
	if err != nil {
		return nil, err
	}
	var out []Report
	for _, n := range chain {
		rep, _, err := VerifyReport(n.Note.Payload)
		if err != nil {
			continue
		}
		out = append(out, rep)
	}
	return out, nil
}

// Widen signs a widening decision and stores it as a note bound to the same
// intent revision, so the request and the decision that answered it live
// together and route the same way. It does not change any grant — applying the
// widened scope is the caller's next, separate step, minting a fresh task.
func Widen(r *repo.Repo, signer identity.Signer, w Widening) (multihash.Multihash, error) {
	intent, err := multihash.ParseHex(w.Intent)
	if err != nil {
		return nil, fmt.Errorf("blocked: bad intent hash: %w", err)
	}
	payload, err := sign(signer, kindWidening, w)
	if err != nil {
		return nil, err
	}
	return notes.New(r).Add(reserved.NoteBlocked, intent, payload, w.Decider, w.Timestamp)
}

// Widenings returns the verified widening decisions bound to intent, newest
// first.
func Widenings(r *repo.Repo, intent multihash.Multihash) ([]Widening, error) {
	chain, err := notes.New(r).List(reserved.NoteBlocked, intent)
	if err != nil {
		return nil, err
	}
	var out []Widening
	for _, n := range chain {
		var w Widening
		if _, err := verify(n.Note.Payload, kindWidening, &w); err != nil {
			continue
		}
		out = append(out, w)
	}
	return out, nil
}

// Trace is the provenance view a resumed task carries (build spec P1.2): every
// blocked-on-scope request against an intent revision, and every widening
// decision that answered one — the original scope declarations and the widening
// decisions side by side, so resuming under a widened scope shows both.
type Trace struct {
	Requests  []Report
	Widenings []Widening
}

// Provenance assembles the request/widening trace for an intent revision.
func Provenance(r *repo.Repo, intent multihash.Multihash) (Trace, error) {
	reqs, err := List(r, intent)
	if err != nil {
		return Trace{}, err
	}
	wids, err := Widenings(r, intent)
	if err != nil {
		return Trace{}, err
	}
	return Trace{Requests: reqs, Widenings: wids}, nil
}
