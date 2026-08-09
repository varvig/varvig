package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
	"github.com/dividebyzero/claude-experiments/varvig/internal/object"
	"github.com/dividebyzero/claude-experiments/varvig/internal/peercred"
	"github.com/dividebyzero/claude-experiments/varvig/internal/readapi"
	"github.com/dividebyzero/claude-experiments/varvig/internal/repo"
)

func newRepo(t *testing.T) (*repo.Repo, multihash.Multihash) {
	t.Helper()
	r, err := repo.Init(filepath.Join(t.TempDir(), "repo"))
	if err != nil {
		t.Fatal(err)
	}
	put := func(o *object.Object) multihash.Multihash {
		id, err := r.Objects.Put(o)
		if err != nil {
			t.Fatal(err)
		}
		return id
	}
	login := put(object.NewBlob([]byte("package auth\n")))
	authTree := put(object.NewTree([]object.Entry{
		{Name: "login.go", Mode: 0o100644, Kind: object.TypeBlob, ID: login},
	}))
	srcTree := put(object.NewTree([]object.Entry{
		{Name: "auth", Mode: 0o40000, Kind: object.TypeTree, ID: authTree},
	}))
	root := put(object.NewTree([]object.Entry{
		{Name: "src", Mode: 0o40000, Kind: object.TypeTree, ID: srcTree},
	}))
	base := put(object.NewChange(object.Change{Tree: root, Message: "init", Author: "jan", Timestamp: 100}))
	if err := r.Refs.Create("refs/heads/main", base, "test", "seed"); err != nil {
		t.Fatal(err)
	}
	return r, base
}

// callTool connects to a per-task socket and drives one MCP tools/call after the
// initialize handshake, returning the decoded tool result envelope.
func callTool(t *testing.T, socket, callJSON string) map[string]any {
	t.Helper()
	conn, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial task socket: %v", err)
	}
	defer conn.Close()
	enc := json.NewEncoder(conn)
	dec := json.NewDecoder(conn)
	if err := enc.Encode(json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	var initResp map[string]any
	if err := dec.Decode(&initResp); err != nil {
		t.Fatalf("initialize: %v", err)
	}
	if err := enc.Encode(json.RawMessage(callJSON)); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result map[string]any `json:"result"`
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("tools/call: %v", err)
	}
	return resp.Result
}

func TestStartTaskServesScopedGate(t *testing.T) {
	r, base := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()

	info, err := d.StartTask("src/auth", time.Hour, base)
	if err != nil {
		t.Fatalf("StartTask: %v", err)
	}
	if info.Socket == "" || info.Fingerprint == "" {
		t.Fatal("task info missing socket or fingerprint")
	}

	// A read within scope over the per-task socket succeeds.
	res := callTool(t, info.Socket, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fetch_blob","arguments":{"path":"src/auth/login.go"}}}`)
	if res["isError"] == true {
		t.Fatalf("in-scope read errored: %v", res["content"])
	}
	sc, _ := res["structuredContent"].(map[string]any)
	if sc["content"] != "package auth\n" {
		t.Fatalf("content = %v, want package auth", sc["content"])
	}

	// A read outside scope is refused by the gate the daemon serves.
	bad := callTool(t, info.Socket, `{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"fetch_blob","arguments":{"path":"src/other.go"}}}`)
	if bad["isError"] != true {
		t.Fatal("out-of-scope read should be refused")
	}
}

func TestReapClosesExpiredTask(t *testing.T) {
	r, base := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()

	now := time.Unix(1000, 0)
	d.SetClock(func() time.Time { return now })
	info, err := d.StartTask("/", time.Minute, base)
	if err != nil {
		t.Fatal(err)
	}
	if len(d.ListTasks()) != 1 {
		t.Fatal("task should be live before expiry")
	}

	// Advance past expiry; the reaper drops it and closes the socket.
	now = time.Unix(1000+61, 0)
	if n := d.Reap(); n != 1 {
		t.Fatalf("Reap returned %d, want 1", n)
	}
	if len(d.ListTasks()) != 0 {
		t.Fatal("expired task should be gone")
	}
	if _, err := net.Dial("unix", info.Socket); err == nil {
		t.Fatal("expired task socket should no longer accept connections")
	}
}

func TestStopTaskRevokes(t *testing.T) {
	r, base := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()

	info, err := d.StartTask("/", time.Hour, base)
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StopTask(info.ID); err != nil {
		t.Fatalf("StopTask: %v", err)
	}
	if len(d.ListTasks()) != 0 {
		t.Fatal("stopped task should be gone")
	}
	if err := d.StopTask(info.ID); err == nil {
		t.Fatal("stopping an unknown task should error")
	}
}

func TestControlProtocolStartAndList(t *testing.T) {
	r, _ := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()

	ctrlSock := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := readapi.ListenUnix(ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.ServeControl(ctx, ln)

	// ping
	if resp, err := DialControl(ctrlSock, PingRequest()); err != nil || !resp.OK {
		t.Fatalf("ping failed: %+v %v", resp, err)
	}

	// start (base defaults to the daemon repo's HEAD)
	resp, err := DialControl(ctrlSock, StartRequest("src/auth", "30m", ""))
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if !resp.OK || resp.Task == nil {
		t.Fatalf("start not ok: %+v", resp)
	}
	if resp.Task.Base == "" {
		t.Error("start should default base to HEAD")
	}
	socket := resp.Task.Socket

	// the returned socket serves the scoped gate
	res := callTool(t, socket, `{"jsonrpc":"2.0","id":2,"method":"tools/call","params":{"name":"fetch_blob","arguments":{"path":"src/auth/login.go"}}}`)
	if res["isError"] == true {
		t.Fatalf("gate read over daemon-minted task failed: %v", res)
	}

	// list shows the task
	lresp, err := DialControl(ctrlSock, ListRequest())
	if err != nil {
		t.Fatal(err)
	}
	if len(lresp.Tasks) != 1 || lresp.Tasks[0].ID != resp.Task.ID {
		t.Fatalf("list = %+v, want the one started task", lresp.Tasks)
	}

	// stop revokes it
	if sresp, err := DialControl(ctrlSock, StopRequest(resp.Task.ID)); err != nil || !sresp.OK {
		t.Fatalf("stop failed: %+v %v", sresp, err)
	}
	lresp2, _ := DialControl(ctrlSock, ListRequest())
	if len(lresp2.Tasks) != 0 {
		t.Fatal("task should be gone after stop")
	}
}

func TestStatusAndShutdown(t *testing.T) {
	r, _ := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()

	ctrlSock := filepath.Join(t.TempDir(), "daemon.sock")
	ln, err := readapi.ListenUnix(ctrlSock)
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- d.ServeControl(context.Background(), ln) }()

	// status reflects the live task count.
	if _, err := DialControl(ctrlSock, StartRequest("/", "1h", "")); err != nil {
		t.Fatal(err)
	}
	sresp, err := DialControl(ctrlSock, StatusRequest())
	if err != nil || sresp.Status == nil {
		t.Fatalf("status failed: %+v %v", sresp, err)
	}
	if sresp.Status.Tasks != 1 {
		t.Fatalf("status tasks = %d, want 1", sresp.Status.Tasks)
	}
	if sresp.Status.Pid == 0 {
		t.Error("status should report a pid")
	}

	// shutdown makes ServeControl return and the socket stop answering.
	if resp, err := DialControl(ctrlSock, ShutdownRequest()); err != nil || !resp.OK {
		t.Fatalf("shutdown failed: %+v %v", resp, err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ServeControl returned %v, want nil after shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ServeControl did not return after shutdown")
	}
	if _, err := DialControl(ctrlSock, PingRequest()); err == nil {
		t.Fatal("control socket should be closed after shutdown")
	}
}

func TestPeerCredRejectsMismatchedUID(t *testing.T) {
	r, base := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()
	// Allow an impossible uid so this test process's own connections are refused
	// by SO_PEERCRED (on Linux). Off Linux the check is a no-op and we skip.
	d.SetAllowUID(os.Getuid() + 99999)

	info, err := d.StartTask("/", time.Hour, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, uerr := peercred.Of(mustDialUnix(t, info.Socket)); errors.Is(uerr, peercred.ErrUnsupported) {
		t.Skip("SO_PEERCRED unsupported on this platform")
	}

	// The per-task socket accepts the TCP-less connection then closes it (the
	// guard rejects the uid), so an MCP request gets no response before EOF.
	conn := mustDialUnix(t, info.Socket)
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	_, _ = conn.Write([]byte(`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{}}` + "\n"))
	buf := make([]byte, 64)
	if n, err := conn.Read(buf); err == nil && n > 0 {
		t.Fatalf("expected the rejected connection to be closed, got %d bytes: %q", n, buf[:n])
	}
}

func mustDialUnix(t *testing.T, socket string) net.Conn {
	t.Helper()
	c, err := net.Dial("unix", socket)
	if err != nil {
		t.Fatalf("dial %s: %v", socket, err)
	}
	return c
}

func TestBridgeStdioToTaskSocket(t *testing.T) {
	r, base := newRepo(t)
	d := New(r, filepath.Join(t.TempDir(), "run"))
	defer d.Close()
	info, err := d.StartTask("src/auth", time.Hour, base)
	if err != nil {
		t.Fatal(err)
	}

	// Bridge one JSON-RPC exchange through a pipe pair standing in for stdio.
	clientConn, bridgeSide := net.Pipe()
	go func() { _ = Bridge(bridgeSide, info.Socket) }()
	defer clientConn.Close()

	enc := json.NewEncoder(clientConn)
	dec := json.NewDecoder(clientConn)
	if err := enc.Encode(json.RawMessage(`{"jsonrpc":"2.0","id":1,"method":"tools/list","params":{}}`)); err != nil {
		t.Fatal(err)
	}
	var resp struct {
		Result struct {
			Tools []struct {
				Name string `json:"name"`
			} `json:"tools"`
		} `json:"result"`
	}
	if err := dec.Decode(&resp); err != nil {
		t.Fatalf("bridged tools/list: %v", err)
	}
	if len(resp.Result.Tools) == 0 {
		t.Fatal("bridge did not relay the tool list")
	}
}
