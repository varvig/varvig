package mcp

import (
	"encoding/base64"
	"encoding/json"
)

// Context discipline (MCP spec §6). The most common way an MCP server becomes
// useless is flooding the context window, so every response is capped and every
// truncation is explicit — a marker plus a cursor, never silent.

// responseCap is the soft byte budget for a single tool response's payload
// (~50KB, §6). Tools that can exceed it paginate with an opaque cursor or, for
// read_file, fall back to a head-plus-cursor slice.
const responseCap = 50 * 1024

// Page sizes bound the number of entries a single response returns before a
// cursor is needed. They keep responses well under responseCap for typical
// entry sizes; the byte cap is the backstop for pathological entries.
const (
	treePageSize   = 200 // list_tree entries per page
	logPageSize    = 100 // read_log entries per page
	findPageSize   = 200 // find_files matches per page
	searchFileCap  = 20  // search_text matches per file, so one file cannot eat the budget
	searchCtxLines = 2   // lines of context on each side of a search match
	readFileHead   = 400 // read_file: lines returned when a file exceeds the cap and no range is given
)

// cursor is the opaque continuation token shared by the paginated tools. It is
// an offset into a deterministic, content-addressed ordering (the base is
// immutable), so the same cursor always resumes at the same place — the
// property the cursor-stability test pins.
type cursor struct {
	// Offset is the number of entries already returned by prior pages.
	Offset int `json:"o"`
	// Line is the next 1-based line for read_file continuation (0 when unused).
	Line int `json:"l,omitempty"`
}

// encodeCursor renders a cursor as an opaque base64 token. Callers treat it as
// a blob: it round-trips through decodeCursor and carries no meaning to the
// client (§6, "opaque cursors").
func encodeCursor(c cursor) string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

// decodeCursor parses an opaque token back into a cursor. An empty token is the
// start (offset 0); a malformed token is reported so a client cannot silently
// restart from the beginning after corrupting it.
func decodeCursor(tok string) (cursor, error) {
	if tok == "" {
		return cursor{}, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(tok)
	if err != nil {
		return cursor{}, gerr(codeNotFound, "invalid cursor %q", tok)
	}
	var c cursor
	if err := json.Unmarshal(b, &c); err != nil {
		return cursor{}, gerr(codeNotFound, "invalid cursor %q", tok)
	}
	if c.Offset < 0 || c.Line < 0 {
		return cursor{}, gerr(codeNotFound, "invalid cursor %q", tok)
	}
	return c, nil
}

// paginate slices items[offset:] to at most pageSize entries and reports the
// next cursor when more remain. The returned truncated flag lets a tool add the
// explicit marker the spec requires (§6). Ordering must be deterministic at the
// call site so the cursor is stable across calls.
func paginate[T any](items []T, offset, pageSize int) (page []T, next string, truncated bool) {
	if offset > len(items) {
		offset = len(items)
	}
	end := offset + pageSize
	if end >= len(items) {
		return items[offset:], "", false
	}
	return items[offset:end], encodeCursor(cursor{Offset: end}), true
}
