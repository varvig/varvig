package main

import (
	"fmt"
	"runtime"
	"runtime/debug"

	"github.com/dividebyzero/claude-experiments/varvig/internal/mcp"
)

// version is the release version, stamped at build time by tools/build.sh via
//
//	go build -ldflags "-X main.version=v0.3.1"
//
// It is the one place a build learns its own tag. Unstamped builds (a bare
// `go build`, `go install`, `go test`) report the module's VCS build info when
// the toolchain recorded it, falling back to "dev". Release automation
// (varvig-release-automation §4) always stamps it, so a shipped binary's
// `--version` is the tag the marketplace pins against.
var version = "dev"

// init threads the build version into the MCP server's advertised version, so a
// v0.3.1 binary does not report an MCP serverInfo version frozen at build time
// of the package. The version tag lives in exactly one place (main), which is
// the single-source-of-truth discipline the release design insists on (§2).
func init() {
	mcp.SetServerVersion(versionString())
}

// versionString resolves the effective version. A stamped build wins outright;
// otherwise it consults the embedded VCS info so `go install`-ed and `go run`
// builds still identify themselves rather than lying "dev".
func versionString() string {
	if version != "dev" {
		return version
	}
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return version
	}
	if v := bi.Main.Version; v != "" && v != "(devel)" {
		return v
	}
	for _, s := range bi.Settings {
		if s.Key == "vcs.revision" && s.Value != "" {
			rev := s.Value
			if len(rev) > 12 {
				rev = rev[:12]
			}
			if dirty := vcsModified(bi); dirty {
				rev += "-dirty"
			}
			return "dev+" + rev
		}
	}
	return version
}

func vcsModified(bi *debug.BuildInfo) bool {
	for _, s := range bi.Settings {
		if s.Key == "vcs.modified" {
			return s.Value == "true"
		}
	}
	return false
}

// cmdVersion prints the version and the platform triple. Release smoke tests
// (§7) assert the tag appears here, so keep the tag as the first token after the
// program name and unadorned.
func cmdVersion([]string) error {
	fmt.Printf("varvig %s %s/%s\n", versionString(), runtime.GOOS, runtime.GOARCH)
	return nil
}
