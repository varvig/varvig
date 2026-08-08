package affected

import (
	"path"
	"regexp"
	"strings"
)

// An analyzer extracts, from a file's content, the intra-repo dependency
// specifiers it references, already normalized to path-like form so resolution
// is uniform. Bare/external specifiers (e.g. "react", a stdlib import) are
// returned as-is and simply fail to resolve, producing no edge.
type analyzer struct {
	match   func(path string) bool
	extract func(content []byte) []string
}

func hasExt(exts ...string) func(string) bool {
	return func(p string) bool {
		e := strings.ToLower(path.Ext(p))
		for _, x := range exts {
			if e == x {
				return true
			}
		}
		return false
	}
}

var (
	// JS/TS: `import ... from 'x'`, `export ... from 'x'`, bare `import 'x'`,
	// `require('x')`, and dynamic `import('x')`.
	reFrom    = regexp.MustCompile(`(?:import|export)\b[^;\n]*?\bfrom\s*['"]([^'"]+)['"]`)
	reBare    = regexp.MustCompile(`\bimport\s*['"]([^'"]+)['"]`)
	reRequire = regexp.MustCompile(`\brequire\(\s*['"]([^'"]+)['"]\s*\)`)
	reDynamic = regexp.MustCompile(`\bimport\(\s*['"]([^'"]+)['"]\s*\)`)

	// C/C++: quoted includes are local; angle-bracket includes are system.
	reInclude = regexp.MustCompile(`(?m)^\s*#\s*include\s*"([^"]+)"`)

	// Python: relative imports, `from .pkg.mod import ...` / `from . import x`.
	rePyFrom = regexp.MustCompile(`(?m)^\s*from\s+(\.+[\w.]*)\s+import\b`)
)

var analyzers = []analyzer{
	{
		match: hasExt(".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"),
		extract: func(c []byte) []string {
			s := string(c)
			var out []string
			for _, re := range []*regexp.Regexp{reFrom, reBare, reRequire, reDynamic} {
				for _, m := range re.FindAllStringSubmatch(s, -1) {
					out = append(out, m[1])
				}
			}
			return out
		},
	},
	{
		match: hasExt(".c", ".h", ".cc", ".cpp", ".hpp", ".cxx", ".hxx"),
		extract: func(c []byte) []string {
			var out []string
			for _, m := range reInclude.FindAllStringSubmatch(string(c), -1) {
				out = append(out, m[1])
			}
			return out
		},
	},
	{
		match: hasExt(".py"),
		extract: func(c []byte) []string {
			var out []string
			for _, m := range rePyFrom.FindAllStringSubmatch(string(c), -1) {
				out = append(out, pyRelativeToPath(m[1]))
			}
			return out
		},
	},
}

// extractSpecifiers returns the dependency specifiers a file references, or nil
// if no analyzer handles its type (textual fallback: no out-edges).
func extractSpecifiers(p string, content []byte) []string {
	for _, a := range analyzers {
		if a.match(p) {
			return a.extract(content)
		}
	}
	return nil
}

// pyRelativeToPath converts a Python relative import head into a path-like
// specifier: ".foo" -> "./foo", "..bar.baz" -> "../bar/baz", "." -> "./".
func pyRelativeToPath(spec string) string {
	n := 0
	for n < len(spec) && spec[n] == '.' {
		n++
	}
	rest := strings.ReplaceAll(spec[n:], ".", "/")
	// One leading dot means "this package" (./); each extra dot goes up one.
	up := strings.Repeat("../", n-1)
	if up == "" {
		up = "./"
	}
	return up + rest
}
