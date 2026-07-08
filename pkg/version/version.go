// Package version holds build-time version metadata for the hpcdn binary.
package version

import (
	"fmt"
	"runtime"
)

// These are set at build time via -ldflags. Defaults identify a source build.
var (
	Version = "1.0.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

// UserAgent returns the HTTP User-Agent string used by all hpcdn components.
func UserAgent() string {
	return fmt.Sprintf("hpcdn/%s (%s; %s)", Version, runtime.GOOS, runtime.GOARCH)
}

// String returns a human-readable multi-line version description.
func String() string {
	return fmt.Sprintf("hpcdn %s\n  commit:  %s\n  built:   %s\n  runtime: %s %s/%s",
		Version, Commit, Date, runtime.Version(), runtime.GOOS, runtime.GOARCH)
}
