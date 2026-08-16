// Package coreguard holds no code — only a build-failing guard that keeps the
// portable core free of vendor-specific integrations. varvig bridges to
// external trackers through peers that hold a bridge-kind key (tickets §5.1),
// never through code baked into the binary; DESIGN.md §3.1 ("no dlopen plugin
// ABI, ever") and §3.3 ("deliberately outside the binary") draw that line. The
// two tests here make the line mechanical rather than a matter of reviewer
// vigilance: a new mention of a named tracker, or a vendor SDK in go.mod, fails
// CI immediately.
//
// What is deliberately NOT forbidden, and why:
//   - Import-path hosts like "github.com/..." — that is where Go modules live,
//     not an integration. Stripped before scanning.
//   - The ".github/" filesystem convention (e.g. copilot-instructions.md the
//     agent-rules writer targets) — a path, like ".git", not a tracker API.
//   - Ambiguous English words that happen to be product names ("linear",
//     "shortcut", "monday") — they produce false positives ("linear history"),
//     so the denylist contains only unambiguous tracker nouns.
package coreguard

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// trackerTokens are unambiguous names of external trackers/forges. None of
// these should appear anywhere in core Go source: the core knows only the
// generic concepts (kind=bridge, an opaque `system` tag, foreign ids), and any
// vendor-shaped behavior lives in an out-of-core peer.
var trackerTokens = []string{
	"jira", "gitlab", "bitbucket", "atlassian",
	"youtrack", "clickup", "asana", "trello",
}

// scanRoots are the core source trees, relative to the module root.
var scanRoots = []string{"internal", "cmd"}

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("cannot locate this test file")
	}
	// This file is <root>/internal/coreguard/coreguard_test.go.
	return filepath.Join(filepath.Dir(file), "..", "..")
}

// TestNoVendorTokensInCore fails if any core Go file names an external tracker.
// A hit is always a real violation: import-path hosts and the .github/ path
// convention are stripped first, so nothing legitimate trips it.
func TestNoVendorTokensInCore(t *testing.T) {
	root := moduleRoot(t)
	var violations []string

	for _, sub := range scanRoots {
		dir := filepath.Join(root, sub)
		err := filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if d.IsDir() || !strings.HasSuffix(path, ".go") {
				return nil
			}
			// The guard file names the tokens by necessity; skip it.
			if strings.Contains(path, "coreguard") {
				return nil
			}
			data, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			rel, _ := filepath.Rel(root, path)
			for i, line := range strings.Split(string(data), "\n") {
				for _, tok := range vendorHits(line) {
					violations = append(violations,
						rel+":"+itoa(i+1)+": mentions vendor token "+tok)
				}
			}
			return nil
		})
		if err != nil {
			t.Fatalf("walk %s: %v", sub, err)
		}
	}

	if len(violations) > 0 {
		t.Fatalf("core must stay vendor-neutral (tickets §5.1); found %d mention(s):\n%s",
			len(violations), strings.Join(violations, "\n"))
	}
}

// vendorHits returns the vendor tokens a line contains, after removing the
// allowed "github.com" host and ".github" path convention.
func vendorHits(line string) []string {
	low := strings.ToLower(line)
	low = strings.ReplaceAll(low, "github.com", "")
	low = strings.ReplaceAll(low, ".github", "")

	var hits []string
	for _, tok := range trackerTokens {
		if strings.Contains(low, tok) {
			hits = append(hits, tok)
		}
	}
	// Any remaining bare "github" is a vendor mention (not a module path or the
	// .github/ convention, both stripped above).
	if strings.Contains(low, "github") {
		hits = append(hits, "github")
	}
	return hits
}

// TestNoVendorSDKsInGoMod fails if a require line pulls in a tracker SDK. The
// host github.com is where Go modules live, so it is not itself a signal — only
// a tracker name *within* a module path is (e.g. go-github, go-jira, go-gitlab).
func TestNoVendorSDKsInGoMod(t *testing.T) {
	root := moduleRoot(t)
	data, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	// Module-path substrings that only appear in a vendor tracker SDK.
	sdkTokens := []string{"jira", "gitlab", "bitbucket", "atlassian", "go-github", "youtrack", "clickup", "asana", "trello"}

	var violations []string
	for i, line := range strings.Split(string(data), "\n") {
		low := strings.ToLower(line)
		for _, tok := range sdkTokens {
			if strings.Contains(low, tok) {
				violations = append(violations, "go.mod:"+itoa(i+1)+": vendor SDK "+tok+" — the connector is a separate peer, not a core dependency")
			}
		}
	}
	if len(violations) > 0 {
		t.Fatalf("core must not depend on vendor SDKs (tickets §5.1):\n%s", strings.Join(violations, "\n"))
	}
}

// TestVendorHitsLogic proves the detector is not vacuous: it flags real vendor
// mentions and ignores the deliberately-allowed cases, so a green
// TestNoVendorTokensInCore actually means something.
func TestVendorHitsLogic(t *testing.T) {
	flagged := []string{
		"// call the Jira REST API",
		"// integrate with GitHub Issues", // bare "github", not a module path or .github/ dir
		"// push to GitLab",
		"client := bitbucket.New()",
	}
	for _, line := range flagged {
		if len(vendorHits(line)) == 0 {
			t.Errorf("expected a vendor hit in %q, got none", line)
		}
	}
	allowed := []string{
		`"github.com/dividebyzero/claude-experiments/varvig/internal/object"`,
		`filepath.Join(dir, ".github", "copilot-instructions.md")`,
		"// a linear history has one best common ancestor",
		"// take a shortcut through the cache",
		"// nothing vendor-specific here",
	}
	for _, line := range allowed {
		if hits := vendorHits(line); len(hits) != 0 {
			t.Errorf("false positive on allowed line %q: %v", line, hits)
		}
	}
}

// itoa avoids pulling strconv in for one small use.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
