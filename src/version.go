// pg-guard -- version.go -- build-time version metadata, set via -ldflags
// (see build.sh's -X main.version=.../-X main.commit=.../-X main.buildDate=...).
// Explicit -ldflags rather than Go's own automatic VCS stamping
// (runtime/debug.ReadBuildInfo) since build.sh already builds with
// -buildvcs=false. Defaults below are what a plain "go build ." (no
// ldflags -- e.g. running straight from source during development)
// produces.

package main

var (
	version   = "dev"
	commit    = "unknown"
	buildDate = "unknown"
)
