// Package daemon is the long-running local process that holds task credentials
// in memory and serves the MCP gate for each task (auth design §6.1, §7.4). It
// is what makes `varvig task start` and `varvig mcp` two halves of one flow
// rather than two disconnected processes each minting their own key.
//
// The model (auth design §6.1, §10.5):
//
//   - One daemon per repository keeps the repo open (warm indices, §7.1) and
//     holds an in-memory table of grants. Nothing is persisted: the ephemeral
//     keys live and die with the process, so there is no credential store to leak.
//   - `task start` asks the daemon to mint a grant. The daemon generates the
//     ephemeral key, records it, and opens a per-task Unix socket. The key never
//     touches disk and never crosses the wire — it stays in the daemon's memory
//     and is used there to sign the task's proposals (§6.1).
//   - The agent connects an MCP client to that per-task socket; the daemon serves
//     the same mcp.Gate over the connection, bound to the task's grant. The
//     socket's 0600 mode is the authentication (§7.4).
//   - Expiry does the revocation work (§6.2): a reaper prunes expired grants,
//     closes their sockets, and forgets their keys. No revocation infrastructure
//     is needed for the common case.
//
// The daemon holds no authority of its own beyond the repo it opened; each
// connection is confined to its grant's scope by the gate (§8, confused deputy).
package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/mcp"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/peercred"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
)

// TaskInfo is the daemon's description of a live task, returned to `task start`
// and `task list`.
type TaskInfo struct {
	ID          string `json:"id"`
	Fingerprint string `json:"fingerprint"`
	Scope       string `json:"scope"`
	Socket      string `json:"socket"`
	Base        string `json:"base,omitempty"`
	Expires     int64  `json:"expires"`
}

// Daemon holds the grant table and per-task MCP sockets for one repository.
type Daemon struct {
	repo   *repo.Repo
	table  *task.Table
	runDir string // directory holding per-task sockets (0700)

	now func() time.Time // injectable clock; nil defaults to time.Now

	// allowUID is the only uid permitted to connect to the control and per-task
	// sockets, confirmed by SO_PEERCRED (auth design §7.4). It defaults to the
	// daemon's own uid; tests set it to force rejection.
	allowUID int

	mu      sync.Mutex
	tasks   map[string]*taskServer
	closed  bool
	started int64              // unix seconds the daemon came up
	cancel  context.CancelFunc // set while ServeControl runs; shutdown op calls it
}

type taskServer struct {
	grant  *task.Grant
	base   multihash.Multihash
	ln     net.Listener
	socket string
}

// New creates a daemon over repo r, placing per-task sockets under runDir.
func New(r *repo.Repo, runDir string) *Daemon {
	d := &Daemon{repo: r, table: task.NewTable(), runDir: runDir, tasks: map[string]*taskServer{}, allowUID: os.Getuid()}
	d.started = d.clock().Unix()
	return d
}

// SetClock overrides the daemon's clock (tests pin expiry). It also propagates
// to the gates served for each task, so expiry is consistent end to end.
func (d *Daemon) SetClock(now func() time.Time) { d.now = now }

// SetAllowUID overrides the uid permitted to connect (default: the daemon's own
// uid). Tests use it to force a peer-credential rejection.
func (d *Daemon) SetAllowUID(uid int) { d.allowUID = uid }

// guard wraps a Unix listener so only the allowed uid may connect, confirmed by
// SO_PEERCRED. Where peer credentials cannot be read (unsupported platform), the
// wrapper passes connections through and the 0600 mode remains the guard (§7.4).
func (d *Daemon) guard(ln net.Listener) net.Listener {
	return peercred.FilterListener(ln, d.allowUID, func(c peercred.Cred) {
		fmt.Fprintf(os.Stderr, "varvig daemon: rejected connection from uid %d (allow %d)\n", c.UID, d.allowUID)
	})
}

func (d *Daemon) clock() time.Time {
	if d.now != nil {
		return d.now()
	}
	return time.Now()
}

// StartTask mints a scoped, propose-only, expiring grant, opens its per-task
// socket, and begins serving the MCP gate on it. It is safe for concurrent use.
func (d *Daemon) StartTask(scope string, ttl time.Duration, base multihash.Multihash) (TaskInfo, error) {
	grant, err := task.New(scope, true, ttl, d.clock())
	if err != nil {
		return TaskInfo{}, err
	}
	if err := os.MkdirAll(d.runDir, 0o700); err != nil {
		return TaskInfo{}, err
	}
	socket := filepath.Join(d.runDir, "task-"+grant.ID+".sock")
	rawLn, err := readapi.ListenUnix(socket) // 0600: filesystem perms are the auth (§7.4)
	if err != nil {
		return TaskInfo{}, err
	}
	ln := d.guard(rawLn) // SO_PEERCRED: only the allowed uid may connect (§7.4)
	ts := &taskServer{grant: grant, base: base, ln: ln, socket: socket}

	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		ln.Close()
		os.Remove(socket)
		return TaskInfo{}, fmt.Errorf("daemon: closed")
	}
	d.tasks[grant.ID] = ts
	d.table.Add(grant)
	d.mu.Unlock()

	go d.acceptLoop(ts)
	return d.infoOf(ts), nil
}

// acceptLoop serves the MCP gate for every connection to a task's socket. One
// grant can serve many sequential connections over its life; they share the
// grant's read set, so provenance accumulates across the whole task.
func (d *Daemon) acceptLoop(ts *taskServer) {
	for {
		conn, err := ts.ln.Accept()
		if err != nil {
			return // listener closed by StopTask/Reap/Close
		}
		go func() {
			defer conn.Close()
			gate := mcp.NewGate(d.repo, ts.grant, ts.base)
			if d.now != nil {
				gate.SetClock(d.now)
			}
			_ = gate.Serve(conn)
		}()
	}
}

// StatusInfo summarizes a running daemon.
type StatusInfo struct {
	Pid         int    `json:"pid"`
	StartedUnix int64  `json:"started"`
	Tasks       int    `json:"tasks"`
	RunDir      string `json:"run_dir"`
}

// Status reports the daemon's live task count and uptime anchor. It reaps
// expired tasks first so the count reflects reality.
func (d *Daemon) Status() StatusInfo {
	d.Reap()
	d.mu.Lock()
	defer d.mu.Unlock()
	return StatusInfo{Pid: os.Getpid(), StartedUnix: d.started, Tasks: len(d.tasks), RunDir: d.runDir}
}

// ListTasks returns every live task, pruning any that have expired first.
func (d *Daemon) ListTasks() []TaskInfo {
	d.Reap()
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]TaskInfo, 0, len(d.tasks))
	for _, ts := range d.tasks {
		out = append(out, d.infoOf(ts))
	}
	return out
}

// StopTask revokes a task early: it closes the socket (no new connections) and
// forgets the grant. Existing connections end when their stream closes.
func (d *Daemon) StopTask(id string) error {
	d.mu.Lock()
	ts, ok := d.tasks[id]
	if ok {
		delete(d.tasks, id)
	}
	d.mu.Unlock()
	if !ok {
		return fmt.Errorf("daemon: no such task %q", id)
	}
	d.table.Remove(id)
	ts.ln.Close()
	os.Remove(ts.socket)
	return nil
}

// Reap prunes expired grants — closing their sockets and forgetting their keys —
// and returns how many were reaped. Expiry is the revocation mechanism (§6.2).
func (d *Daemon) Reap() int {
	now := d.clock()
	d.mu.Lock()
	var dead []*taskServer
	for id, ts := range d.tasks {
		if !ts.grant.Valid(now) {
			dead = append(dead, ts)
			delete(d.tasks, id)
			d.table.Remove(id)
		}
	}
	d.mu.Unlock()
	for _, ts := range dead {
		ts.ln.Close()
		os.Remove(ts.socket)
	}
	return len(dead)
}

// Close shuts the daemon down: it stops accepting on every task socket and
// removes them. The repo is left open for the caller to close.
func (d *Daemon) Close() error {
	d.mu.Lock()
	d.closed = true
	tss := make([]*taskServer, 0, len(d.tasks))
	for id, ts := range d.tasks {
		tss = append(tss, ts)
		delete(d.tasks, id)
	}
	d.mu.Unlock()
	for _, ts := range tss {
		ts.ln.Close()
		os.Remove(ts.socket)
	}
	return nil
}

func (d *Daemon) infoOf(ts *taskServer) TaskInfo {
	info := TaskInfo{
		ID:          ts.grant.ID,
		Fingerprint: ts.grant.Fingerprint(),
		Scope:       ts.grant.Scopes.String(),
		Socket:      ts.socket,
		Expires:     ts.grant.NotAfter,
	}
	if ts.base != nil {
		info.Base = ts.base.Hex()
	}
	return info
}

// --- control protocol ---

// ctrlRequest is one line of the control protocol on the daemon's control
// socket. It is deliberately tiny: mint, list, stop, ping.
type ctrlRequest struct {
	Op    string `json:"op"`
	Scope string `json:"scope,omitempty"`
	TTL   string `json:"ttl,omitempty"`  // a Go duration string
	Base  string `json:"base,omitempty"` // a hex hash; empty means the daemon's HEAD
	ID    string `json:"id,omitempty"`
}

// ctrlResponse is the reply to a ctrlRequest.
type ctrlResponse struct {
	OK     bool        `json:"ok"`
	Error  string      `json:"error,omitempty"`
	Task   *TaskInfo   `json:"task,omitempty"`
	Tasks  []TaskInfo  `json:"tasks,omitempty"`
	Status *StatusInfo `json:"status,omitempty"`
}

// ServeControl runs the control protocol over ln and a background reaper until
// ctx is canceled or ln closes. Each connection carries one or more requests as
// newline-delimited JSON; each gets one JSON response.
func (d *Daemon) ServeControl(ctx context.Context, ln net.Listener) error {
	// A derived context so a "shutdown" control op can stop the daemon from the
	// inside, in addition to the caller canceling ctx (e.g. on SIGINT).
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	d.mu.Lock()
	d.cancel = cancel
	d.mu.Unlock()

	// Only the allowed uid may drive the control protocol (§7.4).
	ln = d.guard(ln)

	// Background reaper: expiry does the revocation work without any client call.
	reaper := time.NewTicker(30 * time.Second)
	defer reaper.Stop()
	go func() {
		for {
			select {
			case <-ctx.Done():
				return
			case <-reaper.C:
				d.Reap()
			}
		}
	}()
	// Close the listener when the context is canceled so Accept returns.
	go func() { <-ctx.Done(); ln.Close() }()

	for {
		conn, err := ln.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		go d.serveControlConn(conn)
	}
}

func (d *Daemon) serveControlConn(conn net.Conn) {
	defer conn.Close()
	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)
	for {
		var req ctrlRequest
		if err := dec.Decode(&req); err != nil {
			return
		}
		_ = enc.Encode(d.handleControl(req))
	}
}

func (d *Daemon) handleControl(req ctrlRequest) ctrlResponse {
	switch req.Op {
	case "ping":
		return ctrlResponse{OK: true}
	case "start":
		ttl := time.Hour
		if req.TTL != "" {
			parsed, err := time.ParseDuration(req.TTL)
			if err != nil {
				return ctrlResponse{Error: "bad ttl: " + err.Error()}
			}
			ttl = parsed
		}
		base, err := d.resolveBase(req.Base)
		if err != nil {
			return ctrlResponse{Error: err.Error()}
		}
		info, err := d.StartTask(req.Scope, ttl, base)
		if err != nil {
			return ctrlResponse{Error: err.Error()}
		}
		return ctrlResponse{OK: true, Task: &info}
	case "list":
		return ctrlResponse{OK: true, Tasks: d.ListTasks()}
	case "stop":
		if err := d.StopTask(req.ID); err != nil {
			return ctrlResponse{Error: err.Error()}
		}
		return ctrlResponse{OK: true}
	case "status":
		s := d.Status()
		return ctrlResponse{OK: true, Status: &s}
	case "shutdown":
		d.mu.Lock()
		cancel := d.cancel
		d.mu.Unlock()
		if cancel != nil {
			cancel() // stops ServeControl's accept loop; the process then exits
		}
		return ctrlResponse{OK: true}
	default:
		return ctrlResponse{Error: "unknown op: " + req.Op}
	}
}

// resolveBase resolves a task's base: an explicit hex hash, else the daemon
// repo's HEAD. A repository with no commits resolves to a nil base.
func (d *Daemon) resolveBase(hexHash string) (multihash.Multihash, error) {
	if hexHash != "" {
		return multihash.ParseHex(hexHash)
	}
	headRef, err := d.repo.Head()
	if err != nil {
		return nil, err
	}
	id, err := d.repo.Refs.Resolve(headRef)
	if err != nil {
		return nil, nil // no commits yet: empty base
	}
	return id, nil
}

// Bridge copies bytes both ways between a local stream (typically stdio) and the
// daemon's per-task socket, so an MCP client that only speaks stdio can reach a
// daemon-hosted task.
//
// Shutdown is drain-correct: when the local side reaches EOF (the client closed
// its request stream) we half-close the write half of the socket to signal EOF
// to the gate, then keep copying the gate's replies until it closes the socket.
// Returning on the first finished copy instead would truncate the final
// responses — the bug a naive bidirectional pipe has.
func Bridge(local io.ReadWriter, socketPath string) error {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return err
	}
	defer conn.Close()
	go func() {
		_, _ = io.Copy(conn, local) // local -> socket, until local EOF
		if uc, ok := conn.(*net.UnixConn); ok {
			_ = uc.CloseWrite() // signal EOF to the gate; keep reading replies
		}
	}()
	_, err = io.Copy(local, conn) // socket -> local, until the gate closes
	return err
}

// DialControl connects to a daemon control socket and sends one request,
// returning the response. It is the client half used by the CLI.
func DialControl(socketPath string, req ctrlRequest) (ctrlResponse, error) {
	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		return ctrlResponse{}, err
	}
	defer conn.Close()
	if err := json.NewEncoder(conn).Encode(req); err != nil {
		return ctrlResponse{}, err
	}
	var resp ctrlResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		return ctrlResponse{}, err
	}
	return resp, nil
}

// Client-side request constructors keep the ctrlRequest shape private while
// letting the CLI drive the protocol.

// StartRequest builds a "start" control request.
func StartRequest(scope, ttl, base string) ctrlRequest {
	return ctrlRequest{Op: "start", Scope: scope, TTL: ttl, Base: base}
}

// ListRequest builds a "list" control request.
func ListRequest() ctrlRequest { return ctrlRequest{Op: "list"} }

// StopRequest builds a "stop" control request.
func StopRequest(id string) ctrlRequest { return ctrlRequest{Op: "stop", ID: id} }

// PingRequest builds a "ping" control request.
func PingRequest() ctrlRequest { return ctrlRequest{Op: "ping"} }

// StatusRequest builds a "status" control request.
func StatusRequest() ctrlRequest { return ctrlRequest{Op: "status"} }

// ShutdownRequest builds a "shutdown" control request, asking the daemon to exit.
func ShutdownRequest() ctrlRequest { return ctrlRequest{Op: "shutdown"} }
