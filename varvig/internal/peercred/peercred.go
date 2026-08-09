// Package peercred reads the kernel-attested credentials of the process on the
// other end of a Unix-domain socket (auth design §7.4). Where the OS supports it
// this is unforgeable and requires nothing on the wire: the kernel already knows
// which process connected. It is the second, kernel-level confirmation that backs
// "filesystem permissions are the authentication" — a 0600 socket already limits
// connections to the owning uid, and peercred verifies that the kernel agrees.
//
// SO_PEERCRED (Linux) is read through the standard library's syscall package, so
// this stays cgo-free. Platforms without an equivalent return ErrUnsupported and
// callers fall back to the socket mode alone (the documented default, §7.4).
package peercred

import (
	"errors"
	"net"
)

var (
	// ErrUnsupported is returned on a platform with no SO_PEERCRED equivalent.
	ErrUnsupported = errors.New("peercred: not supported on this platform")
	// ErrNotUnix is returned for a connection that is not Unix-domain (e.g. TCP).
	ErrNotUnix = errors.New("peercred: not a unix-domain connection")
)

// Cred is a peer process's kernel-reported identity.
type Cred struct {
	UID int
	GID int
	PID int
}

// Of returns the peer credentials of a Unix-domain connection. It returns
// ErrNotUnix for a non-unix conn and ErrUnsupported where the platform has no
// SO_PEERCRED equivalent.
func Of(conn net.Conn) (Cred, error) { return peerCred(conn) }

// Allowed reports whether conn's peer uid equals wantUID. When credentials
// cannot be read — a non-unix conn, or an unsupported platform — it returns
// (true, err) so the caller can fall back to filesystem permissions rather than
// deny outright; the error names why the check could not run.
func Allowed(conn net.Conn, wantUID int) (bool, error) {
	c, err := peerCred(conn)
	if err != nil {
		return true, err
	}
	return c.UID == wantUID, nil
}

// FilterListener wraps ln so Accept only returns connections whose peer uid is
// wantUID. A rejected connection is closed (after onReject, if non-nil, is called
// with its credentials); Accept then keeps waiting for an acceptable one.
// Connections whose credentials cannot be read (non-unix, or an unsupported
// platform) are passed through, falling back to the socket's filesystem
// permissions — so wrapping a TCP listener is a harmless no-op.
func FilterListener(ln net.Listener, wantUID int, onReject func(Cred)) net.Listener {
	return &filtered{Listener: ln, uid: wantUID, onReject: onReject}
}

type filtered struct {
	net.Listener
	uid      int
	onReject func(Cred)
}

func (f *filtered) Accept() (net.Conn, error) {
	for {
		c, err := f.Listener.Accept()
		if err != nil {
			return nil, err
		}
		cred, cerr := peerCred(c)
		if cerr != nil || cred.UID == f.uid {
			return c, nil // credentials unreadable (fall back) or uid matches
		}
		if f.onReject != nil {
			f.onReject(cred)
		}
		_ = c.Close()
	}
}
