//go:build !linux

package peercred

import "net"

// peerCred has no portable implementation off Linux. macOS/BSD would use
// getpeereid / LOCAL_PEERCRED; until that is added, callers fall back to the
// socket's 0600 mode (auth design §7.4). Returning ErrUnsupported keeps the
// single portable binary cross-compiling everywhere.
func peerCred(conn net.Conn) (Cred, error) { return Cred{}, ErrUnsupported }
