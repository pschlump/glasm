// Package host is the public Go API a pure-Go daemon (the Ultima Redis
// clone) imports to run untrusted Lua scripts: compile to wasm, execute
// on wazero against the embedded production runtime blob, exchange values
// with in-script host functions (the redis.call seam). It is pure Go —
// no cgo, no C sources, no C toolchain (design doc A8; the C under
// runtime/ is this repo's build-time concern only).
//
// The run protocol here is the gate-green embedding from
// testdiff/wazeroengine.go (M6a–M6e), productized: options instead of
// struct fields, a compile cache, typed results, and the A9 per-VM
// memory-image lock.
package host

import (
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"

	"github.com/pschlump/glasm/wasm"
)

// The production runtime blob: lua55_prod.wasm, the zero-patch Lua 5.5.1
// tree's prod flavor (U6d, ledger row 95) — base/coroutine/string/table/
// math/utf8 libraries with loaders nil'd, imports exactly the three
// host.* functions, zero WASI imports (no fs/env/clock ever enters the
// guest). ABI v4: variant-tag TValue cells (int64 lossless) and the
// hostfn seam (rt_hostfn/rt_encode_value + host.host_call, U6e row 96).
// Rebuilt by runtime/build55.sh, which copies it here; the SHA pin below
// is the lock that forces a conscious bump on every rebuild.
//
//go:embed lua55_prod.wasm
var prodBlob []byte

const prodBlobSHA256 = "aa2ec67406eccc7fb99316db4a78fe2d92eea8090595be11dc2328873efb87bf"

// hostImports is the frozen import surface the blob expects this package
// to provide (mirrored by testdiff/m6c_test.go's artifact gate).
var hostImports = []string{
	"event",         // print/library event stream out of the guest
	"wasm_dispatch", // C adapter → compiled-proto dispatch (same goroutine)
	"host_call",     // Lua→Go host functions (redis.call; U6e row 96)
}

// verifyBlob enforces the artifact contract on engine construction: the
// exact pinned bytes, imports confined to the host.* set above, and no
// WASI imports. A drifted or tampered blob fails loudly at NewEngine,
// never at first guest call.
func verifyBlob(bin []byte) error {
	sum := sha256.Sum256(bin)
	if hex.EncodeToString(sum[:]) != prodBlobSHA256 {
		return fmt.Errorf("host: runtime blob SHA-256 %s does not match pin %s",
			hex.EncodeToString(sum[:]), prodBlobSHA256)
	}
	imps, err := wasm.Imports(bin)
	if err != nil {
		return fmt.Errorf("host: parse runtime blob imports: %w", err)
	}
	want := map[string]bool{}
	for _, n := range hostImports {
		want[n] = true
	}
	for _, im := range imps {
		if im.Module != "host" || !want[im.Name] {
			return errors.New("host: runtime blob has an import outside the frozen host.* set: " +
				im.Module + "." + im.Name)
		}
		delete(want, im.Name)
	}
	if len(want) > 0 {
		return fmt.Errorf("host: runtime blob is missing host imports %v", want)
	}
	if wasi, err := wasm.HasWASIImports(bin); err != nil || wasi {
		return errors.New("host: runtime blob imports WASI (fs/env/clock) — not a production blob")
	}
	return nil
}
