// Package gitobj implements just enough of Git's on-disk object format to
// export a Varvig repository to a repository that plain `git` can read, and to
// import one back. This is the "lossless bidirectional export to Git" that
// design §2 calls non-negotiable for adoption: anything that requires a
// special client to read is dead on arrival.
//
// Git identities are SHA-1 over "<type> <len>\0<body>". Varvig keeps its own
// multihash identities; git ids are computed here purely for interop. Objects
// are written as loose, zlib-compressed files under objects/xx/yy…, and read
// from both loose files and packfiles (objects/pack/*.pack, with ofs- and
// ref-delta resolution) so a normally-cloned repository imports cleanly.
package gitobj

import (
	"bytes"
	"compress/zlib"
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
)

// Kind is a git object type.
type Kind string

const (
	KindBlob   Kind = "blob"
	KindTree   Kind = "tree"
	KindCommit Kind = "commit"
)

// Git file modes, as they appear in tree objects (octal, no leading zero).
const (
	ModeFile    = "100644"
	ModeExec    = "100755"
	ModeSymlink = "120000"
	ModeTree    = "40000"
)

// OID is a git object identity (SHA-1).
type OID [20]byte

// Hex returns the 40-character lowercase hex form.
func (o OID) Hex() string { return hex.EncodeToString(o[:]) }

// ParseOID decodes a 40-char hex string.
func ParseOID(s string) (OID, error) {
	var o OID
	b, err := hex.DecodeString(s)
	if err != nil || len(b) != 20 {
		return o, fmt.Errorf("gitobj: invalid oid %q", s)
	}
	copy(o[:], b)
	return o, nil
}

// HashObject computes the git identity of an object of the given kind and body.
func HashObject(kind Kind, body []byte) OID {
	h := sha1.New()
	fmt.Fprintf(h, "%s %d", kind, len(body))
	h.Write([]byte{0})
	h.Write(body)
	var o OID
	copy(o[:], h.Sum(nil))
	return o
}

// TreeEntry is one child of a git tree.
type TreeEntry struct {
	Mode string // one of the Mode* constants
	Name string
	OID  OID
}

// EncodeTree serializes tree entries into a git tree body, applying git's
// exact entry ordering (directories compare as if their names ended in '/').
func EncodeTree(entries []TreeEntry) []byte {
	es := append([]TreeEntry(nil), entries...)
	sort.Slice(es, func(i, j int) bool {
		return treeLess(es[i].Name, es[i].Mode == ModeTree, es[j].Name, es[j].Mode == ModeTree)
	})
	var buf bytes.Buffer
	for _, e := range es {
		buf.WriteString(e.Mode)
		buf.WriteByte(' ')
		buf.WriteString(e.Name)
		buf.WriteByte(0)
		buf.Write(e.OID[:])
	}
	return buf.Bytes()
}

// treeLess implements git's tree-entry comparison. When one name is a prefix
// of the other, the shorter side's virtual next byte is '/' for a directory
// and 0 otherwise — this is why "foo" (file) and "foo" (dir) sort distinctly.
func treeLess(n1 string, dir1 bool, n2 string, dir2 bool) bool {
	l := len(n1)
	if len(n2) < l {
		l = len(n2)
	}
	if c := bytes.Compare([]byte(n1[:l]), []byte(n2[:l])); c != 0 {
		return c < 0
	}
	c1 := virtualByte(n1, l, dir1)
	c2 := virtualByte(n2, l, dir2)
	return c1 < c2
}

func virtualByte(name string, at int, dir bool) int {
	if at < len(name) {
		return int(name[at])
	}
	if dir {
		return '/'
	}
	return 0
}

// DecodeTree parses a git tree body into entries.
func DecodeTree(body []byte) ([]TreeEntry, error) {
	var entries []TreeEntry
	i := 0
	for i < len(body) {
		sp := bytes.IndexByte(body[i:], ' ')
		if sp < 0 {
			return nil, fmt.Errorf("gitobj: tree entry missing mode separator")
		}
		mode := string(body[i : i+sp])
		i += sp + 1
		nul := bytes.IndexByte(body[i:], 0)
		if nul < 0 {
			return nil, fmt.Errorf("gitobj: tree entry missing name terminator")
		}
		name := string(body[i : i+nul])
		i += nul + 1
		if i+20 > len(body) {
			return nil, fmt.Errorf("gitobj: tree entry truncated oid")
		}
		var oid OID
		copy(oid[:], body[i:i+20])
		i += 20
		entries = append(entries, TreeEntry{Mode: mode, Name: name, OID: oid})
	}
	return entries, nil
}

// Commit is a parsed git commit. RawBody is the exact object body, retained so
// a re-export reproduces a byte-identical object (and thus an identical OID).
type Commit struct {
	Tree     OID
	Parents  []OID // in original order — first-parent order is significant
	Author   string
	AuthorTS int64
	Message  string
	RawBody  []byte
}

// DecodeCommit parses a git commit body.
func DecodeCommit(body []byte) (*Commit, error) {
	c := &Commit{RawBody: append([]byte(nil), body...)}
	sep := bytes.Index(body, []byte("\n\n"))
	if sep < 0 {
		return nil, fmt.Errorf("gitobj: commit missing header/message separator")
	}
	header := body[:sep]
	c.Message = string(body[sep+2:])

	for _, line := range bytes.Split(header, []byte("\n")) {
		switch {
		case bytes.HasPrefix(line, []byte("tree ")):
			o, err := ParseOID(string(line[5:]))
			if err != nil {
				return nil, err
			}
			c.Tree = o
		case bytes.HasPrefix(line, []byte("parent ")):
			o, err := ParseOID(string(line[7:]))
			if err != nil {
				return nil, err
			}
			c.Parents = append(c.Parents, o)
		case bytes.HasPrefix(line, []byte("author ")):
			ident, ts := parseIdent(string(line[7:]))
			c.Author = ident
			c.AuthorTS = ts
		}
	}
	return c, nil
}

// parseIdent splits a git identity line "Name <email> <unixts> <tz>" into the
// "Name <email>" identity and the unix timestamp.
func parseIdent(s string) (ident string, ts int64) {
	gt := bytes.LastIndexByte([]byte(s), '>')
	if gt < 0 {
		return s, 0
	}
	ident = s[:gt+1]
	rest := s[gt+1:]
	fields := bytes.Fields([]byte(rest))
	if len(fields) >= 1 {
		if v, err := strconv.ParseInt(string(fields[0]), 10, 64); err == nil {
			ts = v
		}
	}
	return ident, ts
}

// EncodeCommit builds a git commit body from parts, using UTC and identical
// author/committer identities. Used for Varvig-native changes that have no
// original git object to reproduce.
func EncodeCommit(tree OID, parents []OID, ident string, ts int64, message string) []byte {
	var buf bytes.Buffer
	fmt.Fprintf(&buf, "tree %s\n", tree.Hex())
	for _, p := range parents {
		fmt.Fprintf(&buf, "parent %s\n", p.Hex())
	}
	fmt.Fprintf(&buf, "author %s %d +0000\n", ident, ts)
	fmt.Fprintf(&buf, "committer %s %d +0000\n", ident, ts)
	buf.WriteByte('\n')
	buf.WriteString(message)
	return buf.Bytes()
}

// --- loose object store ---

// Store reads and writes git objects under a git directory: loose objects
// directly, and packed objects (from objects/pack/*.pack) on demand.
type Store struct {
	gitDir  string
	once    sync.Once
	packed  map[string]packedObject
	packErr error
}

// OpenStore returns a store rooted at gitDir/objects.
func OpenStore(gitDir string) *Store { return &Store{gitDir: gitDir} }

func (s *Store) objectPath(o OID) string {
	h := o.Hex()
	return filepath.Join(s.gitDir, "objects", h[:2], h[2:])
}

// Write stores an object of the given kind and body, returning its OID. It is
// idempotent: an object already present is left untouched.
func (s *Store) Write(kind Kind, body []byte) (OID, error) {
	oid := HashObject(kind, body)
	path := s.objectPath(oid)
	if _, err := os.Stat(path); err == nil {
		return oid, nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return oid, err
	}
	var zbuf bytes.Buffer
	zw := zlib.NewWriter(&zbuf)
	fmt.Fprintf(zw, "%s %d", kind, len(body))
	zw.Write([]byte{0})
	zw.Write(body)
	if err := zw.Close(); err != nil {
		return oid, err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return oid, err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(zbuf.Bytes()); err != nil {
		tmp.Close()
		return oid, err
	}
	if err := tmp.Close(); err != nil {
		return oid, err
	}
	return oid, os.Rename(tmpName, path)
}

// Read returns the kind and body of an object, verifying that its bytes hash to
// the requested OID. Loose objects are tried first; on a miss the packfiles are
// consulted (loaded once, lazily).
func (s *Store) Read(o OID) (Kind, []byte, error) {
	kind, body, err := s.readLoose(o)
	if err == nil {
		return kind, body, nil
	}
	if !os.IsNotExist(err) {
		return "", nil, err
	}
	if perr := s.ensurePacks(); perr != nil {
		return "", nil, perr
	}
	if po, ok := s.packed[o.Hex()]; ok {
		return po.kind, po.body, nil
	}
	return "", nil, err // the original not-exist
}

// ensurePacks loads and resolves all packfiles once. ref-delta bases that are
// loose (not in any pack) resolve via readLoose.
func (s *Store) ensurePacks() error {
	s.once.Do(func() {
		s.packed, s.packErr = loadPacks(s.gitDir, func(o OID) (Kind, []byte, bool) {
			k, b, err := s.readLoose(o)
			if err != nil {
				return "", nil, false
			}
			return k, b, true
		})
	})
	return s.packErr
}

// readLoose reads a single loose object. A missing object returns an
// os.IsNotExist error so Read can fall back to packs.
func (s *Store) readLoose(o OID) (Kind, []byte, error) {
	f, err := os.Open(s.objectPath(o))
	if err != nil {
		return "", nil, err
	}
	defer f.Close()
	zr, err := zlib.NewReader(f)
	if err != nil {
		return "", nil, err
	}
	defer zr.Close()
	raw, err := io.ReadAll(zr)
	if err != nil {
		return "", nil, err
	}
	nul := bytes.IndexByte(raw, 0)
	if nul < 0 {
		return "", nil, fmt.Errorf("gitobj: object missing header terminator")
	}
	var kind Kind
	var size int
	if _, err := fmt.Sscanf(string(raw[:nul]), "%s %d", (*string)(&kind), &size); err != nil {
		return "", nil, fmt.Errorf("gitobj: bad object header %q", raw[:nul])
	}
	body := raw[nul+1:]
	if len(body) != size {
		return "", nil, fmt.Errorf("gitobj: object size mismatch: header %d, body %d", size, len(body))
	}
	if HashObject(kind, body) != o {
		return "", nil, fmt.Errorf("gitobj: object %s failed integrity check", o.Hex())
	}
	return kind, body, nil
}
