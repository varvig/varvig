//go:build linux

package peercred

import (
	"net"
	"syscall"
)

// peerCred reads SO_PEERCRED off the connection's file descriptor. It goes
// through SyscallConn().Control so it never takes ownership of the fd, and uses
// the standard library's syscall.GetsockoptUcred — no cgo.
func peerCred(conn net.Conn) (Cred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Cred{}, ErrNotUnix
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var cred *syscall.Ucred
	var opErr error
	if cerr := raw.Control(func(fd uintptr) {
		cred, opErr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); cerr != nil {
		return Cred{}, cerr
	}
	if opErr != nil {
		return Cred{}, opErr
	}
	return Cred{UID: int(cred.Uid), GID: int(cred.Gid), PID: int(cred.Pid)}, nil
}
