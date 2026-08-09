package refupdate

import (
	"bufio"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// ReplayGuard remembers nonces so a captured signed update cannot be replayed
// (auth design §5.2 step 3). The window is keyed by (signer, ref): a nonce need
// only be remembered until its update's not_after passes, after which the
// expiry check (step 2) rejects the update anyway.
type ReplayGuard interface {
	// Check reports whether (signerFP, ref, nonce) has already been seen. When
	// it has not, Check records it and returns false; when it has, Check returns
	// true and the caller must reject the update. now and notAfter are unix
	// seconds; entries whose notAfter has passed are pruned.
	Check(signerFP, ref string, nonce []byte, notAfter, now int64) (seen bool, err error)
}

// MemoryGuard is an in-memory ReplayGuard for tests and ephemeral verifiers.
type MemoryGuard struct {
	seen map[string]int64 // key -> notAfter
}

// NewMemoryGuard returns an empty in-memory guard.
func NewMemoryGuard() *MemoryGuard { return &MemoryGuard{seen: map[string]int64{}} }

func (g *MemoryGuard) Check(signerFP, ref string, nonce []byte, notAfter, now int64) (bool, error) {
	// Prune expired entries so the map does not grow without bound.
	for k, na := range g.seen {
		if na < now {
			delete(g.seen, k)
		}
	}
	key := replayKey(signerFP, ref, nonce)
	if _, ok := g.seen[key]; ok {
		return true, nil
	}
	g.seen[key] = notAfter
	return false, nil
}

// FileGuard is a filesystem-backed ReplayGuard. Each surviving entry is one line
//
//	<signerFP> <ref> <nonceHex> <notAfter>
//
// and the whole file is rewritten (minus expired entries, plus the new one) on
// each successful record, bounding its size to the active replay window. A lock
// file serializes concurrent checks so the read-test-append is atomic.
type FileGuard struct {
	path     string
	lockPath string
}

// NewFileGuard returns a guard persisting under dir (created if missing).
func NewFileGuard(dir string) (*FileGuard, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &FileGuard{
		path:     filepath.Join(dir, "nonces"),
		lockPath: filepath.Join(dir, "nonces.lock"),
	}, nil
}

func (g *FileGuard) Check(signerFP, ref string, nonce []byte, notAfter, now int64) (bool, error) {
	unlock, err := g.lock()
	if err != nil {
		return false, err
	}
	defer unlock()

	entries, err := g.load()
	if err != nil {
		return false, err
	}
	key := replayKey(signerFP, ref, nonce)
	kept := entries[:0]
	for _, e := range entries {
		if e.notAfter < now {
			continue // expired; drop
		}
		if e.key == key {
			// Already seen and still within window: a replay.
			return true, nil
		}
		kept = append(kept, e)
	}
	kept = append(kept, replayEntry{key: key, notAfter: notAfter})
	if err := g.store(kept); err != nil {
		return false, err
	}
	return false, nil
}

type replayEntry struct {
	key      string
	notAfter int64
}

func (g *FileGuard) load() ([]replayEntry, error) {
	f, err := os.Open(g.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()
	var out []replayEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) != 4 {
			continue // tolerate a malformed line rather than fail closed
		}
		na, err := strconv.ParseInt(fields[3], 10, 64)
		if err != nil {
			continue
		}
		out = append(out, replayEntry{key: fields[0] + " " + fields[1] + " " + fields[2], notAfter: na})
	}
	return out, sc.Err()
}

func (g *FileGuard) store(entries []replayEntry) error {
	var sb strings.Builder
	for _, e := range entries {
		fmt.Fprintf(&sb, "%s %d\n", e.key, e.notAfter)
	}
	tmp, err := os.CreateTemp(filepath.Dir(g.path), ".tmp-nonces-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.WriteString(sb.String()); err != nil {
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
	return os.Rename(tmpName, g.path)
}

func (g *FileGuard) lock() (func(), error) {
	deadline := time.Now().Add(10 * time.Second)
	backoff := 1 * time.Millisecond
	for {
		f, err := os.OpenFile(g.lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
		if err == nil {
			f.Close()
			return func() { os.Remove(g.lockPath) }, nil
		}
		if !os.IsExist(err) {
			return nil, err
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("refupdate: timed out acquiring replay lock %s", g.lockPath)
		}
		time.Sleep(backoff)
		if backoff < 50*time.Millisecond {
			backoff *= 2
		}
	}
}

// replayKey is the storage key: the entry line minus the notAfter column. The
// nonce is hex-encoded so it never contains a separator.
func replayKey(signerFP, ref string, nonce []byte) string {
	return signerFP + " " + ref + " " + hex.EncodeToString(nonce)
}
