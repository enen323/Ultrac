// Package version exposes the build identity of the binary.
//
// The three variables below are overridden at link time rather than read at
// runtime, so `go build` alone still produces a working binary:
//
//	go build -ldflags "\
//	  -X ultrac/internal/version.Version=$(git describe --tags --always) \
//	  -X ultrac/internal/version.GitCommit=$(git rev-parse --short=12 HEAD) \
//	  -X ultrac/internal/version.BuildTimeUTC=$(date -u +%Y-%m-%dT%H:%M:%SZ)"
//
// This package is part of the infrastructure layer: it imports nothing from
// ultrac and may be imported from anywhere.
package version

import (
	"fmt"
	"runtime"
	"runtime/debug"
	"strings"
)

var (
	// Version is the release identifier, e.g. "0.1.0". "dev" when unset.
	Version = "dev"
	// GitCommit is the commit the binary was built from. "unknown" when unset.
	GitCommit = "unknown"
	// BuildTimeUTC is an RFC3339 UTC timestamp. "unknown" when unset.
	BuildTimeUTC = "unknown"
)

// Info is a snapshot of build identity.
type Info struct {
	Version      string
	GitCommit    string
	BuildTimeUTC string
	GoVersion    string
	// Modified reports whether the working tree was dirty at build time. It is
	// only meaningful when the linker did not supply GitCommit, because a clean
	// release build has nothing to be dirty about.
	Modified bool
}

// Get returns the build identity, filling gaps from the embedded build info
// when the linker did not supply a value.
func Get() Info {
	info := Info{
		Version:      Version,
		GitCommit:    GitCommit,
		BuildTimeUTC: BuildTimeUTC,
		GoVersion:    runtime.Version(),
	}

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return info
	}
	for _, s := range bi.Settings {
		switch s.Key {
		case "vcs.revision":
			if info.GitCommit == "unknown" && s.Value != "" {
				if len(s.Value) > 12 {
					info.GitCommit = s.Value[:12]
				} else {
					info.GitCommit = s.Value
				}
			}
		case "vcs.modified":
			info.Modified = s.Value == "true"
		}
	}
	return info
}

// String renders a single line suitable for `ultrac --version`.
func String() string {
	i := Get()

	var b strings.Builder
	fmt.Fprintf(&b, "ultrac %s", i.Version)
	if i.GitCommit != "" && i.GitCommit != "unknown" {
		fmt.Fprintf(&b, " (%.12s)", i.GitCommit)
	}
	if i.Modified {
		b.WriteString(" dirty")
	}
	fmt.Fprintf(&b, " %s", i.GoVersion)
	if i.BuildTimeUTC != "unknown" {
		fmt.Fprintf(&b, " built %s", i.BuildTimeUTC)
	}
	return b.String()
}
