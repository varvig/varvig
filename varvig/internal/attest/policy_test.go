package attest

import (
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/reserved"
)

// policySrc is a promotion-policy module: it reads the PolicyInput JSON on
// stdin and refuses (exit 1) unless the change is veto-free and carries at
// least one strong approval. It is deliberately strict, to exercise both the
// admit and refuse paths from one compile.
const policySrc = `package main

import (
	"encoding/json"
	"io"
	"os"
)

type att struct {
	Decision string ` + "`json:\"decision\"`" + `
	Strength string ` + "`json:\"strength\"`" + `
}
type input struct {
	Vetoed       bool  ` + "`json:\"vetoed\"`" + `
	Attestations []att ` + "`json:\"attestations\"`" + `
}

func main() {
	data, _ := io.ReadAll(os.Stdin)
	var in input
	if err := json.Unmarshal(data, &in); err != nil {
		os.Stderr.WriteString("bad input\n")
		os.Exit(2)
	}
	if in.Vetoed {
		os.Stderr.WriteString("refused: vetoed ancestry\n")
		os.Exit(1)
	}
	for _, a := range in.Attestations {
		if a.Decision == "approve" && a.Strength == "strong" {
			os.Exit(0)
		}
	}
	os.Stderr.WriteString("refused: needs a strong approval\n")
	os.Exit(1)
}
`

var (
	policyOnce  sync.Once
	policyBytes []byte
	policyErr   error
)

func policyModule(t *testing.T) []byte {
	t.Helper()
	policyOnce.Do(func() { policyBytes, policyErr = buildWASI(policySrc) })
	if policyErr != nil {
		t.Skipf("cannot build wasm fixture (toolchain unavailable?): %v", policyErr)
	}
	return policyBytes
}

func buildWASI(src string) ([]byte, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "policyfix-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module policyfix\n\ngo 1.24\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "m.wasm")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, &buildErr{msg: string(b), err: err}
	}
	return os.ReadFile(out)
}

type buildErr struct {
	msg string
	err error
}

func (e *buildErr) Error() string { return e.err.Error() + ": " + e.msg }

// TestWasmPolicyAdmitsAndRefuses runs a real wasm policy module against the
// governance context and checks it admits an approved change and refuses an
// unapproved and a vetoed one (tickets §2.5).
func TestWasmPolicyAdmitsAndRefuses(t *testing.T) {
	mod := policyModule(t)
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)
	// A generous timeout: under `go test -race` wazero compiles and runs the
	// ~2 MB Go-compiled module far slower than in production, well past the 10s
	// default. The default guards a runaway policy in real use; the test must
	// not depend on race-mode speed.
	policy := WasmPolicy{Module: mod, Timeout: 2 * time.Minute}

	// Unapproved: refused.
	c := object.NewChange(object.Change{Message: "ticket", Timestamp: 1, Author: "d"})
	cID, _ := r.Objects.Put(c)
	if err := policy.Admit(r, cID); err == nil {
		t.Fatal("policy admitted an unapproved change")
	}

	// Strong approval: admitted.
	ap, _ := Sign(s, object.Attestation{Target: cID, Decision: object.DecisionApprove, Strength: object.StrengthStrong})
	if _, err := Attach(r, ap, "d", 1); err != nil {
		t.Fatalf("Attach approve: %v", err)
	}
	if err := policy.Admit(r, cID); err != nil {
		t.Fatalf("policy refused an approved change: %v", err)
	}

	// A veto on the same revision: refused despite the approval.
	veto, _ := Sign(s, object.Attestation{Target: cID, Decision: object.DecisionVeto, Strength: object.StrengthStrong})
	if _, err := Attach(r, veto, "d", 2); err != nil {
		t.Fatalf("Attach veto: %v", err)
	}
	if err := policy.Admit(r, cID); err == nil {
		t.Fatal("policy admitted a vetoed change")
	}
}

// TestLoadPolicy round-trips a policy module through refs/varvig/policy.
func TestLoadPolicy(t *testing.T) {
	mod := policyModule(t)
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	// No policy configured yet.
	if _, ok, err := LoadPolicy(r); err != nil || ok {
		t.Fatalf("LoadPolicy on empty repo = ok %v err %v, want ok=false", ok, err)
	}
	// Store the module and point the ref at it.
	id, err := r.Objects.Put(object.NewBlob(mod))
	if err != nil {
		t.Fatalf("Put: %v", err)
	}
	if err := r.Refs.Create(reserved.PolicyRef, id, "t", "set"); err != nil {
		t.Fatalf("Create ref: %v", err)
	}
	wp, ok, err := LoadPolicy(r)
	if err != nil || !ok {
		t.Fatalf("LoadPolicy = ok %v err %v, want ok=true", ok, err)
	}
	if len(wp.Module) != len(mod) {
		t.Fatalf("loaded module is %d bytes, want %d", len(wp.Module), len(mod))
	}
}

// TestBuildPolicyInput checks the host-computed context reflects the store.
func TestBuildPolicyInput(t *testing.T) {
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	s := newSigner(t)
	c := object.NewChange(object.Change{Message: "the spec", Timestamp: 1, Author: "director"})
	cID, _ := r.Objects.Put(c)
	ap, _ := Sign(s, object.Attestation{Target: cID, Decision: object.DecisionApprove, Strength: object.StrengthDelegated})
	if _, err := Attach(r, ap, "director", 1); err != nil {
		t.Fatalf("Attach: %v", err)
	}

	in, err := BuildPolicyInput(r, cID)
	if err != nil {
		t.Fatalf("BuildPolicyInput: %v", err)
	}
	if in.Change != cID.Hex() || in.Author != "director" || in.Message != "the spec" {
		t.Fatalf("metadata wrong: %+v", in)
	}
	if in.Materialized {
		t.Fatal("unmaterialized change reported materialized")
	}
	if in.Vetoed {
		t.Fatal("clean change reported vetoed")
	}
	if len(in.Attestations) != 1 || in.Attestations[0].Decision != "approve" || in.Attestations[0].Strength != "delegated" {
		t.Fatalf("attestations wrong: %+v", in.Attestations)
	}
}
