package affected

import (
	"bufio"
	"os"
	"path/filepath"
	"strings"

	"github.com/dividebyzero/claude-experiments/loom/internal/multihash"
)

// Cache memoizes per-file specifier extraction keyed by blob id. Because a
// blob's content fully determines its specifiers, the cache is sound across
// commits and repositories — it is a derived, rebuildable index (design §4.3),
// never a source of truth.
type Cache interface {
	Get(blobID multihash.Multihash) ([]string, bool)
	Put(blobID multihash.Multihash, specs []string)
}

// MemCache is an in-memory cache.
type MemCache struct{ m map[string][]string }

// NewMemCache returns an empty in-memory cache.
func NewMemCache() *MemCache { return &MemCache{m: map[string][]string{}} }

func (c *MemCache) Get(id multihash.Multihash) ([]string, bool) {
	v, ok := c.m[id.Hex()]
	return v, ok
}

func (c *MemCache) Put(id multihash.Multihash, specs []string) {
	c.m[id.Hex()] = specs
}

// DiskCache persists specifier lists under a directory (e.g. .loom/index/deps),
// giving incrementality across process runs. One file per blob id holds its
// specifiers, one per line; an empty file records "analyzed, no dependencies"
// so a fileless language is not re-analyzed forever.
type DiskCache struct{ dir string }

// NewDiskCache returns a disk cache rooted at dir, creating it.
func NewDiskCache(dir string) (*DiskCache, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return &DiskCache{dir: dir}, nil
}

func (c *DiskCache) path(id multihash.Multihash) string {
	h := id.Hex()
	return filepath.Join(c.dir, h[:2], h[2:])
}

func (c *DiskCache) Get(id multihash.Multihash) ([]string, bool) {
	f, err := os.Open(c.path(id))
	if err != nil {
		return nil, false
	}
	defer f.Close()
	var specs []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if line := sc.Text(); line != "" {
			specs = append(specs, line)
		}
	}
	return specs, true
}

func (c *DiskCache) Put(id multihash.Multihash, specs []string) {
	p := c.path(id)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return // cache is best-effort
	}
	_ = os.WriteFile(p, []byte(strings.Join(specs, "\n")), 0o644)
}
