package affected

import (
	"path"
	"strings"
)

// candidateExts are appended when a specifier omits an extension. The empty
// string is first so an exact path match wins.
var candidateExts = []string{
	"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
	".py", ".h", ".hpp", ".hxx", ".c", ".cc", ".cpp", ".cxx",
}

// resolve maps a specifier referenced by importer to a repo path present in
// pathset, or reports that it does not resolve (an external/unknown dependency,
// which yields no edge). Resolution is purely path-based:
//
//   - "./x" or "../x": relative to the importer's directory
//   - "/x": rooted at the repo root
//   - "x/y": tried as a repo-root-relative path (monorepo-style absolute import)
//
// For each form it tries the path as-is, with each candidate extension, and as
// a directory index file.
func resolve(importer, spec string, pathset map[string]bool) (string, bool) {
	var base string
	switch {
	case strings.HasPrefix(spec, "./") || strings.HasPrefix(spec, "../"):
		base = path.Join(path.Dir(importer), spec)
	case strings.HasPrefix(spec, "/"):
		base = strings.TrimPrefix(spec, "/")
	default:
		base = spec // bare: try repo-root relative
	}
	base = path.Clean(base)

	for _, ext := range candidateExts {
		if cand := base + ext; pathset[cand] {
			return cand, true
		}
	}
	for _, ext := range candidateExts[1:] {
		if cand := path.Clean(base + "/index" + ext); pathset[cand] {
			return cand, true
		}
	}
	return "", false
}
