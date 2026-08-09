//go:build darwin || freebsd

package peercred

import (
	"net"

	"golang.org/x/sys/unix"
)

// peerCred reads the peer's credentials via LOCAL_PEERCRED, which returns a
// struct xucred (auth design §7.4). macOS and FreeBSD expose the effective uid
// and groups this way; LOCAL_PEERCRED carries no peer pid, so PID is left zero.
//
// It goes through SyscallConn().Control so it never takes ownership of the fd,
// and uses x/sys/unix.GetsockoptXucred so the per-platform xucred layout is
// correct — still cgo-free.
func peerCred(conn net.Conn) (Cred, error) {
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return Cred{}, ErrNotUnix
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return Cred{}, err
	}
	var xu *unix.Xucred
	var opErr error
	if cerr := raw.Control(func(fd uintptr) {
		xu, opErr = unix.GetsockoptXucred(int(fd), unix.SOL_LOCAL, unix.LOCAL_PEERCRED)
	}); cerr != nil {
		return Cred{}, cerr
	}
	if opErr != nil {
		return Cred{}, opErr
	}
	gid := 0
	if xu.Ngroups > 0 {
		gid = int(xu.Groups[0]) // cr_groups[0] is the effective gid
	}
	return Cred{UID: int(xu.Uid), GID: gid, PID: 0}, nil
}
