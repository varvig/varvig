package identity

import (
	"crypto/ed25519"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func envStub(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestResolveVarvigFallback(t *testing.T) {
	home := t.TempDir()
	id, err := InitKey(filepath.Join(home, ".varvig", "keys"), "jan")
	if err != nil {
		t.Fatal(err)
	}
	want := id.Fingerprint()

	got, err := Resolve(home, envStub(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.Source != SourceVarvig {
		t.Fatalf("source: got %q want %q", got.Source, SourceVarvig)
	}
	if got.Fingerprint() != want {
		t.Fatal("fingerprint mismatch")
	}
	if !got.CanSign() {
		t.Fatal("varvig fallback identity should be able to sign")
	}
	// The signer must actually produce a verifiable signature.
	msg := []byte("hello")
	sig, err := got.Signer.Sign(msg)
	if err != nil {
		t.Fatal(err)
	}
	if !ed25519.Verify(got.Signer.Public(), msg, sig) {
		t.Fatal("signature does not verify")
	}
}

func TestResolveNoIdentity(t *testing.T) {
	home := t.TempDir()
	_, err := Resolve(home, envStub(nil))
	if !errors.Is(err, ErrNoIdentity) {
		t.Fatalf("expected ErrNoIdentity, got %v", err)
	}
}

func TestInitKeyRefusesOverwrite(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if _, err := InitKey(dir, "jan"); err != nil {
		t.Fatal(err)
	}
	_, err := InitKey(dir, "jan")
	if !errors.Is(err, ErrKeyExists) {
		t.Fatalf("expected ErrKeyExists, got %v", err)
	}
}

func TestInitKeyPermissions(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "keys")
	if _, err := InitKey(dir, "jan"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(filepath.Join(dir, "jan.key"))
	if err != nil {
		t.Fatal(err)
	}
	if perm := fi.Mode().Perm(); perm != 0o600 {
		t.Fatalf("key file mode: got %o want 600", perm)
	}
}

// SSH key on disk takes priority over the varvig fallback.
func TestResolvePrefersSSHKey(t *testing.T) {
	home := t.TempDir()
	// A varvig fallback key exists...
	if _, err := InitKey(filepath.Join(home, ".varvig", "keys"), "fallback"); err != nil {
		t.Fatal(err)
	}
	// ...and an ~/.ssh/id_ed25519 also exists.
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = 0x42
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pem := buildTestOpenSSHKey(priv, pub, "ssh")
	if err := os.WriteFile(filepath.Join(sshDir, "id_ed25519"), pem, 0o600); err != nil {
		t.Fatal(err)
	}

	got, err := Resolve(home, envStub(nil))
	if err != nil {
		t.Fatal(err)
	}
	defer got.Close()
	if got.Source != SourceSSHKey {
		t.Fatalf("source: got %q want %q", got.Source, SourceSSHKey)
	}
	if !got.PublicKey.Key.Equal(pub) {
		t.Fatal("resolved the wrong key")
	}
}
