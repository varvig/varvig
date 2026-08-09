package readapi

import (
	"errors"
	"net"
	"net/http"
	"os"
	"time"
)

// ListenUnix creates a Unix-domain socket at path with mode 0600. On this
// server, filesystem permissions *are* the authentication (auth design §7.4):
// only the owning uid can open a 0600 socket. A stale socket left by a previous
// run is removed first.
//
// A kernel-backed peer-uid check (§7.4) layers on top of the 0600 mode: the
// caller wraps this listener with peercred.FilterListener (see cmd/varvig
// serveReadOnly), which is cgo-free on Linux/macOS/FreeBSD and falls back to the
// mode alone where the OS has no equivalent.
func ListenUnix(path string) (net.Listener, error) {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	ln, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		ln.Close()
		return nil, err
	}
	return ln, nil
}

// Serve runs the read-only HTTP API over ln until it closes. It is used with
// both a Unix listener (the default) and, on explicit opt-in, a TCP listener.
func Serve(q *Query, ln net.Listener) error {
	srv := &http.Server{
		Handler:           Handler(q),
		ReadHeaderTimeout: 10 * time.Second,
	}
	return srv.Serve(ln)
}
