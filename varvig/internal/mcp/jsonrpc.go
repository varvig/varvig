package mcp

import (
	"bufio"
	"encoding/json"
	"io"
	"sync"
)

// MCP speaks JSON-RPC 2.0. Over the stdio transport, messages are a stream of
// JSON values (the reference implementation delimits them with newlines; a
// streaming decoder reads them back regardless of whitespace). This file is the
// minimal framing: request/response/notification types and a reader/writer pair.

// jsonrpcVersion is the only version this gate speaks.
const jsonrpcVersion = "2.0"

// JSON-RPC error codes used by the gate (the standard reserved range).
const (
	codeParseError     = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternalError  = -32603
)

// request is an incoming JSON-RPC message. A request with no ID is a
// notification and receives no response.
type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

// isNotification reports whether the message carries no id (JSON-RPC §4.1).
func (r *request) isNotification() bool { return len(r.ID) == 0 }

// response is an outgoing JSON-RPC reply. Exactly one of Result or Error is set.
type response struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

// rpcError is a JSON-RPC error object.
type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *rpcError) Error() string { return e.Message }

// conn frames JSON-RPC over an io.ReadWriter. Writes are serialized so
// concurrent handlers cannot interleave a message on the wire.
type conn struct {
	dec *json.Decoder
	w   io.Writer
	mu  sync.Mutex
}

func newConn(rw io.ReadWriter) *conn {
	return &conn{
		// A large buffered reader keeps big proposal payloads from stalling the
		// decoder; json.Decoder itself streams values back-to-back.
		dec: json.NewDecoder(bufio.NewReaderSize(rw, 1<<16)),
		w:   rw,
	}
}

// read decodes the next JSON-RPC request. It returns io.EOF when the peer closes.
func (c *conn) read() (*request, error) {
	var req request
	if err := c.dec.Decode(&req); err != nil {
		return nil, err
	}
	return &req, nil
}

// write emits one response as a single newline-delimited JSON object, which is
// what the stdio transport expects and what a line-oriented client can read.
func (c *conn) write(resp *response) error {
	resp.JSONRPC = jsonrpcVersion
	b, err := json.Marshal(resp)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.w.Write(b); err != nil {
		return err
	}
	_, err = c.w.Write([]byte{'\n'})
	return err
}

// replyResult writes a success response carrying result (already JSON).
func (c *conn) replyResult(id json.RawMessage, result any) error {
	b, err := json.Marshal(result)
	if err != nil {
		return c.replyError(id, codeInternalError, err.Error())
	}
	return c.write(&response{ID: id, Result: b})
}

// replyError writes an error response.
func (c *conn) replyError(id json.RawMessage, code int, msg string) error {
	return c.write(&response{ID: id, Error: &rpcError{Code: code, Message: msg}})
}
