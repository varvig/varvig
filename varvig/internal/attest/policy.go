package attest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/hook"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// ErrPolicyRefused reports that a promotion-policy wasm module refused a change.
var ErrPolicyRefused = errors.New("attest: promotion refused by policy module")

// Policy is the promotion checkpoint interface (tickets M1). It is identical in
// method set to spec.PromotionPolicy, so any Policy value can be handed to
// spec.PromoteWithPolicy without attest importing the speculation store.
type Policy interface {
	Admit(r *repo.Repo, change multihash.Multihash) error
}

// AllOf composes policies into one that admits a change only if every member
// admits it — the way §3.3 Stage 2 constraints stack: each is a separate rule,
// and any one refusing is decisive. Members are consulted in order and the
// first refusal is returned.
func AllOf(policies ...Policy) Policy { return allOf(policies) }

type allOf []Policy

func (a allOf) Admit(r *repo.Repo, change multihash.Multihash) error {
	for _, p := range a {
		if p == nil {
			continue
		}
		if err := p.Admit(r, change); err != nil {
			return err
		}
	}
	return nil
}

// PolicyInput is the governance context the host computes for a change and
// passes to a policy module on stdin (tickets §2.5). It is a plain JSON object
// so a module in any WASI language can read it, and it is forward-compatible:
// new fields (affected paths, evidence, cost estimates, signer kind) are added
// over time and a module ignores the ones it does not use.
type PolicyInput struct {
	Change       string          `json:"change"`
	Materialized bool            `json:"materialized"`
	Author       string          `json:"author,omitempty"`
	Message      string          `json:"message,omitempty"`
	Vetoed       bool            `json:"vetoed"`
	VetoedAt     string          `json:"vetoed_at,omitempty"`
	Attestations []PolicyAttInfo `json:"attestations"`
}

// PolicyAttInfo is one attestation as seen by a policy module.
type PolicyAttInfo struct {
	Decision string `json:"decision"`
	Strength string `json:"strength"`
	Signer   string `json:"signer"`
}

// BuildPolicyInput gathers the governance facts a policy module needs about a
// change: its metadata, whether its ancestry carries a veto, and every
// verified attestation bound to it. Only signature-verified attestations are
// included — a policy decides on authenticated decisions, never raw payloads.
func BuildPolicyInput(r *repo.Repo, change multihash.Multihash) (PolicyInput, error) {
	in := PolicyInput{Change: change.Hex()}

	if obj, err := r.Objects.Get(change); err == nil && obj.Type() == object.TypeChange {
		if c, err := obj.AsChange(); err == nil {
			in.Materialized = c.Materialized()
			in.Author = c.Author
			in.Message = c.Message
		}
	}

	blocked, at, err := PromotionBlocked(r, change)
	if err != nil {
		return PolicyInput{}, err
	}
	in.Vetoed = blocked
	if blocked && at != nil {
		in.VetoedAt = at.Hex()
	}

	entries, err := List(r, change)
	if err != nil {
		return PolicyInput{}, err
	}
	in.Attestations = make([]PolicyAttInfo, 0, len(entries))
	for _, e := range entries {
		in.Attestations = append(in.Attestations, PolicyAttInfo{
			Decision: e.Attestation.Decision.String(),
			Strength: e.Attestation.Strength.String(),
			Signer:   Fingerprint(e.SignerKey),
		})
	}
	return in, nil
}

// WasmPolicy is a promotion policy backed by a content-addressed wasm module
// (tickets §2.5). The host computes a PolicyInput and passes it to the module
// on stdin; the module exits 0 to admit and nonzero to refuse, optionally
// writing a reason. The module runs in the same closed WASI sandbox as hooks —
// no filesystem, network, environment, or unbounded clock — so policy is
// portable, versioned alongside the code, and cannot reach outside its inputs.
//
// This is the context-passing form: the host gathers the facts. Exposing live
// host functions to the module (verify a signature, query the affected-set
// index) is the M3/M4 refinement and layers on without changing this shape.
type WasmPolicy struct {
	Module  []byte
	Timeout time.Duration
}

// Admit runs the policy module against the change's governance context.
func (w WasmPolicy) Admit(r *repo.Repo, change multihash.Multihash) error {
	in, err := BuildPolicyInput(r, change)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(in)
	if err != nil {
		return err
	}
	timeout := w.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	res, err := hook.Run(ctx, w.Module, payload)
	if err != nil {
		return fmt.Errorf("attest: policy module failed to run: %w", err)
	}
	if res.Allowed() {
		return nil
	}
	reason := strings.TrimSpace(string(res.Stderr))
	if reason == "" {
		reason = strings.TrimSpace(string(res.Stdout))
	}
	if reason == "" {
		reason = fmt.Sprintf("exit %d", res.ExitCode)
	}
	return fmt.Errorf("%w: %s", ErrPolicyRefused, reason)
}

// LoadPolicy resolves the repository's promotion-policy module (refs/varvig/policy).
// It returns ok=false when no policy is configured, so promotion falls back to
// the built-in constraints alone.
func LoadPolicy(r *repo.Repo) (WasmPolicy, bool, error) {
	id, err := r.Refs.Resolve(reserved.PolicyRef)
	if err != nil {
		return WasmPolicy{}, false, nil // no policy configured
	}
	obj, err := r.Objects.Get(id)
	if err != nil {
		return WasmPolicy{}, false, err
	}
	mod, ok := obj.BlobContent()
	if !ok {
		return WasmPolicy{}, false, fmt.Errorf("attest: policy ref %s is not a blob", reserved.PolicyRef)
	}
	return WasmPolicy{Module: append([]byte(nil), mod...)}, true, nil
}
