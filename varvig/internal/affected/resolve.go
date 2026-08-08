package affected

import (
	"path"
	"strings"
)

// candidateExts are appended when a path specifier omits an extension. The
// empty string is first so an exact path match wins.
var candidateExts = []string{
	"", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs",
	".py", ".rb", ".h", ".hpp", ".hxx", ".c", ".cc", ".cpp", ".cxx",
}

// resolveSpecifier maps a specifier referenced by importer to the repo paths it
// depends on. A path specifier resolves to at most one file; a package
// specifier resolves to every file in the imported package directory. External
// or unresolvable specifiers yield nothing (no false edges).
func resolveSpecifier(importer string, spec Specifier, ctx RepoContext) []string {
	switch spec.Kind {
	case SpecPackage:
		return resolvePackage(spec.Value, ctx)
	default:
		if p, ok := resolvePath(importer, spec.Value, ctx.Paths); ok {
			return []string{p}
		}
		return nil
	}
}

func resolvePath(importer, spec string, pathset map[string]bool) (string, bool) {
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

// resolvePackage maps a Go-style import path to the repo files of that package.
// Only imports under the repo's own module path resolve; external packages
// (stdlib, third-party) yield no edges. A Go package is the set of .go files
// directly in its directory (non-recursive).
func resolvePackage(importPath string, ctx RepoContext) []string {
	if ctx.GoModule == "" {
		return nil
	}
	var dir string
	switch {
	case importPath == ctx.GoModule:
		dir = "" // repo-root package
	case strings.HasPrefix(importPath, ctx.GoModule+"/"):
		dir = strings.TrimPrefix(importPath, ctx.GoModule+"/")
	default:
		return nil // external
	}
	var out []string
	for p := range ctx.Paths {
		if !strings.HasSuffix(p, ".go") {
			continue
		}
		if pkgDir(p) == dir {
			out = append(out, p)
		}
	}
	return out
}

// pkgDir returns the directory of a repo path ("" for a root-level file).
func pkgDir(p string) string {
	i := strings.LastIndexByte(p, '/')
	if i < 0 {
		return ""
	}
	return p[:i]
}
