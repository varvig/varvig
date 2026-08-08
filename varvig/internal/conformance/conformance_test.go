package conformance

import (
	"os"
	"path/filepath"
	"testing"
)

// TestWriteGolden regenerates vectors.json. It is a maintenance action, run
// deliberately (VARVIG_WRITE_GOLDEN=1) when the frozen format legitimately gains
// a new vector — never as part of a normal test run.
func TestWriteGolden(t *testing.T) {
	if os.Getenv("VARVIG_WRITE_GOLDEN") == "" {
		t.Skip("set VARVIG_WRITE_GOLDEN=1 to regenerate vectors.json")
	}
	b, err := CanonicalJSON(Build())
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if err := os.WriteFile(filepath.Join(".", "vectors.json"), b, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	t.Logf("wrote vectors.json (%d bytes), suite id %s", len(b), SuiteID(b))
}

// TestConformance is the frozen-format gate: the current build must satisfy the
// checked-in golden suite exactly.
func TestConformance(t *testing.T) {
	fails := Verify(Golden())
	for _, f := range fails {
		t.Errorf("conformance: %s", f)
	}
}

// TestSuiteIDStable pins the suite's content-addressed identity, so an
// accidental change to the golden artifact is visible in review.
func TestSuiteIDStable(t *testing.T) {
	b, err := CanonicalJSON(Golden())
	if err != nil {
		t.Fatalf("CanonicalJSON: %v", err)
	}
	if got := SuiteID(b).Hex(); got != GoldenSuiteID {
		t.Fatalf("suite id = %s, want %s (golden artifact changed?)", got, GoldenSuiteID)
	}
}
