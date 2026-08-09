package identity

import (
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
)

// ErrKeyExists is returned when InitKey would overwrite an existing key. A key
// is an identity; silently replacing one is never right (auth design §2.3,
// "must refuse to overwrite an existing key").
var ErrKeyExists = errors.New("identity: key already exists")

// InitKey generates a fresh Ed25519 key under dir (typically ~/.varvig/keys),
// writing "<name>.key" (the 32-byte seed, mode 0600) and "<name>.pub" (an
// authorized_keys line). It refuses to overwrite either file. This is the
// fallback for users with no SSH key at all; anyone with ~/.ssh/id_ed25519
// should use that instead (auth design §2.3).
//
// The seed is stored raw rather than encrypted at rest: OS-keychain encryption
// is a documented enhancement (auth design §2.3) but requires platform-specific
// integration a cgo-free portable binary cannot assume, so it is deferred and
// the 0600 mode is the protection in the meantime.
func InitKey(dir, name string) (*Identity, error) {
	if name == "" {
		return nil, errors.New("identity: key name must not be empty")
	}
	keyPath := filepath.Join(dir, name+".key")
	pubPath := filepath.Join(dir, name+".pub")
	for _, p := range []string{keyPath, pubPath} {
		if _, err := os.Stat(p); err == nil {
			return nil, fmt.Errorf("%w: %s", ErrKeyExists, p)
		}
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}

	seed := make([]byte, ed25519.SeedSize)
	if _, err := rand.Read(seed); err != nil {
		return nil, err
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := sshkey.PublicKey{Key: priv.Public().(ed25519.PublicKey), Comment: name}

	if err := os.WriteFile(keyPath, seed, 0o600); err != nil {
		return nil, err
	}
	if err := os.WriteFile(pubPath, []byte(pub.AuthorizedLine()+"\n"), 0o644); err != nil {
		// Best-effort cleanup so a half-written identity does not linger.
		_ = os.Remove(keyPath)
		return nil, err
	}
	return &Identity{PublicKey: pub, Source: SourceVarvig, Signer: keySigner(priv)}, nil
}
