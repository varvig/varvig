package refs

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// lock acquires the repo-wide ref mutation lock via an exclusive lock file,
// spinning with a short backoff until it succeeds or times out. The returned
// function releases the lock. This serializes concurrent CAS attempts across
// processes so the read-check-write sequence stays atomic.
func (s *Store) lock() (func(), error) {
	if err := os.MkdirAll(filepath.Dir(s.lockPath), 0o755); err != nil {
		return nil, err
	}
	// Lock timing uses the wall clock, not the injectable clock: the injectable
	// clock stamps reflog entries (and tests drive it deterministically), so it
	// must not be consumed by lock backoff.
	deadline := time.Now().Add(10 * time.Second)
	backoff := 1 * time.Millisecond
	for {
		f, err := os.OpenFile(s.lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(s.lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("refs: timed out acquiring lock %s", s.lockPath)
		}
		time.Sleep(backoff)
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
}
