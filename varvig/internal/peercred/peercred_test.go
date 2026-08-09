package peercred

import (
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// dialedPair returns a connected client/server Unix-socket pair for testing.
func dialedPair(t *testing.T) (client, server net.Conn) {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()
	client, err = net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case server = <-accepted:
	case <-time.After(2 * time.Second):
		t.Fatal("accept timed out")
	}
	return client, server
}

func TestOfReportsOwnUID(t *testing.T) {
	client, server := dialedPair(t)
	defer client.Close()
	defer server.Close()

	cred, err := Of(server)
	if errors.Is(err, ErrUnsupported) {
		t.Skip("SO_PEERCRED unsupported on this platform")
	}
	if err != nil {
		t.Fatalf("Of: %v", err)
	}
	// Both ends are this test process, so the peer uid is our own.
	if cred.UID != os.Getuid() {
		t.Errorf("peer uid = %d, want %d", cred.UID, os.Getuid())
	}
	if cred.PID == 0 {
		t.Error("peer pid should be reported")
	}
}

func TestOfRejectsNonUnix(t *testing.T) {
	// A TCP pair is not Unix-domain: Of reports ErrNotUnix (or ErrUnsupported off
	// Linux), and Allowed falls back to permit.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			c.Close()
		}
	}()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err := Of(c); err == nil {
		t.Fatal("Of should error on a TCP connection")
	}
	ok, err := Allowed(c, os.Getuid()+1)
	if !ok || err == nil {
		t.Fatalf("Allowed on non-unix should fall back to permit with an error, got ok=%v err=%v", ok, err)
	}
}

func TestFilterListenerAllowsMatchingUID(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "f.sock")
	raw, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	ln := FilterListener(raw, os.Getuid(), nil)
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	select {
	case c := <-accepted:
		c.Close() // matching uid was accepted
	case <-time.After(2 * time.Second):
		t.Fatal("matching-uid connection was not accepted")
	}
}

func TestFilterListenerRejectsMismatchedUID(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "r.sock")
	raw, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	rejected := make(chan Cred, 1)
	// Allow an impossible uid so our own connection is refused.
	ln := FilterListener(raw, os.Getuid()+99999, func(c Cred) { rejected <- c })
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	client, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()

	// On Linux the connection is rejected (onReject fires, Accept keeps waiting).
	// Off Linux, credentials are unreadable and the conn is passed through.
	if _, err := Of(client); errors.Is(err, ErrUnsupported) {
		t.Skip("SO_PEERCRED unsupported on this platform")
	}
	select {
	case c := <-rejected:
		if c.UID != os.Getuid() {
			t.Errorf("rejected uid = %d, want %d", c.UID, os.Getuid())
		}
	case <-accepted:
		t.Fatal("mismatched-uid connection should have been rejected")
	case <-time.After(2 * time.Second):
		t.Fatal("neither accept nor reject happened")
	}
}
