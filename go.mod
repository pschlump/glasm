module github.com/pschlump/glasm

go 1.27.0

require (
	github.com/bytecodealliance/wasmtime-go/v48 v48.0.0
	github.com/ergochat/readline v0.1.3
	github.com/pschlump/pluto v0.1.8
	github.com/tetratelabs/wazero v1.12.0
)

require (
	golang.org/x/sys v0.47.0 // indirect
	golang.org/x/text v0.9.0 // indirect
)

// local sibling until pluto tags lru_disk_ts (then: drop this replace
// and bump the require)
replace github.com/pschlump/pluto => ../pluto
