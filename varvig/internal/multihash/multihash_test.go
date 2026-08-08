package multihash

import (
	"bytes"
	"testing"
)

func TestSumDecodeRoundTrip(t *testing.T) {
	for _, c := range []Code{SHA2_256, BLAKE3} {
		mh, err := Sum(c, []byte("agents"))
		if err != nil {
			t.Fatalf("Sum(%s): %v", Name(c), err)
		}
		code, digest, err := Decode(mh)
		if err != nil {
			t.Fatalf("Decode(%s): %v", Name(c), err)
		}
		if code != c {
			t.Fatalf("code = %x, want %x", code, c)
		}
		if len(digest) != 32 {
			t.Fatalf("digest len = %d, want 32", len(digest))
		}
	}
}

func TestVerify(t *testing.T) {
	mh, _ := Sum(BLAKE3, []byte("data"))
	ok, err := Verify(mh, []byte("data"))
	if err != nil || !ok {
		t.Fatalf("Verify true case: ok=%v err=%v", ok, err)
	}
	ok, err = Verify(mh, []byte("tampered"))
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if ok {
		t.Fatal("Verify accepted tampered data")
	}
}

func TestDifferentAlgosDifferentMultihash(t *testing.T) {
	a, _ := Sum(SHA2_256, []byte("x"))
	b, _ := Sum(BLAKE3, []byte("x"))
	if a.Equal(b) {
		t.Fatal("different algorithms produced equal multihashes")
	}
}

func TestParseHexRoundTrip(t *testing.T) {
	mh, _ := Sum(BLAKE3, []byte("y"))
	parsed, err := ParseHex(mh.Hex())
	if err != nil {
		t.Fatalf("ParseHex: %v", err)
	}
	if !bytes.Equal(parsed, mh) {
		t.Fatal("ParseHex round-trip mismatch")
	}
}

func TestDecodeRejectsBadFraming(t *testing.T) {
	mh, _ := Sum(BLAKE3, []byte("z"))
	// Truncate the digest: declared length no longer matches.
	if _, _, err := Decode(mh[:len(mh)-1]); err == nil {
		t.Fatal("Decode accepted truncated digest")
	}
	// Trailing byte beyond the declared length.
	if _, _, err := Decode(append(append([]byte(nil), mh...), 0x00)); err == nil {
		t.Fatal("Decode accepted trailing bytes")
	}
}

func TestUnknownCode(t *testing.T) {
	if _, err := Sum(Code(0xffff), []byte("x")); err == nil {
		t.Fatal("Sum accepted unknown code")
	}
	if Registered(Code(0xffff)) {
		t.Fatal("Registered true for unknown code")
	}
}
