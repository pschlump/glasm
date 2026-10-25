// glasm: a Lua 5.5 VM, compiler, and Lua→wasm backend in Go — a fork of
// yuin/gopher-lua (upstream copyright and license in LICENSE).
package lua

const PackageName = "glasm"
const PackageAuthors = "Yusuke Inuzuka"

// PackageVersion is overridden at link time by the Makefile (make build) via
// -ldflags "-X github.com/pschlump/glasm.PackageVersion=<version>"; the
// literal below is the fallback for plain go builds and tests.
var PackageVersion = "1.2.0"

// PackageCopyRight must stay a var: it is computed from PackageVersion at
// package init, after the linker has applied any -X override.
var PackageCopyRight = PackageName + " " + PackageVersion + " (fork of GopherLua) Copyright (C) 2015 -2017 " + PackageAuthors
