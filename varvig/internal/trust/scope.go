package trust

import (
	"sort"
	"strings"
)

// ScopeSet is a task's capability boundary as a *set* of path prefixes: a task
// may read and propose within any of them (build spec P0.5). A single-scope task
// is the common case (a one-element set); declaring several — `--scope A --scope
// B` — unions them, because a capability boundary that silently discarded one of
// two declared scopes would be exactly the last-wins bug A3 forbids.
//
// The whole-repo scope "/" subsumes everything, so a set containing it collapses
// to {"/"}. The empty set also means the whole repo, matching a task started with
// no scope.
type ScopeSet []Scope

// NewScopeSet normalizes and unions raw scope tokens (each of which may itself be
// a comma-separated list) into a canonical, deduplicated, sorted set. Any "/"
// present collapses the set to {"/"}; an empty input is {"/"} (the whole repo).
func NewScopeSet(raw ...string) ScopeSet {
	seen := map[Scope]bool{}
	var out ScopeSet
	for _, r := range raw {
		for _, part := range strings.Split(r, ",") {
			if strings.TrimSpace(part) == "" {
				continue
			}
			sc := NormalizeScope(part)
			if sc == "/" {
				return ScopeSet{"/"}
			}
			if !seen[sc] {
				seen[sc] = true
				out = append(out, sc)
			}
		}
	}
	if len(out) == 0 {
		return ScopeSet{"/"}
	}
	sort.Slice(out, func(i, j int) bool { return out[i] < out[j] })
	return out
}

// Covers reports whether any scope in the set includes target.
func (s ScopeSet) Covers(target string) bool {
	for _, sc := range s {
		if sc.Covers(target) {
			return true
		}
	}
	return false
}

// String renders the set as a stable comma-joined token that round-trips through
// NewScopeSet — used for display and to carry a multi-scope grant over the
// daemon's single-string control protocol.
func (s ScopeSet) String() string {
	if len(s) == 0 {
		return "/"
	}
	parts := make([]string, len(s))
	for i, sc := range s {
		parts[i] = string(sc)
	}
	return strings.Join(parts, ",")
}

// Primary returns the first scope's repo-relative directory ("" for the whole
// repo) — the default read root for a tool that lists or walks a single subtree.
func (s ScopeSet) Primary() string {
	if len(s) == 0 {
		return ""
	}
	return strings.Trim(string(s[0]), "/")
}
