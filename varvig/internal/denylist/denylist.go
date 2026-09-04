// Package denylist is the repo-level read deny-list (design addendum, U5). It
// ships empty and is expected to stay that way: every entry is a path present in
// the base tree that is nonetheless unreadable, which means tree construction
// would have to splice the unreadable subtree from the base and tell it apart
// from a deletion — the sparse-checkout mechanisms (Option A #2 and #4, build
// spec F5) that full checkouts exist to avoid. So a non-empty deny-list
// reintroduces a dose of sparse, and until F5 is built a proposal touching a
// denied path is refused rather than silently splicing.
//
// The deny-list is therefore a mechanism with an empty default and a loud
// failure mode, not a feature to populate: pressure to add an entry is the signal
// to solve the problem elsewhere — secret management for secrets, repo boundaries
// for tenant separation — not to grow the list. A denied read fails with a named
// refusal, never as not-found, so the missing-vs-denied ambiguity that makes
// sparse dangerous never appears.
package denylist

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// fileName is the repo-level deny-list file under the metadata dir: newline-
// separated repo-relative path prefixes; blank lines and # comments ignored.
const fileName = "denylist"

// List is a set of denied path prefixes. The zero value is a valid empty list
// that denies nothing.
type List struct {
	prefixes []string
}

// Load reads the deny-list under gitDir. A missing file is an empty list — the
// shipped default — not an error.
func Load(gitDir string) List {
	b, err := os.ReadFile(filepath.Join(gitDir, fileName))
	if err != nil {
		return List{}
	}
	var out []string
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.Trim(strings.TrimSpace(line), "/")
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	sort.Strings(out)
	return List{prefixes: out}
}

// Empty reports whether the list denies nothing — the shipped state.
func (l List) Empty() bool { return len(l.prefixes) == 0 }

// Denied reports whether a repo-relative path is on the deny-list: an exact
// match or under a denied directory prefix.
func (l List) Denied(path string) bool {
	p := strings.Trim(path, "/")
	for _, d := range l.prefixes {
		if p == d || strings.HasPrefix(p, d+"/") {
			return true
		}
	}
	return false
}

// Prefixes returns the denied prefixes, sorted. The returned slice is a copy.
func (l List) Prefixes() []string {
	return append([]string(nil), l.prefixes...)
}

// String renders the list for diagnostics.
func (l List) String() string {
	if l.Empty() {
		return "(empty)"
	}
	return strings.Join(l.prefixes, ", ")
}
