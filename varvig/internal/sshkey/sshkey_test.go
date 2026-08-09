package sshkey

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"strings"
	"testing"
)

// deterministic key material for reproducible tests.
func testKey(t *testing.T) (ed25519.PrivateKey, ed25519.PublicKey) {
	t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 1)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	return priv, priv.Public().(ed25519.PublicKey)
}

func TestPublicKeyRoundTrip(t *testing.T) {
	_, pub := testKey(t)
	pk := PublicKey{Key: pub, Comment: "jan@laptop"}

	line := pk.AuthorizedLine()
	if !strings.HasPrefix(line, "ssh-ed25519 ") || !strings.HasSuffix(line, " jan@laptop") {
		t.Fatalf("unexpected authorized line: %q", line)
	}
	got, err := ParseAuthorizedKey(line)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Key.Equal(pub) {
		t.Fatal("public key did not round-trip")
	}
	if got.Comment != "jan@laptop" {
		t.Fatalf("comment: got %q", got.Comment)
	}
}

func TestFingerprintMatchesSSHConvention(t *testing.T) {
	_, pub := testKey(t)
	pk := PublicKey{Key: pub}
	sum := sha256.Sum256(pk.Blob())
	want := "SHA256:" + base64.RawStdEncoding.EncodeToString(sum[:])
	if got := pk.Fingerprint(); got != want {
		t.Fatalf("fingerprint: got %q want %q", got, want)
	}
	// It must be padding-free, as OpenSSH prints it.
	if strings.Contains(pk.Fingerprint(), "=") {
		t.Fatal("fingerprint should not contain base64 padding")
	}
}

func TestParseAuthorizedKeyRejects(t *testing.T) {
	cases := map[string]string{
		"empty":      "",
		"comment":    "# a comment",
		"rsa":        "ssh-rsa AAAAB3Nz",
		"one field":  "ssh-ed25519",
		"bad base64": "ssh-ed25519 not-base64!!",
		"wrong length": "ssh-ed25519 " + base64.StdEncoding.EncodeToString(
			append(appendString(nil, []byte("ssh-ed25519")), appendString(nil, []byte("short"))...)),
	}
	for name, line := range cases {
		if _, err := ParseAuthorizedKey(line); err == nil {
			t.Errorf("%s: expected error, got nil", name)
		}
	}
}

// buildOpenSSHKey encodes an unencrypted OpenSSH private key for the given pair.
func buildOpenSSHKey(priv ed25519.PrivateKey, pub ed25519.PublicKey, comment string) []byte {
	pubBlob := append(appendString(nil, []byte(KeyTypeEd25519)), appendString(nil, pub)...)

	var privSection []byte
	privSection = appendUint32(privSection, 0x01020304) // check1
	privSection = appendUint32(privSection, 0x01020304) // check2 (equal)
	privSection = appendString(privSection, []byte(KeyTypeEd25519))
	privSection = appendString(privSection, pub)
	privSection = appendString(privSection, priv) // 64-byte ed25519 private key
	privSection = appendString(privSection, []byte(comment))
	for i := 1; len(privSection)%8 != 0; i++ { // pad 1,2,3,... to 8-byte block
		privSection = append(privSection, byte(i))
	}

	var body []byte
	body = append(body, opensshMagic...)
	body = appendString(body, []byte("none")) // cipher
	body = appendString(body, []byte("none")) // kdf
	body = appendString(body, nil)            // kdfoptions
	body = appendUint32(body, 1)              // nkeys
	body = appendString(body, pubBlob)
	body = appendString(body, privSection)

	b64 := base64.StdEncoding.EncodeToString(body)
	var sb strings.Builder
	sb.WriteString("-----BEGIN OPENSSH PRIVATE KEY-----\n")
	for i := 0; i < len(b64); i += 70 {
		end := i + 70
		if end > len(b64) {
			end = len(b64)
		}
		sb.WriteString(b64[i:end])
		sb.WriteByte('\n')
	}
	sb.WriteString("-----END OPENSSH PRIVATE KEY-----\n")
	return []byte(sb.String())
}

func TestParseOpenSSHPrivateKey(t *testing.T) {
	priv, pub := testKey(t)
	pem := buildOpenSSHKey(priv, pub, "jan@laptop")

	gotPriv, gotPub, err := ParseOpenSSHPrivateKey(pem)
	if err != nil {
		t.Fatal(err)
	}
	if !gotPriv.Equal(priv) {
		t.Fatal("private key mismatch")
	}
	if !gotPub.Key.Equal(pub) {
		t.Fatal("public key mismatch")
	}
	if gotPub.Comment != "jan@laptop" {
		t.Fatalf("comment: got %q", gotPub.Comment)
	}

	// A signature made with the parsed key must verify under the parsed pubkey.
	msg := []byte("promote refs/heads/main")
	sig := ed25519.Sign(gotPriv, msg)
	if !ed25519.Verify(gotPub.Key, msg, sig) {
		t.Fatal("signature from parsed key does not verify")
	}
}

func TestParseOpenSSHPrivateKeyEncrypted(t *testing.T) {
	// Hand-build a header claiming an aes256-ctr cipher; parsing must decline.
	var body []byte
	body = append(body, opensshMagic...)
	body = appendString(body, []byte("aes256-ctr"))
	body = appendString(body, []byte("bcrypt"))
	body = appendString(body, []byte("salt+rounds"))
	body = appendUint32(body, 1)
	body = appendString(body, []byte("pub"))
	body = appendString(body, []byte("ciphertext"))
	b64 := base64.StdEncoding.EncodeToString(body)
	pem := "-----BEGIN OPENSSH PRIVATE KEY-----\n" + b64 + "\n-----END OPENSSH PRIVATE KEY-----\n"

	if _, _, err := ParseOpenSSHPrivateKey([]byte(pem)); !errors.Is(err, ErrEncrypted) {
		t.Fatalf("expected ErrEncrypted, got %v", err)
	}
}

func TestParseOpenSSHPrivateKeyMismatch(t *testing.T) {
	priv, _ := testKey(t)
	_, otherPub := func() (ed25519.PrivateKey, ed25519.PublicKey) {
		seed := make([]byte, ed25519.SeedSize)
		for i := range seed {
			seed[i] = 0xAA
		}
		p := ed25519.NewKeyFromSeed(seed)
		return p, p.Public().(ed25519.PublicKey)
	}()
	// Embed a public key that does not match the private key.
	pem := buildOpenSSHKey(priv, otherPub, "x")
	if _, _, err := ParseOpenSSHPrivateKey(pem); err == nil {
		t.Fatal("expected mismatch error")
	}
}
