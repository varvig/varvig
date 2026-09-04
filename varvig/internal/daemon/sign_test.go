package daemon

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"path/filepath"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/core"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

// TestRemoteSignerAuthorsCheckoutCommitAsTask is the daemon-path half of F4's
// "a commit in a task checkout carries full task provenance": the task key never
// leaves the daemon, yet a commit made in the checkout — signed through the
// daemon — comes out authored as the task and verifies.
func TestRemoteSignerAuthorsCheckoutCommitAsTask(t *testing.T) {
	src, base := newRepo(t)
	d := New(src, filepath.Join(t.TempDir(), "run"))
	ctrlSock := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := readapi.ListenUnix(ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.ServeControl(ctx, ln)

	if resp, err := DialControl(ctrlSock, PingRequest()); err != nil || !resp.OK {
		t.Fatalf("daemon not up: %v", err)
	}
	start, err := DialControl(ctrlSock, StartRequest("src/auth", "1h", base.Hex()))
	if err != nil || !start.OK || start.Task == nil {
		t.Fatalf("start: %v (%+v)", err, start)
	}
	info := *start.Task
	pubBytes, err := base64.StdEncoding.DecodeString(info.PublicKey)
	if err != nil || len(pubBytes) != ed25519.PublicKeySize {
		t.Fatalf("bad task public key: %v", err)
	}

	// The task's authority derives from its public key alone — no private key here.
	if core.DerivedAuthorityOf(ed25519.PublicKey(pubBytes)) != info.Fingerprint {
		t.Fatal("task fingerprint does not derive from its published public key")
	}

	// A checkout provisioned from the base and sealed with the daemon coordinates.
	dst, _, err := core.ProvisionCheckout(src, filepath.Join(t.TempDir(), "checkout"), base)
	if err != nil {
		t.Fatal(err)
	}
	marker := core.TaskMarker{
		Fingerprint: info.Fingerprint, Scope: info.Scope, Base: info.Base,
		DaemonSocket: ctrlSock, TaskID: info.ID, PublicKey: info.PublicKey,
	}
	if err := core.SealTaskCheckout(dst, marker, nil); err != nil { // nil: no local key
		t.Fatal(err)
	}

	// Commit in the checkout, signed through the daemon.
	rs := NewRemoteSigner(ctrlSock, info.ID, ed25519.PublicKey(pubBytes))
	tree, err := core.TreeOf(dst, base)
	if err != nil {
		t.Fatal(err)
	}
	cr, err := core.Commit(dst, core.CLICapabilities(), core.CommitParams{
		Ref: "refs/heads/main", ExpectedOld: base, Tree: tree,
		Parents: []multihash.Multihash{base}, Message: "work", Author: "agent",
		RemoteSigner: rs, Now: 2,
	})
	if err != nil {
		t.Fatalf("remote-signed commit: %v", err)
	}

	// The change verifies (its embedded key signed it) and is authored as the task,
	// even though this process never held the task key.
	if err := core.VerifyAuthority(dst, cr.Change); err != nil {
		t.Fatalf("remote-signed change does not verify as its own authority: %v", err)
	}
	if got := storedAuthorityOf(t, dst, cr.Change); got != info.Fingerprint {
		t.Fatalf("commit authority = %q, want task fingerprint %q", got, info.Fingerprint)
	}
	if !cr.SignerKey.Equal(ed25519.PublicKey(pubBytes)) {
		t.Fatal("commit result reports a signer key other than the task key")
	}
}

// TestSignRefusedForExpiredTask: expiry is revocation, so the daemon will not
// sign as a task it has reaped.
func TestSignRefusedForExpiredTask(t *testing.T) {
	src, base := newRepo(t)
	now := time.Unix(1000, 0)
	d := New(src, filepath.Join(t.TempDir(), "run"))
	d.SetClock(func() time.Time { return now })
	info, err := d.StartTask("/", time.Minute, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := d.signForTask(info.ID, []byte("x")); err != nil {
		t.Fatalf("sign before expiry: %v", err)
	}
	now = now.Add(2 * time.Minute)
	if _, err := d.signForTask(info.ID, []byte("x")); err == nil {
		t.Fatal("daemon signed as an expired task")
	}
}

func storedAuthorityOf(t *testing.T, r *repo.Repo, change multihash.Multihash) string {
	t.Helper()
	o, err := r.Objects.Get(change)
	if err != nil {
		t.Fatal(err)
	}
	c, err := o.AsChange()
	if err != nil {
		t.Fatal(err)
	}
	po, err := r.Objects.Get(c.Provenance)
	if err != nil {
		t.Fatal(err)
	}
	pv, err := po.AsProvenance()
	if err != nil {
		t.Fatal(err)
	}
	return pv.Authority
}
