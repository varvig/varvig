// Package identity resolves the principal Varvig acts as, reusing the user's
// existing SSH key (auth design §2). There is nothing to register: the
// fingerprint *is* the identity (auth design §2.2). Resolution tries, in order:
//
//  1. ssh-agent  — the socket named by SSH_AUTH_SOCK. Preferred, because the
//     key can be hardware-backed and never leaves the agent.
//  2. ~/.ssh/id_ed25519 — read directly. Signs in-process when unencrypted; an
//     encrypted key still yields the public identity (from ~/.ssh/id_ed25519.pub
//     or the embedded public key) but defers signing to the agent.
//  3. ~/.varvig/keys/ — the fallback used only when no SSH key exists at all
//     (auth design §2.1). `varvig key init` writes one here.
//
// A resolved Identity carries a Signer, so the rest of the system is handed an
// already-chosen principal and is indifferent to where the key came from — the
// separation the auth design draws between identity and the frozen core.
package identity

import (
	"crypto/ed25519"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/dividebyzero/claude-experiments/varvig/internal/sshkey"
)

// Source names where an identity's key was found, for display in `whoami`.
type Source string

const (
	SourceAgent   Source = "ssh-agent"
	SourceSSHKey  Source = "ssh-key"
	SourceVarvig  Source = "varvig"
	SourceUnknown Source = "unknown"
)

// Signer produces Ed25519 signatures for the active principal. The private key
// may live in this process or inside an agent; callers do not distinguish.
type Signer interface {
	// Public returns the signing public key.
	Public() ed25519.PublicKey
	// Sign returns the raw 64-byte Ed25519 signature over data.
	Sign(data []byte) ([]byte, error)
}

// Identity is a resolved principal: its public key, where it came from, and a
// Signer (nil when only the public identity is known, e.g. an encrypted on-disk
// key with no agent available).
type Identity struct {
	PublicKey sshkey.PublicKey
	Source    Source
	Signer    Signer

	agent *sshkey.Agent // held open for an agent-backed signer; closed by Close
}

// Fingerprint returns the standard SSH SHA256 fingerprint of the identity.
func (id *Identity) Fingerprint() string { return id.PublicKey.Fingerprint() }

// CanSign reports whether this identity can produce signatures.
func (id *Identity) CanSign() bool { return id.Signer != nil }

// Close releases any agent connection held by the identity.
func (id *Identity) Close() error {
	if id.agent != nil {
		return id.agent.Close()
	}
	return nil
}

// ErrNoIdentity is returned when no usable key can be found anywhere.
var ErrNoIdentity = errors.New("identity: no ssh-agent key, ~/.ssh/id_ed25519, or ~/.varvig key found")

// Resolve finds the active identity following the priority order documented on
// the package. home is the user's home directory; pass "" to use the OS value.
// env looks up environment variables (pass os.Getenv or a test stub).
func Resolve(home string, env func(string) string) (*Identity, error) {
	if home == "" {
		h, err := os.UserHomeDir()
		if err != nil {
			return nil, err
		}
		home = h
	}
	if env == nil {
		env = os.Getenv
	}

	// 1. ssh-agent.
	if sock := env("SSH_AUTH_SOCK"); sock != "" {
		if id, err := fromAgent(sock); err == nil {
			return id, nil
		}
		// Agent present but unusable (no ed25519 key, or dial failed): fall
		// through to on-disk keys rather than failing outright.
	}

	// 2. ~/.ssh/id_ed25519 (+ .pub).
	sshPriv := filepath.Join(home, ".ssh", "id_ed25519")
	if id, err := fromSSHFile(sshPriv); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	// 3. ~/.varvig/keys/*.key fallback.
	if id, err := fromVarvigKeys(filepath.Join(home, ".varvig", "keys")); err == nil {
		return id, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}

	return nil, ErrNoIdentity
}

func fromAgent(sock string) (*Identity, error) {
	ag, err := sshkey.DialAgent(sock)
	if err != nil {
		return nil, err
	}
	ids, err := ag.Identities()
	if err != nil || len(ids) == 0 {
		ag.Close()
		if err == nil {
			err = errors.New("identity: agent holds no ed25519 key")
		}
		return nil, err
	}
	first := ids[0]
	return &Identity{
		PublicKey: first.PublicKey,
		Source:    SourceAgent,
		Signer:    &agentSigner{agent: ag, blob: first.Blob, pub: first.PublicKey.Key},
		agent:     ag,
	}, nil
}

func fromSSHFile(path string) (*Identity, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, err // may be os.ErrNotExist, handled by the caller
	}
	priv, pub, err := sshkey.ParseOpenSSHPrivateKey(pem)
	if err == nil {
		return &Identity{PublicKey: pub, Source: SourceSSHKey, Signer: keySigner(priv)}, nil
	}
	if !errors.Is(err, sshkey.ErrEncrypted) {
		return nil, err
	}
	// Encrypted key with no agent: we can still report identity from the .pub.
	pub, perr := readPub(path + ".pub")
	if perr != nil {
		return nil, fmt.Errorf("%w (and no readable %s.pub)", err, path)
	}
	return &Identity{PublicKey: pub, Source: SourceSSHKey, Signer: nil}, nil
}

func readPub(path string) (sshkey.PublicKey, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return sshkey.PublicKey{}, err
	}
	return sshkey.ParseAuthorizedKey(string(b))
}

func fromVarvigKeys(dir string) (*Identity, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err // os.ErrNotExist handled by the caller
	}
	for _, e := range entries {
		if e.IsDir() || filepath.Ext(e.Name()) != ".key" {
			continue
		}
		seed, err := os.ReadFile(filepath.Join(dir, e.Name()))
		if err != nil {
			continue
		}
		if len(seed) != ed25519.SeedSize {
			continue
		}
		priv := ed25519.NewKeyFromSeed(seed)
		pub := sshkey.PublicKey{
			Key:     priv.Public().(ed25519.PublicKey),
			Comment: name(e.Name()),
		}
		return &Identity{PublicKey: pub, Source: SourceVarvig, Signer: keySigner(priv)}, nil
	}
	return nil, os.ErrNotExist
}

func name(file string) string { return file[:len(file)-len(filepath.Ext(file))] }

// --- signers ---

// keySigner signs with an in-process Ed25519 private key.
type keySigner ed25519.PrivateKey

func (k keySigner) Public() ed25519.PublicKey {
	return ed25519.PrivateKey(k).Public().(ed25519.PublicKey)
}
func (k keySigner) Sign(data []byte) ([]byte, error) {
	return ed25519.Sign(ed25519.PrivateKey(k), data), nil
}

// agentSigner defers signing to an ssh-agent, naming the exact advertised key.
type agentSigner struct {
	agent *sshkey.Agent
	blob  []byte
	pub   ed25519.PublicKey
}

func (a *agentSigner) Public() ed25519.PublicKey { return a.pub }
func (a *agentSigner) Sign(data []byte) ([]byte, error) {
	return a.agent.Sign(a.blob, data)
}

// FromPrivateKey builds an in-process Signer for tests and for callers that
// already hold a key (e.g. the repo-local provenance seed).
func FromPrivateKey(priv ed25519.PrivateKey) Signer { return keySigner(priv) }
