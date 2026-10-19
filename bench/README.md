# bench/ — luabench raw runs

Raw `cmd/luabench` outputs, kept as same-day baselines for the perf
work (U9+). Files are named `YYYY-MM-DD-<label>.txt`.

Protocol (binding, per the darwin-27 machine notes):

- **Never compare numbers across days.** Machine performance has
  shifted 2.2–3.3× between days under macOS 27; a delta is only
  meaningful between runs from the same day, back to back.
- Regenerate with `go run ./cmd/luabench` (optionally `-only substr`
  for one lane). The header line records date, Go version, and
  GOMAXPROCS.
- Timing rows are min/median/mean of auto-calibrated iterations;
  ignore timings from `-cpuprofile` runs.
- The `outputs:` line is an interp-vs-wasm value check per lane — a
  MISMATCH there is a correctness finding, not noise.

Companion analysis lives in `docs/U9-perf.md` (baseline tables,
rulings, and per-chunk deltas).
