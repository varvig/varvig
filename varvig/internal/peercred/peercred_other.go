//go:build !linux && !darwin && !freebsd

package peercred

import "net"

// peerCred has no implementation on the remaining platforms (Windows, and the
// BSDs without LOCAL_PEERCRED such as OpenBSD/NetBSD). Callers fall back to the
// socket's 0600 mode (auth design §7.4). Returning ErrUnsupported keeps the
// single portable binary cross-compiling everywhere.
func peerCred(conn net.Conn) (Cred, error) { return Cred{}, ErrUnsupported }
