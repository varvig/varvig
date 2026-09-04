package denylist

import (
	"os"
	"path/filepath"
	"testing"
)

// TestDefaultIsEmpty is the shipped-state guarantee (design addendum, U5): with no
// deny-list file, the list denies nothing.
func TestDefaultIsEmpty(t *testing.T) {
	l := Load(t.TempDir())
	if !l.Empty() {
		t.Fatalf("a repo with no deny-list file must load an empty list, got %s", l)
	}
	if l.Denied("anything/at/all") {
		t.Fatal("an empty list must deny nothing")
	}
}

// TestDeniedPrefixMatch: a denied entry covers an exact path and everything under
// it; comments and blank lines are ignored.
func TestDeniedPrefixMatch(t *testing.T) {
	dir := t.TempDir()
	content := "# secrets\nsecrets\n\nsrc/private/\n"
	if err := os.WriteFile(filepath.Join(dir, fileName), []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	l := Load(dir)
	if l.Empty() {
		t.Fatal("a populated file must not load empty")
	}
	for _, p := range []string{"secrets", "secrets/key.pem", "src/private/db.go"} {
		if !l.Denied(p) {
			t.Errorf("%q should be denied", p)
		}
	}
	for _, p := range []string{"src/public/x.go", "secretsauce/ok.go", "README.md"} {
		if l.Denied(p) {
			t.Errorf("%q should not be denied (prefix must be boundary-aware)", p)
		}
	}
}
