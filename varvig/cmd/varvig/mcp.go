package main

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/daemon"
	"github.com/dividebyzero/claude-experiments/varvig/internal/mcp"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/refs"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
	"github.com/dividebyzero/claude-experiments/varvig/internal/task"
	"github.com/dividebyzero/claude-experiments/varvig/internal/worktree"
)

// repoRuntimeDir is where a repo's daemon control socket and per-task sockets
// live. Unix socket paths are capped near 108 bytes (sun_path), so they cannot
// sit under a deep .varvig/ path; the design puts them at short runtime paths
// (auth design §6.1 "/run/varvig/…", §7.4 "~/.varvig/sock"). We use a short,
// per-uid runtime dir namespaced by a hash of the repo root, so `daemon` and
// `task start` independently derive the same path for the same repository.
func repoRuntimeDir(r *repo.Repo) string {
	base := os.Getenv("XDG_RUNTIME_DIR")
	if base == "" {
		base = os.TempDir()
	}
	h := fnv.New64a()
	if abs, err := filepath.Abs(r.Root()); err == nil {
		h.Write([]byte(abs))
	} else {
		h.Write([]byte(r.Root()))
	}
	return filepath.Join(base, fmt.Sprintf("varvig-%d", os.Getuid()), fmt.Sprintf("%016x", h.Sum64()))
}

func controlSocket(r *repo.Repo) string { return filepath.Join(repoRuntimeDir(r), "daemon.sock") }
func runDir(r *repo.Repo) string        { return filepath.Join(repoRuntimeDir(r), "run") }

// cmdDaemon runs or controls the long-running local daemon (auth design §6.1,
// §7.4). It holds task credentials in memory, keeps the repo warm, and serves
// the MCP gate on a per-task socket for each task it mints. Nothing is persisted;
// the keys live and die with the process.
//
//	varvig daemon [--socket PATH]   run until interrupted
//	varvig daemon status            report a running daemon's pid, uptime, task count
//	varvig daemon stop              ask a running daemon to exit
func cmdDaemon(args []string) error {
	if len(args) >= 1 && (args[0] == "status" || args[0] == "stop") {
		return daemonControl(args[0])
	}

	socket := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--socket":
			if i+1 >= len(args) {
				return errors.New("daemon: --socket requires a path")
			}
			socket, i = args[i+1], i+1
		default:
			return fmt.Errorf("daemon: unknown argument %q", args[i])
		}
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	if socket == "" {
		socket = controlSocket(r)
	}
	// Refuse to start a second daemon on the same socket: if one answers a ping,
	// bail with a clear message instead of a confusing bind error.
	if resp, derr := daemon.DialControl(socket, daemon.PingRequest()); derr == nil && resp.OK {
		return fmt.Errorf("daemon: already running (control socket %s responds to ping)", socket)
	}
	if err := os.MkdirAll(filepath.Dir(socket), 0o700); err != nil {
		return err
	}
	ln, err := readapi.ListenUnix(socket)
	if err != nil {
		return fmt.Errorf("daemon: cannot listen on %s: %w (already running?)", socket, err)
	}
	defer os.Remove(socket)

	d := daemon.New(r, runDir(r))
	defer d.Close()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	fmt.Fprintf(os.Stderr, "varvig daemon: control %s; per-task sockets under %s\n", socket, runDir(r))
	fmt.Fprintln(os.Stderr, "varvig daemon: ready — `varvig task start` mints a scoped, propose-only task")
	return d.ServeControl(ctx, ln)
}

// daemonControl implements `varvig daemon status` and `varvig daemon stop` by
// talking to a running daemon's control socket.
func daemonControl(op string) error {
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	sock := controlSocket(r)
	switch op {
	case "status":
		resp, err := daemon.DialControl(sock, daemon.StatusRequest())
		if err != nil {
			return fmt.Errorf("daemon status: no daemon running for this repo (%w)", err)
		}
		if !resp.OK || resp.Status == nil {
			return fmt.Errorf("daemon status: %s", resp.Error)
		}
		s := resp.Status
		fmt.Printf("running  pid %d  up since %s  tasks %d\n",
			s.Pid, time.Unix(s.StartedUnix, 0).Format(time.RFC3339), s.Tasks)
		fmt.Printf("control  %s\n", sock)
		fmt.Printf("run dir  %s\n", s.RunDir)
		return nil
	case "stop":
		resp, err := daemon.DialControl(sock, daemon.ShutdownRequest())
		if err != nil {
			return fmt.Errorf("daemon stop: no daemon running for this repo (%w)", err)
		}
		if !resp.OK {
			return fmt.Errorf("daemon stop: %s", resp.Error)
		}
		fmt.Println("daemon stopping")
		return nil
	default:
		return fmt.Errorf("daemon: unknown subcommand %q", op)
	}
}

// cmdMcp is the stdio entry point an agent harness spawns (auth design §8). It
// speaks the MCP JSON-RPC stream on stdin/stdout; human-facing output goes to
// stderr so it cannot corrupt the protocol channel. Three modes, in precedence:
//
//	varvig mcp --connect <task.sock>                  # bridge stdio to a specific task socket
//	varvig mcp [--scope S] [--ttl DUR] [--base REF]   # relay through the daemon (if one is up)
//	varvig mcp --standalone [...]                     # force an in-process gate, no daemon
//
// The default (no flags beyond scope/ttl) is the relay: if a daemon is running
// for the repo, mcp asks it to mint an ephemeral task, then bridges stdio to the
// per-task socket — so the credential and the warm repo live in the daemon, and
// the task is stopped when the client disconnects. With no daemon it falls back
// to a standalone in-process gate that mints its own key.
func cmdMcp(args []string) error {
	scope := "/"
	ttl := time.Hour
	base := ""
	connect := ""
	standalone := false
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--connect":
			if i+1 >= len(args) {
				return errors.New("mcp: --connect requires a socket path")
			}
			connect, i = args[i+1], i+1
		case "--standalone":
			standalone = true
		case "--scope":
			if i+1 >= len(args) {
				return errors.New("mcp: --scope requires a value")
			}
			scope, i = args[i+1], i+1
		case "--ttl":
			if i+1 >= len(args) {
				return errors.New("mcp: --ttl requires a value")
			}
			d, err := time.ParseDuration(args[i+1])
			if err != nil {
				return fmt.Errorf("mcp: bad --ttl: %w", err)
			}
			ttl, i = d, i+1
		case "--base":
			if i+1 >= len(args) {
				return errors.New("mcp: --base requires a value")
			}
			base, i = args[i+1], i+1
		case "--propose-only":
			// The only mode a task key has at v1; accepted for clarity.
		default:
			return fmt.Errorf("mcp: unknown argument %q", args[i])
		}
	}

	// Explicit bridge to a specific per-task socket.
	if connect != "" {
		fmt.Fprintf(os.Stderr, "varvig mcp: bridging stdio to %s\n", connect)
		return daemon.Bridge(stdio{}, connect)
	}

	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	baseID, err := resolveBase(r, base)
	if err != nil {
		return err
	}

	// Relay through the daemon when one is running (the default): mint an
	// ephemeral task there and bridge stdio to it, so the key stays in the
	// daemon and the repo stays warm. Stop the task when the client disconnects.
	if !standalone {
		sock := controlSocket(r)
		if resp, derr := daemon.DialControl(sock, daemon.PingRequest()); derr == nil && resp.OK {
			sresp, err := daemon.DialControl(sock, daemon.StartRequest(scope, ttl.String(), hexOrEmpty(baseID)))
			if err != nil {
				return fmt.Errorf("mcp: daemon start: %w", err)
			}
			if !sresp.OK || sresp.Task == nil {
				return fmt.Errorf("mcp: daemon refused: %s", sresp.Error)
			}
			t := sresp.Task
			fmt.Fprintf(os.Stderr, "varvig mcp: relaying task %s scope %s via daemon (key %s, expires %s)\n",
				t.ID, t.Scope, t.Fingerprint, time.Unix(t.Expires, 0).Format(time.Kitchen))
			defer daemon.DialControl(sock, daemon.StopRequest(t.ID)) // clean up the ephemeral relay task
			return daemon.Bridge(stdio{}, t.Socket)
		}
	}

	// Standalone: an in-process gate minting its own key (no daemon, or forced).
	grant, err := task.New(scope, true, ttl, time.Now())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "varvig mcp: standalone task %s scope %s key %s (expires %s)\n",
		grant.ID, grant.Scope, grant.Fingerprint(),
		time.Unix(grant.NotAfter, 0).Format(time.Kitchen))
	fmt.Fprintf(os.Stderr, "varvig mcp: base %s; propose-only, cannot promote\n", hexOrNone(baseID))

	gate := mcp.NewGate(r, grant, baseID)
	return gate.Serve(stdio{})
}

// cmdTask manages task credentials (auth design §6): `start`, `list`, `stop`.
// When a daemon is running for the repo, `task start` mints the grant *in the
// daemon* — so the ephemeral key persists for the task's life and the per-task
// socket it returns is immediately usable. Without a daemon it falls back to a
// standalone grant whose key lives only with a later `varvig mcp` process.
//
//	varvig task start [--scope S] [--ttl DUR] [--base REF] [dir]
//	varvig task list
//	varvig task stop <id>
func cmdTask(args []string) error {
	if len(args) < 1 {
		return errors.New("usage: varvig task <start|list|stop> ...")
	}
	r, err := repo.Open(".")
	if err != nil {
		return err
	}
	switch args[0] {
	case "start":
		return taskStart(r, args[1:])
	case "list":
		return taskList(r)
	case "stop":
		if len(args) != 2 {
			return errors.New("usage: varvig task stop <id>")
		}
		return taskStop(r, args[1])
	default:
		return fmt.Errorf("task: unknown subcommand %q (want: start, list, stop)", args[0])
	}
}

func taskStart(r *repo.Repo, args []string) error {
	scope := "/"
	ttl := time.Hour
	base := ""
	dir := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--scope":
			if i+1 >= len(args) {
				return errors.New("task start: --scope requires a value")
			}
			scope, i = args[i+1], i+1
		case "--ttl":
			if i+1 >= len(args) {
				return errors.New("task start: --ttl requires a value")
			}
			if _, err := time.ParseDuration(args[i+1]); err != nil {
				return fmt.Errorf("task start: bad --ttl: %w", err)
			}
			ttl, _ = time.ParseDuration(args[i+1])
			i++
		case "--base":
			if i+1 >= len(args) {
				return errors.New("task start: --base requires a value")
			}
			base, i = args[i+1], i+1
		case "--propose-only":
			// The only mode at v1.
		default:
			dir = args[i]
		}
	}

	baseID, err := resolveBase(r, base)
	if err != nil {
		return err
	}

	// If a daemon is up, mint the grant there so the key persists and the socket
	// it returns is live. Otherwise mint a standalone grant for a later `mcp`.
	sock := controlSocket(r)
	daemonUp := false
	if resp, derr := daemon.DialControl(sock, daemon.PingRequest()); derr == nil && resp.OK {
		daemonUp = true
	}

	var id, fp, socketDesc, scopeDisplay string
	var expires int64
	if daemonUp {
		resp, err := daemon.DialControl(sock, daemon.StartRequest(scope, ttl.String(), hexOrEmpty(baseID)))
		if err != nil {
			return fmt.Errorf("task start: daemon: %w", err)
		}
		if !resp.OK || resp.Task == nil {
			return fmt.Errorf("task start: daemon refused: %s", resp.Error)
		}
		id, fp, expires, scopeDisplay = resp.Task.ID, resp.Task.Fingerprint, resp.Task.Expires, resp.Task.Scope
		socketDesc = resp.Task.Socket
		if resp.Task.Base != "" {
			if b, err := multihash.ParseHex(resp.Task.Base); err == nil {
				baseID = b
			}
		}
	} else {
		grant, err := task.New(scope, true, ttl, time.Now())
		if err != nil {
			return err
		}
		id, fp, expires, scopeDisplay = grant.ID, grant.Fingerprint(), grant.NotAfter, string(grant.Scope)
		socketDesc = "(no daemon) run: varvig mcp --scope " + scope + " --ttl " + ttl.String()
	}
	if dir == "" {
		dir = "./task-" + id
	}

	// Sparse checkout of the read set: scope equals the checkout equals the API's
	// visibility (§6.2). Materialize only the scope subtree at its repo-relative
	// path, so proposed paths line up with what the gate enforces.
	checkoutDesc := "(none — empty repo)"
	if baseID != nil {
		q := readapi.New(r)
		scopePath := trimScope(scope)
		listing, err := q.Tree(baseID, scopePath)
		if err != nil {
			return fmt.Errorf("task start: cannot resolve scope %q: %w", scope, err)
		}
		subTree, err := multihash.ParseHex(listing.Hash)
		if err != nil {
			return err
		}
		dest := dir
		if scopePath != "" {
			dest = filepath.Join(dir, filepath.FromSlash(scopePath))
		}
		if err := worktree.Checkout(r.Objects, subTree, dest); err != nil {
			return err
		}
		checkoutDesc = dest
	}

	fmt.Printf("task %s\n", id)
	fmt.Printf("  scope     %s\n", scopeDisplay)
	fmt.Printf("  key       %s  (ephemeral, expires %s)\n", fp, time.Unix(expires, 0).Format(time.Kitchen))
	fmt.Printf("  base      %s\n", hexOrNone(baseID))
	fmt.Printf("  checkout  %s\n", checkoutDesc)
	if daemonUp {
		fmt.Printf("  socket    %s\n", socketDesc)
		fmt.Printf("  connect   varvig mcp --connect %s   # stdio bridge for MCP clients\n", socketDesc)
		fmt.Fprintln(os.Stderr, "note: minted in the daemon — the ephemeral key persists for the task's life")
	} else {
		fmt.Printf("  mcp       %s\n", socketDesc)
		fmt.Fprintln(os.Stderr, "note: no daemon running (`varvig daemon &`); `varvig mcp` will mint its own scoped key")
	}
	fmt.Fprintln(os.Stderr, "note: propose-only — a task key can create proposals but never promote a ref")
	return nil
}

func taskList(r *repo.Repo) error {
	resp, err := daemon.DialControl(controlSocket(r), daemon.ListRequest())
	if err != nil {
		return fmt.Errorf("task list: no daemon running for this repo (%w)", err)
	}
	if len(resp.Tasks) == 0 {
		fmt.Println("no active tasks")
		return nil
	}
	for _, t := range resp.Tasks {
		fmt.Printf("%s  %-10s  %s  expires %s\n  %s\n",
			t.ID, t.Scope, t.Fingerprint, time.Unix(t.Expires, 0).Format(time.Kitchen), t.Socket)
	}
	return nil
}

func taskStop(r *repo.Repo, id string) error {
	resp, err := daemon.DialControl(controlSocket(r), daemon.StopRequest(id))
	if err != nil {
		return fmt.Errorf("task stop: no daemon running for this repo (%w)", err)
	}
	if !resp.OK {
		return fmt.Errorf("task stop: %s", resp.Error)
	}
	fmt.Printf("stopped task %s\n", id)
	return nil
}

// resolveBase resolves the task's base state: an explicit ref/hash, else HEAD.
// A repository with no commits yet resolves to a nil base (an empty tree).
func resolveBase(r *repo.Repo, ref string) (multihash.Multihash, error) {
	if ref != "" {
		return resolve(r, ref)
	}
	headRef, err := r.Head()
	if err != nil {
		return nil, err
	}
	id, err := r.Refs.Resolve(headRef)
	if errors.Is(err, refs.ErrNotExist) {
		return nil, nil
	}
	return id, err
}

func trimScope(scope string) string {
	s := scope
	for len(s) > 0 && s[0] == '/' {
		s = s[1:]
	}
	for len(s) > 0 && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	if s == "." {
		return ""
	}
	return s
}

func hexOrNone(m multihash.Multihash) string {
	if m == nil {
		return "(none)"
	}
	return m.Hex()
}

func hexOrEmpty(m multihash.Multihash) string {
	if m == nil {
		return ""
	}
	return m.Hex()
}

// stdio is an io.ReadWriter bridging the process's stdin and stdout, for the MCP
// stdio transport.
type stdio struct{}

func (stdio) Read(b []byte) (int, error)  { return os.Stdin.Read(b) }
func (stdio) Write(b []byte) (int, error) { return os.Stdout.Write(b) }
