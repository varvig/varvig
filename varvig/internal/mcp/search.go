package mcp

import (
	"path"
	"regexp"
	"sort"
	"strings"

	"github.com/dividebyzero/claude-experiments/varvig/internal/affected"
	"github.com/dividebyzero/claude-experiments/varvig/internal/multihash"
)

// find_files and search_text walk only the task's scope subtree, so scope is a
// property of what is walked, not a filter applied after the fact. There is no
// existing repo-tree matcher in the codebase; these are built over
// affected.FlattenTree (path -> blob for a tree) plus path.Match / regexp.

// fileRef is one in-scope file: its repo-relative path and blob hash.
type fileRef struct {
	Path string
	Blob multihash.Multihash
}

// scopeFiles lists every file within the task's scope, as repo-relative paths
// sorted lexicographically (so pagination cursors are stable). It flattens the
// scope subtree only — never the whole repo — so out-of-scope objects are not
// even read. Uses the raw query; per-file reads are logged when a tool actually
// resolves a blob.
func (g *Gate) scopeFiles() ([]fileRef, error) {
	if g.base == nil {
		return nil, nil
	}
	scopePath := g.scopePath()
	listing, err := g.q.Tree(g.base, scopePath)
	if err != nil {
		return nil, gerr(codeNotFound, "cannot resolve scope %q: %v", g.grant.Scope, err)
	}
	subID, err := multihash.ParseHex(listing.Hash)
	if err != nil {
		return nil, gerr(codeInternal, "bad subtree hash: %v", err)
	}
	flat, err := affected.FlattenTree(g.repo.Objects, subID)
	if err != nil {
		return nil, gerr(codeInternal, "walking scope %q: %v", g.grant.Scope, err)
	}
	refs := make([]fileRef, 0, len(flat))
	for rel, blob := range flat {
		full := rel
		if scopePath != "" {
			full = scopePath + "/" + rel
		}
		refs = append(refs, fileRef{Path: full, Blob: blob})
	}
	sort.Slice(refs, func(i, j int) bool { return refs[i].Path < refs[j].Path })
	return refs, nil
}

// matchGlob reports whether repo-relative path p matches pattern. A pattern
// containing "/" is matched against the whole path; a bare pattern is matched
// against the basename, which is the common "*.go" case.
func matchGlob(pattern, p string) bool {
	if strings.Contains(pattern, "/") {
		ok, err := path.Match(pattern, p)
		return err == nil && ok
	}
	ok, err := path.Match(pattern, path.Base(p))
	return err == nil && ok
}

// textMatch is a single search hit: a 1-based line number, the matching line,
// and the surrounding context lines.
type textMatch struct {
	Line    int      `json:"line"`
	Text    string   `json:"text"`
	Context []string `json:"context"`
}

// searchBlob scans content for matches, returning at most searchFileCap hits so
// one pathological file cannot consume the whole budget (§6). matcher reports
// whether a line matches.
func searchBlob(content []byte, matcher func(string) bool) (hits []textMatch, capped bool) {
	lines := strings.Split(string(content), "\n")
	for i, ln := range lines {
		if !matcher(ln) {
			continue
		}
		if len(hits) >= searchFileCap {
			return hits, true
		}
		lo := i - searchCtxLines
		if lo < 0 {
			lo = 0
		}
		hi := i + searchCtxLines
		if hi >= len(lines) {
			hi = len(lines) - 1
		}
		ctx := make([]string, 0, hi-lo+1)
		for j := lo; j <= hi; j++ {
			ctx = append(ctx, lines[j])
		}
		hits = append(hits, textMatch{Line: i + 1, Text: ln, Context: ctx})
	}
	return hits, false
}

// buildMatcher compiles the query into a line predicate. A regex query is
// anchored nowhere (substring semantics via FindString); a literal query is a
// plain substring test.
func buildMatcher(query string, isRegex bool) (func(string) bool, error) {
	if !isRegex {
		return func(s string) bool { return strings.Contains(s, query) }, nil
	}
	re, err := regexp.Compile(query)
	if err != nil {
		return nil, gerr(codeNotFound, "invalid regex %q: %v", query, err)
	}
	return re.MatchString, nil
}
