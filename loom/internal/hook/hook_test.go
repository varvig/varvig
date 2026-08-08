package hook

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/dividebyzero/claude-experiments/loom/internal/repo"
)

// fixtureSrc is a single WASI hook whose behavior branches on its stdin, so one
// compile exercises allow, veto, sandbox, and stdout paths.
const fixtureSrc = `package main

import (
	"io"
	"os"
	"strings"
)

func main() {
	data, _ := io.ReadAll(os.Stdin)
	s := strings.TrimSpace(string(data))
	switch {
	case strings.Contains(s, "deny"):
		os.Stderr.WriteString("vetoed: contains deny\n")
		os.Exit(1)
	case s == "readfile":
		// The sandbox grants no filesystem, so this read must fail.
		if _, err := os.ReadFile("/etc/passwd"); err == nil {
			os.Exit(3)
		}
		os.Exit(0)
	case s == "out":
		os.Stdout.WriteString("hello from hook\n")
		os.Exit(0)
	case s == "loop":
		for {
		}
	default:
		os.Exit(0)
	}
}
`

var (
	moduleOnce  sync.Once
	moduleBytes []byte
	moduleErr   error
)

func getModule(t *testing.T) []byte {
	t.Helper()
	moduleOnce.Do(func() { moduleBytes, moduleErr = buildWASI(fixtureSrc) })
	if moduleErr != nil {
		t.Skipf("cannot build wasm fixture (toolchain unavailable?): %v", moduleErr)
	}
	return moduleBytes
}

// buildWASI compiles a standalone Go program to a WASI module.
func buildWASI(src string) ([]byte, error) {
	if _, err := exec.LookPath("go"); err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "hookfix-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(dir)
	if err := os.WriteFile(filepath.Join(dir, "go.mod"), []byte("module hookfix\n\ngo 1.24\n"), 0o644); err != nil {
		return nil, err
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"), []byte(src), 0o644); err != nil {
		return nil, err
	}
	out := filepath.Join(dir, "m.wasm")
	cmd := exec.Command("go", "build", "-o", out, ".")
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=wasip1", "GOARCH=wasm")
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, &buildError{msg: string(b), err: err}
	}
	return os.ReadFile(out)
}

type buildError struct {
	msg string
	err error
}

func (e *buildError) Error() string { return e.err.Error() + ": " + e.msg }

func TestRunAllow(t *testing.T) {
	mod := getModule(t)
	res, err := Run(context.Background(), mod, []byte("anything"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !res.Allowed() {
		t.Fatalf("exit = %d, want 0", res.ExitCode)
	}
}

func TestRunVeto(t *testing.T) {
	mod := getModule(t)
	res, err := Run(context.Background(), mod, []byte("please deny this"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if res.Allowed() {
		t.Fatal("expected veto, got allow")
	}
	if !strings.Contains(string(res.Stderr), "vetoed") {
		t.Fatalf("stderr = %q, want veto message", res.Stderr)
	}
}

func TestSandboxNoFilesystem(t *testing.T) {
	mod := getModule(t)
	res, err := Run(context.Background(), mod, []byte("readfile"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	// Exit 3 would mean the read succeeded — a sandbox escape.
	if res.ExitCode == 3 {
		t.Fatal("hook read the host filesystem: sandbox breached")
	}
	if !res.Allowed() {
		t.Fatalf("exit = %d, want 0 (read denied, handled)", res.ExitCode)
	}
}

func TestRunCapturesStdout(t *testing.T) {
	mod := getModule(t)
	res, err := Run(context.Background(), mod, []byte("out"))
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(string(res.Stdout), "hello from hook") {
		t.Fatalf("stdout = %q", res.Stdout)
	}
}

// TestRunTimeoutInterrupts proves the sandbox bounds run time: a hook that
// never exits is interrupted when its context deadline passes, rather than
// hanging the host. A watchdog fails the test instead of hanging the suite if
// interruption does not work.
func TestRunTimeoutInterrupts(t *testing.T) {
	mod := getModule(t)
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	done := make(chan error, 1)
	go func() {
		_, err := Run(ctx, mod, []byte("loop"))
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("a non-terminating hook returned no error")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not honor its context deadline (hook not interrupted)")
	}
}

func TestFireViaManifest(t *testing.T) {
	mod := getModule(t)
	r, err := repo.Init(t.TempDir())
	if err != nil {
		t.Fatalf("Init: %v", err)
	}
	if _, err := SetHook(r, EventPreCommit, mod, "tester"); err != nil {
		t.Fatalf("SetHook: %v", err)
	}
	// Bound module is content-addressed; the manifest survives a fresh open.
	cfg, err := LoadManifest(r)
	if err != nil || len(cfg.Entries) != 1 || cfg.Entries[0].Event != EventPreCommit {
		t.Fatalf("manifest = %+v err=%v", cfg, err)
	}
	results, err := Fire(context.Background(), r, EventPreCommit, []byte("deny it"))
	if err != nil {
		t.Fatalf("Fire: %v", err)
	}
	if len(results) != 1 || results[0].Allowed() {
		t.Fatalf("results = %+v, want one veto", results)
	}
}
