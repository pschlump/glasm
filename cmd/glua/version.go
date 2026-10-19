// version.go — build information injected at link time by the Makefile via
// -ldflags "-X main.<name>=...". This file is static and never regenerated.

package main

import (
	"fmt"

	"github.com/pschlump/glasm"
)

// Overwritten at link time by LDFLAGS in the Makefile; empty in a binary
// built with a plain "go build".
var (
	GitCommit string
	GitTag    string
	BuildDate string
)

// versionInfo returns the version banner: the GopherLua release line plus
// the injected build identifiers. A binary linked without LDFLAGS reports
// "no build info" instead of empty fields.
func versionInfo() string {
	if GitCommit == "" && GitTag == "" && BuildDate == "" {
		return lua.PackageCopyRight + " (no build info)"
	}
	return fmt.Sprintf("%s\ngit tag: %s\ncommit: %s\nbuilt: %s",
		lua.PackageCopyRight, GitTag, GitCommit, BuildDate)
}
