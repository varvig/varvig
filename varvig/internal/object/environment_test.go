package object

import (
	"bytes"
	"testing"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// TestEnvironmentDeterministic asserts identical environments encode to
// identical bytes (and thus hash identically) regardless of Go map iteration
// order — the property deduplication and cross-peer comparison both rely on
// (federation §2.3).
func TestEnvironmentDeterministic(t *testing.T) {
	a := NewEnvironment(Environment{
		Platform:   "linux/amd64",
		Toolchains: map[string]string{"go": "1.24", "clang": "18", "ld": "2.42"},
		Flags:      map[string]string{"CGO_ENABLED": "0", "GOFLAGS": "-trimpath"},
		Model:      &EnvModel{ID: "claude", Version: "3", Params: "temp=0"},
	})
	// Same content, maps built in a different insertion order.
	b := NewEnvironment(Environment{
		Platform:   "linux/amd64",
		Toolchains: map[string]string{"ld": "2.42", "go": "1.24", "clang": "18"},
		Flags:      map[string]string{"GOFLAGS": "-trimpath", "CGO_ENABLED": "0"},
		Model:      &EnvModel{ID: "claude", Version: "3", Params: "temp=0"},
	})
	if !bytes.Equal(a.Encode(), b.Encode()) {
		t.Fatal("identical environments must encode identically regardless of map order")
	}
	ha, _ := multihash.Sum(multihash.Default, a.Encode())
	hb, _ := multihash.Sum(multihash.Default, b.Encode())
	if !ha.Equal(hb) {
		t.Fatal("identical environments must hash identically")
	}

	// Round-trip preserves every component.
	got, err := b.AsEnvironment()
	if err != nil {
		t.Fatal(err)
	}
	if got.Platform != "linux/amd64" || got.Toolchains["go"] != "1.24" ||
		got.Flags["CGO_ENABLED"] != "0" || got.Model == nil || got.Model.ID != "claude" {
		t.Fatalf("round-trip lost data: %+v", got)
	}

	// A different environment hashes differently.
	c := NewEnvironment(Environment{Platform: "darwin/arm64"})
	hc, _ := multihash.Sum(multihash.Default, c.Encode())
	if ha.Equal(hc) {
		t.Fatal("different environments must not collide")
	}
}

// TestSameEnvironmentClass covers §2.3: comparison is confined to a class,
// cross-class is reported (not silently equal), and missing environment is
// unknown class — never matching.
func TestSameEnvironmentClass(t *testing.T) {
	env1, _ := multihash.Sum(multihash.Default, []byte("env-A"))
	env2, _ := multihash.Sum(multihash.Default, []byte("env-B"))

	withEnv := func(h multihash.Multihash) Provenance { return Provenance{Authority: "peer", Environment: h} }
	noEnv := Provenance{Authority: "peer"}

	if same, comparable := SameEnvironmentClass(withEnv(env1), withEnv(env1)); !same || !comparable {
		t.Error("same environment should compare equal and comparable")
	}
	if same, comparable := SameEnvironmentClass(withEnv(env1), withEnv(env2)); same || !comparable {
		t.Error("different environments should be comparable but not equal (cross-class must be visible)")
	}
	if _, comparable := SameEnvironmentClass(withEnv(env1), noEnv); comparable {
		t.Error("evidence without an environment is unknown class and must not be comparable")
	}
	if _, comparable := SameEnvironmentClass(noEnv, noEnv); comparable {
		t.Error("two environment-less records must be unknown class, never matching")
	}
	if _, known := noEnv.EnvironmentClass(); known {
		t.Error("missing environment must report unknown class")
	}
}

// TestProvenanceEnvironmentRoundTrip confirms the new evidence field survives
// encode/decode and that omitting it leaves the encoding byte-identical to a
// pre-federation provenance (additive, mixed-version safe).
func TestProvenanceEnvironmentRoundTrip(t *testing.T) {
	env, _ := multihash.Sum(multihash.Default, []byte("env"))
	p := Provenance{Authority: "a", Model: "m", Environment: env}
	got, err := NewProvenance(p).AsProvenance()
	if err != nil {
		t.Fatal(err)
	}
	if got.Environment == nil || !got.Environment.Equal(env) {
		t.Fatalf("environment not preserved: %+v", got.Environment)
	}

	without := NewProvenance(Provenance{Authority: "a", Model: "m"})
	legacy := NewProvenance(Provenance{Authority: "a", Model: "m"})
	if !bytes.Equal(without.Encode(), legacy.Encode()) {
		t.Fatal("provenance without an environment must encode as before")
	}
	if _, ok := without.Field(tagProvEnvironment); ok {
		t.Fatal("environment tag emitted when unset")
	}
}
