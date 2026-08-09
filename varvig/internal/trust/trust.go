// Package trust implements the repository's trust store: the list of principals
// allowed to act, and what each may do (auth design §3). The store is an
// ordinary versioned file — "the repo is the trust store" — so changing it is a
// change like any other and is itself subject to promotion rules (§3.2).
//
// The file is a table, one principal per line:
//
//		# fingerprint       name    scope        rights
//		SHA256:aXk9Lm4Qr…   jan     /            promote
//		SHA256:cW3nEf8Zx…   ci-01   /            propose
//		SHA256:dK1oIu5Vb…   sam     src/web/     promote
//
//	  - scope  — the path prefix the principal may affect; "/" is the whole repo.
//	  - rights — read, propose, or promote. Later rights imply the earlier ones.
//
// # Round-trip discipline (auth design §3.1, design §4.4)
//
// Comments, blank lines, unknown trailing columns, and lines this build does
// not understand are all retained verbatim and re-emitted in place. A future
// version may add fields; an old client must never silently delete them. Parse
// followed by Bytes on an unmodified file reproduces its exact bytes.
package trust

import (
	"fmt"
	"strings"
)

// DefaultPath is the store's conventional location: a tracked file under the
// .vcs/ configuration directory. It is deliberately NOT under .varvig/, which
// is this implementation's private (untracked) metadata; .vcs/ is versioned
// like any other path and travels with the repository.
const DefaultPath = ".vcs/allowed_keys"

// Right is a privilege level. Rights are ordered: a principal holding a higher
// right implicitly holds the lower ones (auth design §3.1).
type Right int

const (
	// RightUnknown is an unrecognized rights token; it grants nothing but is
	// preserved for round-trip.
	RightUnknown Right = iota
	RightRead
	RightPropose
	RightPromote
)

// ParseRight maps a rights token to a Right, reporting whether it is recognized.
func ParseRight(s string) (Right, bool) {
	switch s {
	case "read":
		return RightRead, true
	case "propose":
		return RightPropose, true
	case "promote":
		return RightPromote, true
	default:
		return RightUnknown, false
	}
}

func (r Right) String() string {
	switch r {
	case RightRead:
		return "read"
	case RightPropose:
		return "propose"
	case RightPromote:
		return "promote"
	default:
		return "unknown"
	}
}

// Allows reports whether holding r satisfies a requirement for want. The zero
// (unknown) right never satisfies anything.
func (r Right) Allows(want Right) bool {
	if r == RightUnknown || want == RightUnknown {
		return false
	}
	return r >= want
}

// Scope is a normalized path prefix. The root scope is "/"; any other scope is
// stored with a trailing slash so prefix tests do not match across a component
// boundary ("src/web/" must not cover "src/webapp/").
type Scope string

// NormalizeScope canonicalizes a raw scope token. "", "/", and "." all denote
// the whole repository. Any other value is reduced to "<path>/".
func NormalizeScope(s string) Scope {
	s = strings.TrimSpace(s)
	s = strings.Trim(s, "/")
	if s == "" || s == "." {
		return "/"
	}
	return Scope(s + "/")
}

// Covers reports whether this scope includes target (itself normalized as a
// path). The root scope covers everything.
func (sc Scope) Covers(target string) bool {
	if sc == "/" {
		return true
	}
	nt := NormalizeScope(target)
	if nt == "/" {
		return false // the whole repo is not within a narrower scope
	}
	return strings.HasPrefix(string(nt), string(sc))
}

// Record is one parsed principal line.
type Record struct {
	Fingerprint string
	Name        string
	Scope       Scope
	Right       Right
	// Extra holds any columns beyond the four known ones, preserved so a newer
	// file's added fields survive an older client (auth design §3.1).
	Extra []string
}

// File is a parsed trust store that preserves its exact byte layout across a
// parse/serialize cycle unless records are explicitly changed.
type File struct {
	lines []line
}

type line struct {
	raw   string  // original text (no newline); source of truth when !dirty
	rec   *Record // non-nil only for recognized principal lines
	dirty bool    // set when rec was added/modified and must be re-rendered
	final bool    // whether the file's last physical line had a trailing newline
}

// Parse reads a trust store. It never fails on unrecognized content: comment,
// blank, and unparseable lines are retained opaquely.
func Parse(b []byte) *File {
	f := &File{}
	if len(b) == 0 {
		return f
	}
	text := string(b)
	hasFinalNewline := strings.HasSuffix(text, "\n")
	raw := strings.Split(text, "\n")
	if hasFinalNewline {
		raw = raw[:len(raw)-1] // drop the empty element after the final newline
	}
	for _, ln := range raw {
		f.lines = append(f.lines, line{raw: ln, rec: parseRecord(ln)})
	}
	if len(f.lines) > 0 {
		f.lines[len(f.lines)-1].final = hasFinalNewline
	}
	return f
}

// parseRecord returns a Record for a recognized principal line, or nil for a
// comment, blank, or unrecognized line (which must be preserved verbatim).
func parseRecord(ln string) *Record {
	trimmed := strings.TrimSpace(ln)
	if trimmed == "" || strings.HasPrefix(trimmed, "#") {
		return nil
	}
	fields := strings.Fields(trimmed)
	if len(fields) < 4 {
		return nil // not enough columns to be a principal record
	}
	right, ok := ParseRight(fields[3])
	if !ok {
		return nil // unknown rights token: preserve, do not guess
	}
	return &Record{
		Fingerprint: fields[0],
		Name:        fields[1],
		Scope:       NormalizeScope(fields[2]),
		Right:       right,
		Extra:       append([]string(nil), fields[4:]...),
	}
}

// Bytes serializes the file. Unmodified lines are emitted verbatim; added or
// modified records are rendered canonically.
func (f *File) Bytes() []byte {
	var sb strings.Builder
	for i, ln := range f.lines {
		if ln.dirty && ln.rec != nil {
			sb.WriteString(ln.rec.render())
		} else {
			sb.WriteString(ln.raw)
		}
		// Every line but the last is newline-terminated; the last mirrors the
		// original file's trailing-newline state.
		if i < len(f.lines)-1 || ln.final {
			sb.WriteByte('\n')
		}
	}
	return []byte(sb.String())
}

func (r *Record) render() string {
	scope := string(r.Scope)
	parts := []string{r.Fingerprint, r.Name, scope, r.Right.String()}
	parts = append(parts, r.Extra...)
	return strings.Join(parts, " ")
}

// Records returns every recognized principal record, in file order.
func (f *File) Records() []Record {
	var out []Record
	for _, ln := range f.lines {
		if ln.rec != nil {
			out = append(out, *ln.rec)
		}
	}
	return out
}

// Lookup returns all records for a fingerprint (a principal may appear more than
// once, e.g. with different scopes).
func (f *File) Lookup(fingerprint string) []Record {
	var out []Record
	for _, ln := range f.lines {
		if ln.rec != nil && ln.rec.Fingerprint == fingerprint {
			out = append(out, *ln.rec)
		}
	}
	return out
}

// Authorized reports whether fingerprint holds at least want over targetScope
// through some record. This is the question ref-update verification asks in
// §5.2 step 4: does the signer hold `promote` at a scope covering the ref?
func (f *File) Authorized(fingerprint string, want Right, targetScope string) bool {
	for _, r := range f.Lookup(fingerprint) {
		if r.Right.Allows(want) && r.Scope.Covers(targetScope) {
			return true
		}
	}
	return false
}

// Add appends a principal record as a new canonical line. It does not
// deduplicate; onboarding is "append a line and push" (auth design §3.2).
func (f *File) Add(r Record) {
	if r.Scope == "" {
		r.Scope = NormalizeScope("")
	}
	// The previously-final line is no longer last, so it keeps its newline.
	if n := len(f.lines); n > 0 {
		f.lines[n-1].final = true
	}
	rec := r
	f.lines = append(f.lines, line{rec: &rec, dirty: true, final: true})
}

// Remove drops every line for a fingerprint, returning how many were removed.
// Offboarding is deleting the line (auth design §3.2).
func (f *File) Remove(fingerprint string) int {
	kept := f.lines[:0]
	removed := 0
	for _, ln := range f.lines {
		if ln.rec != nil && ln.rec.Fingerprint == fingerprint {
			removed++
			continue
		}
		kept = append(kept, ln)
	}
	f.lines = kept
	if n := len(f.lines); n > 0 {
		f.lines[n-1].final = true
	}
	return removed
}

// String renders the store for display (not for storage; use Bytes for that).
func (f *File) String() string {
	var sb strings.Builder
	for _, r := range f.Records() {
		fmt.Fprintf(&sb, "%s  %-10s  %-10s  %s\n", r.Fingerprint, r.Name, r.Scope, r.Right)
	}
	return sb.String()
}
