// Package version exposes build metadata, populated at link time via -ldflags.
package version

import (
	"fmt"
	"runtime"
)

// These values are overridden at build time with:
//
//	-ldflags "-X github.com/chimerakang/macvz/internal/version.Version=..."
var (
	// Version is the semantic version or git describe of the build.
	Version = "dev"
	// Commit is the git commit SHA the binary was built from.
	Commit = "none"
	// Date is the build timestamp (RFC3339).
	Date = "unknown"
)

// String returns a human-readable one-line version summary.
func String() string {
	return fmt.Sprintf("macvz-kubelet %s (commit %s, built %s, %s/%s, %s)",
		Version, Commit, Date, runtime.GOOS, runtime.GOARCH, runtime.Version())
}
