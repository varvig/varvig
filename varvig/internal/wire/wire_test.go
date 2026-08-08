package wire

import (
	"net"
	"reflect"
	"testing"
)

func TestHandshakeRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca, cb := NewConn(a), NewConn(b)

	sent := Hello{Proto: Proto, Caps: []string{CapDeflate, "experimental"}, Hashes: []uint64{0x1e, 0x12}}
	go func() { _ = ca.WriteHandshake(sent) }()

	got, err := cb.ReadHandshake()
	if err != nil {
		t.Fatalf("ReadHandshake: %v", err)
	}
	if got.Proto != Proto {
		t.Fatalf("proto = %q", got.Proto)
	}
	if !reflect.DeepEqual(got.Caps, sent.Caps) {
		t.Fatalf("caps = %v, want %v", got.Caps, sent.Caps)
	}
	if !reflect.DeepEqual(got.Hashes, sent.Hashes) {
		t.Fatalf("hashes = %v, want %v", got.Hashes, sent.Hashes)
	}
}

func TestHandshakeRejectsBadMagic(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	go func() {
		a.Write([]byte("XXXX"))
		a.Close()
	}()
	if _, err := NewConn(b).ReadHandshake(); err == nil {
		t.Fatal("ReadHandshake accepted bad magic")
	}
}

func TestNegotiateIntersection(t *testing.T) {
	cases := []struct {
		local, remote []string
		want          map[string]bool
	}{
		{[]string{"deflate", "x"}, []string{"deflate", "y"}, map[string]bool{"deflate": true}},
		{[]string{"a"}, []string{"b"}, map[string]bool{}},
		{nil, []string{"deflate"}, map[string]bool{}},
		{[]string{"deflate"}, nil, map[string]bool{}},
	}
	for i, c := range cases {
		got := Negotiate(c.local, c.remote)
		if !reflect.DeepEqual(got, c.want) {
			t.Errorf("case %d: got %v, want %v", i, got, c.want)
		}
	}
}

func TestFrameRoundTrip(t *testing.T) {
	a, b := net.Pipe()
	defer a.Close()
	defer b.Close()
	ca, cb := NewConn(a), NewConn(b)

	refs := []Ref{{Name: "refs/heads/main", ID: []byte{0x1e, 0x20, 0x01}}}
	go func() {
		_ = ca.WriteRefs(refs)
		_ = ca.Flush()
	}()
	tp, payload, err := cb.ReadFrame()
	if err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if tp != MsgRefs {
		t.Fatalf("type = %d", tp)
	}
	got, err := ParseRefs(payload)
	if err != nil {
		t.Fatalf("ParseRefs: %v", err)
	}
	if !reflect.DeepEqual(got, refs) {
		t.Fatalf("refs = %+v, want %+v", got, refs)
	}
}

func TestReadUvarintRejectsNonMinimal(t *testing.T) {
	// 0x80 0x00 decodes to 0 but is non-minimal and must be rejected.
	if _, _, err := readUvarint([]byte{0x80, 0x00}); err == nil {
		t.Fatal("accepted non-minimal varint")
	}
}
