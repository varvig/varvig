// Package refs implements named pointers to objects with atomic
// compare-and-swap updates and an append-only reflog. Ref CAS is the concrete
// concurrency primitive under everything in design §1.4 — declared read/write
// sets and the transaction scheduler ultimately reduce to it — and the reflog
// is the universal-undo substrate of design §2: every ref move is recorded and
// nothing is silently lost.
package refs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

var (
	// ErrConflict is returned when a CAS precondition (the expected old value)
	// does not match the ref's current value.
	ErrConflict = errors.New("refs: compare-and-swap conflict")
	// ErrNotExist is returned when resolving a ref that has no value.
	ErrNotExist = errors.New("refs: ref does not exist")
	// ErrInvalidName is returned for a syntactically invalid ref name.
	ErrInvalidName = errors.New("refs: invalid ref name")
)

// Store manages refs under refsDir and their logs under logsDir, serializing
// mutations with a single lock file. A coarse repo-wide lock is sufficient for
// step 1; finer-grained scheduling arrives with the transaction model (§1.4).
type Store struct {
	refsDir  string
	logsDir  string
	lockPath string
	now      func() time.Time
}

// Open returns a ref store. refsDir and logsDir are created if missing;
// lockPath is the mutation lock (typically alongside them under .varvig).
func Open(refsDir, logsDir, lockPath string) (*Store, error) {
	for _, d := range []string{refsDir, logsDir} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, err
		}
	}
	return &Store{refsDir: refsDir, logsDir: logsDir, lockPath: lockPath, now: time.Now}, nil
}

// SetClock overrides the timestamp source (used in tests for determinism).
func (s *Store) SetClock(now func() time.Time) { s.now = now }

// Resolve returns the current value of a ref, or ErrNotExist.
func (s *Store) Resolve(name string) (multihash.Multihash, error) {
	if err := validName(name); err != nil {
		return nil, err
	}
	return s.readRef(name)
}

func (s *Store) readRef(name string) (multihash.Multihash, error) {
	b, err := os.ReadFile(s.refPath(name))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, ErrNotExist
		}
		return nil, err
	}
	hexStr := strings.TrimSpace(string(b))
	mh, err := multihash.ParseHex(hexStr)
	if err != nil {
		return nil, fmt.Errorf("refs: corrupt ref %q: %w", name, err)
	}
	return mh, nil
}

// CompareAndSwap atomically sets name to newval iff its current value equals
// oldval. A nil oldval requires the ref to be absent (creation); a nil newval
// deletes the ref. Every successful swap appends a reflog entry.
func (s *Store) CompareAndSwap(name string, oldval, newval multihash.Multihash, actor, msg string) error {
	if err := validName(name); err != nil {
		return err
	}
	unlock, err := s.lock()
	if err != nil {
		return err
	}
	defer unlock()

	cur, err := s.readRef(name)
	if err != nil && !errors.Is(err, ErrNotExist) {
		return err
	}
	present := err == nil

	// Check the precondition.
	switch {
	case oldval == nil && present:
		return fmt.Errorf("%w: expected absent, found %s", ErrConflict, cur)
	case oldval != nil && !present:
		return fmt.Errorf("%w: expected %s, found absent", ErrConflict, oldval)
	case oldval != nil && present && !cur.Equal(oldval):
		return fmt.Errorf("%w: expected %s, found %s", ErrConflict, oldval, cur)
	}

	if newval == nil {
		if err := os.Remove(s.refPath(name)); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		if _, _, derr := multihash.Decode(newval); derr != nil {
			return fmt.Errorf("refs: invalid new value: %w", derr)
		}
		if err := s.writeRef(name, newval); err != nil {
			return err
		}
	}
	return s.appendLog(name, oldval, newval, actor, msg)
}

// Create sets a ref that must not already exist.
func (s *Store) Create(name string, val multihash.Multihash, actor, msg string) error {
	return s.CompareAndSwap(name, nil, val, actor, msg)
}

// Delete removes a ref whose current value must equal oldval.
func (s *Store) Delete(name string, oldval multihash.Multihash, actor, msg string) error {
	return s.CompareAndSwap(name, oldval, nil, actor, msg)
}

// List returns all ref names in lexical order.
func (s *Store) List() ([]string, error) {
	var names []string
	err := filepath.WalkDir(s.refsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.refsDir, path)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		return nil, err
	}
	return names, nil
}

// LogNames returns every ref name that has a reflog, including logs for refs
// that have since been deleted. Garbage collection reads these logs so that
// objects still reachable through the reflog are never swept — preserving
// universal undo (design §2).
func (s *Store) LogNames() ([]string, error) {
	var names []string
	err := filepath.WalkDir(s.logsDir, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		rel, err := filepath.Rel(s.logsDir, path)
		if err != nil {
			return err
		}
		names = append(names, filepath.ToSlash(rel))
		return nil
	})
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	return names, nil
}

func (s *Store) refPath(name string) string {
	return filepath.Join(s.refsDir, filepath.FromSlash(name))
}

func (s *Store) writeRef(name string, val multihash.Multihash) error {
	p := s.refPath(name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), ".tmp-ref-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(val.Hex() + "\n"); err != nil {
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
	return os.Rename(tmpName, p)
}

// validName restricts ref names to slash-separated non-empty segments,
// rejecting traversal and control characters so names map safely to paths.
func validName(name string) error {
	if name == "" {
		return fmt.Errorf("%w: empty", ErrInvalidName)
	}
	if strings.HasPrefix(name, "/") || strings.HasSuffix(name, "/") {
		return fmt.Errorf("%w: leading/trailing slash", ErrInvalidName)
	}
	for _, seg := range strings.Split(name, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("%w: bad segment %q", ErrInvalidName, seg)
		}
		if strings.ContainsAny(seg, "\\\x00") {
			return fmt.Errorf("%w: illegal character", ErrInvalidName)
		}
	}
	return nil
}
