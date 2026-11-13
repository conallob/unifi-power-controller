// Package version holds build metadata. The variables are set at link time:
//
//	-X github.com/conallob/unifi-power-controller/internal/version.Version=1.2.3
//
// (see .goreleaser.yaml). Builds without those flags, such as `go run` or
// `go install`, fall back to what the Go toolchain embedded.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

var (
	// Version is the release version, e.g. "1.2.3".
	Version = "dev"
	// Commit is the git commit the binary was built from.
	Commit = "unknown"
	// Date is the build time, RFC 3339.
	Date = "unknown"
)

// String returns a one-line description suitable for -version output.
func String() string {
	v, c, d := resolve(Version, Commit, Date, buildInfo())
	return fmt.Sprintf("unifi-power-controller %s (commit %s, built %s, %s)", v, c, d, runtime.Version())
}

// Fields returns the resolved version, commit and date for structured logging.
func Fields() (version, commit, date string) {
	return resolve(Version, Commit, Date, buildInfo())
}

func buildInfo() *debug.BuildInfo {
	bi, _ := debug.ReadBuildInfo()
	return bi
}

// resolve fills unset link-time values from the toolchain's build info: the
// module version for `go install ...@vX.Y.Z`, and VCS stamps for local builds.
func resolve(version, commit, date string, bi *debug.BuildInfo) (string, string, string) {
	if bi == nil {
		return version, commit, date
	}
	if version == "dev" && bi.Main.Version != "" && bi.Main.Version != "(devel)" {
		version = bi.Main.Version
	}
	for _, s := range bi.Settings {
		switch {
		case s.Key == "vcs.revision" && commit == "unknown":
			commit = s.Value
		case s.Key == "vcs.time" && date == "unknown":
			date = s.Value
		case s.Key == "vcs.modified" && s.Value == "true" && version == "dev":
			version = "dev-dirty"
		}
	}
	return version, commit, date
}
