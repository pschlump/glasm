module luabench

go 1.27.0

require (
	github.com/pschlump/glasm v0.0.0
	github.com/pschlump/gopher-lua-orig v0.0.0
)

require (
	github.com/bytecodealliance/wasmtime-go/v48 v48.0.0 // indirect
	github.com/tetratelabs/wazero v1.12.0 // indirect
	golang.org/x/sys v0.47.0 // indirect
)

// this module lives at <gopher-lua>/note/bench-vs-orig — the two replaces
// point at the fork itself and the pristine baseline checkout next to it.
replace github.com/pschlump/glasm => ../..

replace github.com/pschlump/gopher-lua-orig => ../../../gopher-lua-orig
