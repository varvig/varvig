package mcp

import (
	"errors"
	"fmt"
)

// Distinct, machine-readable error codes (auth design / MCP spec §8). An
// orchestrator must be able to tell "renew the credential" from "the agent
// asked for something it may not have" — so a tool failure carries a stable
// code alongside the human message, and every message states what was attempted
// and what the current scope is. Agents recover from specific errors and loop
// on vague ones.
const (
	codeOutOfScope        = "out_of_scope"       // path or object outside the task's read set
	codeCredentialExpired = "credential_expired" // task TTL elapsed
	codeStaleBase         = "stale_base"         // proposal built on a superseded base
	codeNotFound          = "not_found"          // no such hash, ref, or path
	codeTruncated         = "truncated"          // response hit the cap; continue with the cursor
	codeUnavailable       = "unavailable"        // query layer unreachable
	codeInvalidArgs       = "invalid_args"       // malformed tool arguments
	codeInternal          = "internal"           // unexpected server fault
)

// gateError is a tool error carrying a stable, machine-readable code. It is
// surfaced in-band (isError) with the code in structuredContent, not as a
// JSON-RPC protocol fault, so the model and any orchestrator both see it.
type gateError struct {
	Code string
	Msg  string
}

func (e *gateError) Error() string { return e.Msg }

// gerr builds a coded gate error with a formatted message.
func gerr(code, format string, args ...any) *gateError {
	return &gateError{Code: code, Msg: fmt.Sprintf(format, args...)}
}

// codeOf extracts the code from an error, defaulting to codeInternal for a bare
// error that was never classified.
func codeOf(err error) string {
	var ge *gateError
	if errors.As(err, &ge) {
		return ge.Code
	}
	return codeInternal
}
