package sshkey

import (
	"crypto/ed25519"
	"io"
	"net"
	"path/filepath"
	"testing"
)

// fakeAgent is a minimal ssh-agent that holds one Ed25519 key and answers
// REQUEST_IDENTITIES and SIGN_REQUEST — enough to exercise the client.
type fakeAgent struct {
	priv    ed25519.PrivateKey
	pub     ed25519.PublicKey
	comment string
}

func (f *fakeAgent) serve(t *testing.T, ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.handle(t, conn)
	}
}

func (f *fakeAgent) handle(t *testing.T, conn net.Conn) {
	defer conn.Close()
	pubBlob := append(appendString(nil, []byte(KeyTypeEd25519)), appendString(nil, f.pub)...)
	for {
		var hdr [4]byte
		if _, err := io.ReadFull(conn, hdr[:]); err != nil {
			return
		}
		n := uint32(hdr[0])<<24 | uint32(hdr[1])<<16 | uint32(hdr[2])<<8 | uint32(hdr[3])
		payload := make([]byte, n)
		if _, err := io.ReadFull(conn, payload); err != nil {
			return
		}
		switch payload[0] {
		case agentcRequestIdentities:
			var body []byte
			body = append(body, agentIdentitiesAnswer)
			body = appendUint32(body, 1)
			body = appendString(body, pubBlob)
			body = appendString(body, []byte(f.comment))
			writeFrame(conn, body)
		case agentcSignRequest:
			r := reader{b: payload[1:]}
			_, _ = r.string() // key blob
			data, _ := r.string()
			sig := ed25519.Sign(f.priv, data)
			env := append(appendString(nil, []byte(KeyTypeEd25519)), appendString(nil, sig)...)
			var body []byte
			body = append(body, agentSignResponse)
			body = appendString(body, env)
			writeFrame(conn, body)
		default:
			writeFrame(conn, []byte{agentFailure})
		}
	}
}

func writeFrame(conn net.Conn, payload []byte) {
	var frame []byte
	frame = appendUint32(frame, uint32(len(payload)))
	frame = append(frame, payload...)
	conn.Write(frame)
}

func startFakeAgent(t *testing.T) (socket string, pub ed25519.PublicKey) {
	t.Helper()
	priv, pk := testKey(t)
	fa := &fakeAgent{priv: priv, pub: pk, comment: "jan@laptop"}
	sock := filepath.Join(t.TempDir(), "agent.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go fa.serve(t, ln)
	return sock, pk
}

func TestAgentIdentities(t *testing.T) {
	sock, pub := startFakeAgent(t)
	ag, err := DialAgent(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	ids, err := ag.Identities()
	if err != nil {
		t.Fatal(err)
	}
	if len(ids) != 1 {
		t.Fatalf("want 1 identity, got %d", len(ids))
	}
	if !ids[0].PublicKey.Key.Equal(pub) {
		t.Fatal("agent public key mismatch")
	}
	if ids[0].Comment != "jan@laptop" {
		t.Fatalf("comment: got %q", ids[0].Comment)
	}
}

func TestAgentSign(t *testing.T) {
	sock, pub := startFakeAgent(t)
	ag, err := DialAgent(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ag.Close()

	ids, err := ag.Identities()
	if err != nil {
		t.Fatal(err)
	}
	msg := []byte("the bytes that get signed")
	sig, err := ag.Sign(ids[0].Blob, msg)
	if err != nil {
		t.Fatal(err)
	}
	if len(sig) != ed25519.SignatureSize {
		t.Fatalf("signature size %d", len(sig))
	}
	if !ed25519.Verify(pub, msg, sig) {
		t.Fatal("agent signature does not verify")
	}
}

func TestDialAgentNoSocket(t *testing.T) {
	if _, err := DialAgent(""); err == nil {
		t.Fatal("expected error for empty socket path")
	}
}
