// Package luawasm is the Lua→wasm backend (design doc §4, milestones
// M4–M6): FunctionProto trees from the golua55 frontend become wasm
// modules that share linear memory with the C runtime and call down
// through the frozen rt_* ABI (runtime/rt_abi.h).
//
// U2c (upgrade plan D2) moved the Go frontend and interpreter to the
// stock 5.5.1 instruction encoding; this backend still spoke the fork's
// pre-U2c 42-opcode format, so its emitters were retired with the
// cutover. U2e rewrites the backend against the new encoding (exported
// lua.OpGet*/OpCreate* decoders, ABI v4 on runtime/lua55/). Until then
// Compile fails fast; the wasm/wazero engine legs are ledgered-skipped
// (docs/Lua-Wasm-Divergence-Ledger.md row 59). The M4–M6 implementation
// this stubs out lives in git history (backend.go/emit.go/ops.go before
// the U2c commit) as the reference for the rt seam's shape.
package luawasm

import (
	"errors"

	lua "github.com/pschlump/golua55"
)

// errBackendV1 is the window error: the wasm backend awaits its U2e
// rewrite onto the 5.5 encoding (ledger row 59).
var errBackendV1 = errors.New("backend v1 retired at U2c: wasm backend returns with the U2e rewrite (ledger row 59)")

// Compile emits a wasm module for the FunctionProto tree rooted at main.
func Compile(main *lua.FunctionProto, chunkName string) ([]byte, error) {
	_ = main
	_ = chunkName
	return nil, errBackendV1
}
