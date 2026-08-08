// Package store implements a content-addressed object store on a plain
// filesystem. Objects are immutable and named by the multihash of their
// canonical bytes. Writes are atomic and idempotent; reads verify integrity
// against the identity before returning (design §2: immutable
// content-addressed objects in a Merkle DAG).
package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
	"github.com/dividebyzero/claude-experiments/loom/internal/object"
)

// ErrNotFound is returned when an object is absent from the store.
var ErrNotFound = errors.New("store: object not found")

// ErrCorrupt is returned when stored bytes do not hash to their identity.
var ErrCorrupt = errors.New("store: object failed integrity check")

// Store is an object store rooted at a directory. The default hash algorithm
// is used for new writes; reads accept any algorithm the identity declares.
type Store struct {
	root string
	algo multihash.Code
}

// Open returns a store rooted at dir, creating it if necessary.
func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &Store{root: dir, algo: multihash.Default}, nil
}

// Root returns the store's root directory.
func (s *Store) Root() string { return s.root }

// path maps an identity to its on-disk location. Objects are sharded by the
// first byte of the raw digest (not the multihash prefix, which is constant
// per algorithm) to spread them across directories; the filename carries the
// full multihash so identities under different algorithms never collide.
func (s *Store) path(id multihash.Multihash) (dir, file string, err error) {
	if _, _, err := multihash.Decode(id); err != nil {
		return "", "", err
	}
	digest := id.Digest()
	if len(digest) == 0 {
		return "", "", multihash.ErrMalformed
	}
	shard := fmt.Sprintf("%02x", digest[0])
	return filepath.Join(s.root, shard), id.Hex(), nil
}

// PutRaw stores pre-encoded object bytes and returns their identity. It is
// idempotent: storing bytes that already exist is a no-op.
func (s *Store) PutRaw(b []byte) (multihash.Multihash, error) {
	id, err := multihash.Sum(s.algo, b)
	if err != nil {
		return nil, err
	}
	dir, file, err := s.path(id)
	if err != nil {
		return nil, err
	}
	full := filepath.Join(dir, file)
	if _, err := os.Stat(full); err == nil {
		return id, nil // already present; content-addressed => identical
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	if err := writeFileAtomic(full, b); err != nil {
		return nil, err
	}
	return id, nil
}

// Put encodes and stores an object, returning its identity.
func (s *Store) Put(o *object.Object) (multihash.Multihash, error) {
	return s.PutRaw(o.Encode())
}

// PutVerified stores raw bytes under a caller-supplied identity, after
// verifying the bytes hash to it under the identity's own algorithm. Unlike
// PutRaw it preserves the given multihash (and thus its algorithm) rather than
// recomputing under the store default — this is what lets sync replicate
// objects written by a peer using a different digest.
func (s *Store) PutVerified(id multihash.Multihash, b []byte) error {
	ok, err := multihash.Verify(id, b)
	if err != nil {
		return err
	}
	if !ok {
		return ErrCorrupt
	}
	dir, file, err := s.path(id)
	if err != nil {
		return err
	}
	full := filepath.Join(dir, file)
	if _, err := os.Stat(full); err == nil {
		return nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return writeFileAtomic(full, b)
}

// Has reports whether an object exists in the store.
func (s *Store) Has(id multihash.Multihash) bool {
	dir, file, err := s.path(id)
	if err != nil {
		return false
	}
	_, err = os.Stat(filepath.Join(dir, file))
	return err == nil
}

// GetRaw reads the raw bytes of an object, verifying them against id.
func (s *Store) GetRaw(id multihash.Multihash) ([]byte, error) {
	dir, file, err := s.path(id)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, file))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	ok, err := multihash.Verify(id, b)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ErrCorrupt
	}
	return b, nil
}

// Get reads and decodes an object, verifying integrity.
func (s *Store) Get(id multihash.Multihash) (*object.Object, error) {
	b, err := s.GetRaw(id)
	if err != nil {
		return nil, err
	}
	return object.Decode(b)
}

// Walk calls fn for every object identity in the store. It skips temp files
// and any filename that is not a valid multihash. Used by garbage collection.
func (s *Store) Walk(fn func(multihash.Multihash) error) error {
	shards, err := os.ReadDir(s.root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, shard := range shards {
		if !shard.IsDir() {
			continue
		}
		files, err := os.ReadDir(filepath.Join(s.root, shard.Name()))
		if err != nil {
			return err
		}
		for _, f := range files {
			name := f.Name()
			if strings.HasPrefix(name, ".tmp-") {
				continue
			}
			id, err := multihash.ParseHex(name)
			if err != nil {
				continue // not an object file
			}
			if err := fn(id); err != nil {
				return err
			}
		}
	}
	return nil
}

// Delete removes an object from the store. Missing objects are not an error.
// This is used only by garbage collection; objects are otherwise immutable.
func (s *Store) Delete(id multihash.Multihash) error {
	dir, file, err := s.path(id)
	if err != nil {
		return err
	}
	if err := os.Remove(filepath.Join(dir, file)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// writeFileAtomic writes data to a temp file in the same directory, fsyncs it,
// and renames it into place — so a reader never observes a partial object.
func writeFileAtomic(path string, data []byte) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after a successful rename
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}
