package core

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// The task marker is the *checkout-side* counterpart to the scheduler's task
// record (taskrec.go). The record lives in the source repository, written as the
// operator, and is trusted; the marker lives inside the sandboxed checkout and is
// only a hint — it tells the CLI verbs running there that they are inside a task
// checkout, what scope the task was granted, and what its base was, so a `commit`
// or `propose` run in the checkout stamps the change with the task's scope
// (design addendum, F4: "commit ... produces ordinary change objects with task
// provenance attached by the core"). Because it is only a hint, nothing trusts
// it: at promotion the change's self-description is re-verified against the
// source-side record (VerifyTaskScope), not against this file.
//
// Sealing a checkout also seeds its signing identity with the task key, so a
// commit made in the checkout is authored *as the task* — its provenance
// authority is the task's fingerprint, which is exactly what the source-side
// record is keyed by. That is what makes an in-checkout commit carry full task
// provenance rather than the identity of whoever's shell happens to run there.

const taskMarkerFile = "task.json"

// TaskMarker is the checkout-local description of the task a checkout serves.
type TaskMarker struct {
	Fingerprint string `json:"fingerprint"` // the task key's fingerprint (== the change authority commits here will carry)
	Scope       string `json:"scope"`       // the scope the task was granted, verbatim
	Base        string `json:"base"`        // the base the checkout was provisioned from (hex, may be empty)
}

func taskMarkerPath(r *repo.Repo) string { return filepath.Join(r.GitDir(), taskMarkerFile) }

// WriteTaskMarker records the marker inside a checkout's metadata dir.
func WriteTaskMarker(r *repo.Repo, m TaskMarker) error {
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(taskMarkerPath(r), b, 0o600)
}

// ReadTaskMarker returns the checkout's task marker, if it is a task checkout. A
// plain repository (no marker) reports absent, and the CLI verbs then behave as
// they always have — this is what keeps `commit`/`propose` unchanged outside a
// checkout.
func ReadTaskMarker(r *repo.Repo) (TaskMarker, bool, error) {
	b, err := os.ReadFile(taskMarkerPath(r))
	if err != nil {
		if os.IsNotExist(err) {
			return TaskMarker{}, false, nil
		}
		return TaskMarker{}, false, err
	}
	var m TaskMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return TaskMarker{}, false, fmt.Errorf("core: corrupt task marker: %w", err)
	}
	return m, true, nil
}

// SealTaskCheckout turns a freshly provisioned checkout into a task checkout: it
// records the marker and seeds the checkout's signing identity with the task key,
// so a `commit` run inside the checkout is authored as the task (its provenance
// authority is the task's fingerprint) and carries the task's scope. It is called
// by `task start` once, as the operator, right after ProvisionCheckout — the
// point at which the task key is in hand and the checkout has not yet run any
// verb of its own (design addendum, F4).
func SealTaskCheckout(dst *repo.Repo, m TaskMarker, taskKey ed25519.PrivateKey) error {
	if err := WriteTaskMarker(dst, m); err != nil {
		return fmt.Errorf("core: write task marker: %w", err)
	}
	if taskKey != nil {
		if err := seedIdentity(dst, taskKey); err != nil {
			return fmt.Errorf("core: seed task identity: %w", err)
		}
	}
	return nil
}

// seedIdentity writes the given key as the checkout's persistent signing identity,
// in the same on-disk form provenance.LoadOrCreateIdentity reads (a raw 32-byte
// Ed25519 seed under <gitDir>/identity), so a commit run in the checkout loads
// this key rather than minting a fresh one of its own.
func seedIdentity(dst *repo.Repo, key ed25519.PrivateKey) error {
	dir := filepath.Join(dst.GitDir(), "identity")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, "ed25519.seed"), key.Seed(), 0o600)
}
