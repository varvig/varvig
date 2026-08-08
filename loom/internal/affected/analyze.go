package affected

import (
	"path"
	"regexp"
	"strings"
)

// Specifier is a dependency reference extracted from a file, tagged with how it
// should be resolved. Extraction depends only on file content (so it caches by
// blob id); resolution against the repo happens separately.
type Specifier struct {
	Kind  string // SpecPath or SpecPackage
	Value string
}

const (
	// SpecPath is a filesystem-style import ("./x", "../y", "utils/z").
	SpecPath = "path"
	// SpecPackage is a package/module import path (Go-style), resolved against
	// the repo's module path to a package directory.
	SpecPackage = "package"
)

func (s Specifier) encode() string { return s.Kind + "\t" + s.Value }

func decodeSpecifier(s string) (Specifier, bool) {
	i := strings.IndexByte(s, '\t')
	if i < 0 {
		return Specifier{}, false
	}
	return Specifier{Kind: s[:i], Value: s[i+1:]}, true
}

// builtinAnalyzer extracts specifiers from a file's content by language.
type builtinAnalyzer struct {
	match   func(path string) bool
	extract func(content []byte) []Specifier
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

	// Go: single `import "path"` and block `import ( ... )` bodies.
	reGoSingle = regexp.MustCompile(`(?m)^\s*import\s+(?:[\w.]+\s+|_\s+|\.\s+)?"([^"]+)"`)
	reGoBlock  = regexp.MustCompile(`(?s)import\s*\(([^)]*)\)`)
	reGoQuoted = regexp.MustCompile(`"([^"]+)"`)
)

func pathSpecs(vals ...string) []Specifier {
	out := make([]Specifier, 0, len(vals))
	for _, v := range vals {
		out = append(out, Specifier{Kind: SpecPath, Value: v})
	}
	return out
}

var analyzers = []builtinAnalyzer{
	{
		match: hasExt(".js", ".jsx", ".ts", ".tsx", ".mjs", ".cjs"),
		extract: func(c []byte) []Specifier {
			s := string(c)
			var out []Specifier
			for _, re := range []*regexp.Regexp{reFrom, reBare, reRequire, reDynamic} {
				for _, m := range re.FindAllStringSubmatch(s, -1) {
					out = append(out, Specifier{Kind: SpecPath, Value: m[1]})
				}
			}
			return out
		},
	},
	{
		match: hasExt(".c", ".h", ".cc", ".cpp", ".hpp", ".cxx", ".hxx"),
		extract: func(c []byte) []Specifier {
			var out []Specifier
			for _, m := range reInclude.FindAllStringSubmatch(string(c), -1) {
				out = append(out, Specifier{Kind: SpecPath, Value: m[1]})
			}
			return out
		},
	},
	{
		match: hasExt(".py"),
		extract: func(c []byte) []Specifier {
			var out []Specifier
			for _, m := range rePyFrom.FindAllStringSubmatch(string(c), -1) {
				out = append(out, Specifier{Kind: SpecPath, Value: pyRelativeToPath(m[1])})
			}
			return out
		},
	},
	{
		match:   hasExt(".go"),
		extract: func(c []byte) []Specifier { return goImports(c) },
	},
}

// goImports extracts a Go file's import paths as package specifiers.
func goImports(c []byte) []Specifier {
	s := string(c)
	seen := map[string]bool{}
	var out []Specifier
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			out = append(out, Specifier{Kind: SpecPackage, Value: p})
		}
	}
	for _, m := range reGoSingle.FindAllStringSubmatch(s, -1) {
		add(m[1])
	}
	for _, blk := range reGoBlock.FindAllStringSubmatch(s, -1) {
		for _, q := range reGoQuoted.FindAllStringSubmatch(blk[1], -1) {
			add(q[1])
		}
	}
	return out
}

// extractBuiltin returns the specifiers for a file using the first matching
// built-in analyzer, or nil if none handles its type (textual fallback).
func extractBuiltin(p string, content []byte) []Specifier {
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
	up := strings.Repeat("../", n-1)
	if up == "" {
		up = "./"
	}
	return up + rest
}
