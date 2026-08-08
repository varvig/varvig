// Package hook runs sandboxed WebAssembly hooks and triggers (design §3.2).
// Git's portability failure was never the git binary — it was hooks written as
// bash scripts assuming Python and curl exist. Here hook logic is a wasm module
// that is itself a content-addressed object in the repo, so triggers are
// portable, sandboxed, and versioned alongside the code they guard.
//
// A hook is a WASI command: it reads an event payload on stdin, may write to
// stdout/stderr, and signals its verdict with an exit code (0 = allow, nonzero
// = veto). Any language that targets WASI can be a hook. The module runs with
// no filesystem, no network, no environment, and no host clock beyond WASI's —
// a bounded, deterministic sandbox — under a caller-supplied context and a
// memory cap. There is no dlopen or native plugin ABI (design §3.1).
package hook

import (
	"context"
	"fmt"

	"github.com/tetratelabs/wazero"
	"github.com/tetratelabs/wazero/imports/wasi_snapshot_preview1"
	"github.com/tetratelabs/wazero/sys"

	"bytes"
)

// memoryLimitPages bounds a hook's linear memory (pages are 64 KiB): 16 MiB.
const memoryLimitPages = 256

// Result is the outcome of running a hook.
type Result struct {
	ExitCode int
	Stdout   []byte
	Stderr   []byte
}

// Allowed reports whether the hook permitted the action (exit code 0).
func (r Result) Allowed() bool { return r.ExitCode == 0 }

// Run executes a wasm module as a WASI command with input on stdin, returning
// its exit code and captured output. The sandbox grants no filesystem, network,
// environment, or arguments; cancel ctx (or set a deadline) to bound run time.
func Run(ctx context.Context, wasmModule, input []byte) (Result, error) {
	cfg := wazero.NewRuntimeConfig().
		WithCloseOnContextDone(true).
		WithMemoryLimitPages(memoryLimitPages)
	rt := wazero.NewRuntimeWithConfig(ctx, cfg)
	defer rt.Close(ctx)

	if _, err := wasi_snapshot_preview1.Instantiate(ctx, rt); err != nil {
		return Result{}, fmt.Errorf("hook: wasi: %w", err)
	}

	var stdout, stderr bytes.Buffer
	// No FS/network/env: an empty ModuleConfig is a closed sandbox. WithName("")
	// avoids module-name registration so the same bytes can run repeatedly.
	mc := wazero.NewModuleConfig().
		WithName("").
		WithStdin(bytes.NewReader(input)).
		WithStdout(&stdout).
		WithStderr(&stderr)

	mod, err := rt.InstantiateWithConfig(ctx, wasmModule, mc)
	if mod != nil {
		_ = mod.Close(ctx)
	}
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes()}
	if err != nil {
		var exit *sys.ExitError
		if asExit(err, &exit) {
			code := exit.ExitCode()
			// A context deadline/cancel surfaces as an ExitError with a sentinel
			// code; report that as a run error, not a hook verdict, so the
			// sandbox's time bound is not mistaken for an allow/deny.
			if code == sys.ExitCodeContextCanceled || code == sys.ExitCodeDeadlineExceeded {
				if ctx.Err() != nil {
					return res, fmt.Errorf("hook: interrupted: %w", ctx.Err())
				}
				return res, fmt.Errorf("hook: interrupted (exit %d)", code)
			}
			res.ExitCode = int(code)
			return res, nil
		}
		return res, fmt.Errorf("hook: run: %w", err)
	}
	return res, nil
}

// asExit is errors.As specialized to avoid importing errors twice; kept small.
func asExit(err error, target **sys.ExitError) bool {
	for err != nil {
		if e, ok := err.(*sys.ExitError); ok {
			*target = e
			return true
		}
		type unwrapper interface{ Unwrap() error }
		u, ok := err.(unwrapper)
		if !ok {
			return false
		}
		err = u.Unwrap()
	}
	return false
}
