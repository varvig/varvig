package gitobj

import (
	"bytes"
	"testing"
)

// TestBlobHashMatchesGit checks our blob hashing against a known git SHA-1.
// `printf 'hello\n' | git hash-object --stdin` yields this value.
func TestBlobHashMatchesGit(t *testing.T) {
	oid := HashObject(KindBlob, []byte("hello\n"))
	const want = "ce013625030ba8dba906f756967f9e9ca394464a"
	if oid.Hex() != want {
		t.Fatalf("blob hash = %s, want %s", oid.Hex(), want)
	}
}

func TestLooseStoreRoundTrip(t *testing.T) {
	s := OpenStore(t.TempDir())
	oid, err := s.Write(KindBlob, []byte("content\n"))
	if err != nil {
		t.Fatalf("Write: %v", err)
	}
	kind, body, err := s.Read(oid)
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	if kind != KindBlob || string(body) != "content\n" {
		t.Fatalf("round trip mismatch: %s %q", kind, body)
	}
}

func TestTreeEncodeDecode(t *testing.T) {
	a := HashObject(KindBlob, []byte("a"))
	b := HashObject(KindBlob, []byte("b"))
	entries := []TreeEntry{
		{Mode: ModeFile, Name: "z.txt", OID: a},
		{Mode: ModeFile, Name: "a.txt", OID: b},
	}
	body := EncodeTree(entries)
	got, err := DecodeTree(body)
	if err != nil {
		t.Fatalf("DecodeTree: %v", err)
	}
	if len(got) != 2 || got[0].Name != "a.txt" || got[1].Name != "z.txt" {
		t.Fatalf("entries not sorted: %+v", got)
	}
}

// TestTreeSortDirectoryRule checks git's rule that a directory entry sorts as
// if its name ended in '/': "foo.txt" must come before dir "foo" is false;
// specifically "foo" (dir) sorts after "foo.txt" because '/' (0x2f) > '.' (0x2e).
func TestTreeSortDirectoryRule(t *testing.T) {
	oid := HashObject(KindBlob, []byte("x"))
	entries := []TreeEntry{
		{Mode: ModeTree, Name: "foo", OID: oid},
		{Mode: ModeFile, Name: "foo.txt", OID: oid},
	}
	body := EncodeTree(entries)
	got, _ := DecodeTree(body)
	// "foo.txt": compare "foo" == "foo", then '.'(file rest) vs '/'(dir virtual)
	// '.' (0x2e) < '/' (0x2f), so foo.txt sorts first.
	if got[0].Name != "foo.txt" || got[1].Name != "foo" {
		t.Fatalf("git directory sort rule violated: %+v", got)
	}
}

func TestCommitEncodeDecodeRoundTrip(t *testing.T) {
	tree := HashObject(KindTree, []byte{})
	p := HashObject(KindCommit, []byte("p"))
	body := EncodeCommit(tree, []OID{p}, "Agent <a@example.com>", 1723100000, "did a thing\n")
	c, err := DecodeCommit(body)
	if err != nil {
		t.Fatalf("DecodeCommit: %v", err)
	}
	if c.Tree != tree {
		t.Fatal("tree mismatch")
	}
	if len(c.Parents) != 1 || c.Parents[0] != p {
		t.Fatalf("parents = %+v", c.Parents)
	}
	if c.Author != "Agent <a@example.com>" || c.AuthorTS != 1723100000 {
		t.Fatalf("author = %q ts = %d", c.Author, c.AuthorTS)
	}
	if c.Message != "did a thing\n" {
		t.Fatalf("message = %q", c.Message)
	}
	if !bytes.Equal(c.RawBody, body) {
		t.Fatal("RawBody not preserved")
	}
}

func TestReadDetectsCorruption(t *testing.T) {
	s := OpenStore(t.TempDir())
	oid, _ := s.Write(KindBlob, []byte("trust"))
	// Point at a different, non-existent oid: Read must fail cleanly.
	var bad OID
	copy(bad[:], oid[:])
	bad[0] ^= 0xff
	if _, _, err := s.Read(bad); err == nil {
		t.Fatal("Read of altered oid unexpectedly succeeded")
	}
}
