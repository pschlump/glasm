package testdiff

// M5d gate: the byte-exact error suite — interp vs the wasm backend
// over _wasm-err-tests, message heads compared exactly by DiffLogs
// (traceback tails already stripped there). The suite is the spike:
// every failure is either a dialect patch (runtime message families)
// or a ledger row. U6f: the exit-gate engine set (wasm, wazero,
// clua55, wazero-prod columns — plan §5).

import (
	"testing"
)

// ledgeredErrSkips lives in skips.go (shared with cmd/testdiff).

func TestWasmErrorSuite(t *testing.T) {
	dir := "../_wasm-err-tests"
	// The clua55 column is TestU2fErrorOracle (this battery, same
	// skips) — swept inline here it crossed the wasmtime Store budget
	// (the M6e SIGTRAP cliff; see exitGateEngines in m5gate_test.go).
	exitGateEngines(t, dir, CorpusSkips(dir), oracleSkipsFor(dir), false)
}
