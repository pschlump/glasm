package testdiff

// M6b perf sanity (plan §5 M6b): numeric kernels timed on the two wasm
// hosts — wasmtime (the dev/CI oracle) vs wazero (the production engine,
// ledger row 32). darwin/arm64 runs wazero's interpreter engine (the
// conservative case; production targets linux/amd64+arm64 where the
// compiler engine engages) — this gate records the darwin numbers, M8
// owns the headline trend.
//
// Assertions, in order of importance:
//  1. engine parity under load: the same blob on both hosts produces
//     byte-identical event logs (this is the full-corpus gates' contract
//     exercised on a single long-running kernel);
//  2. the interp oracle agrees over the same source;
//  3. the kernels' published reference values appear in the log —
//     nbody's initial energy is exactly -0.169075164 (benchmarks game)
//     and fannkuch(9) prints "1911505 30" (checksum; max flips 30 is
//     OEIS A000375) — a self-check on the constants and the float
//     formatting path;
//  4. a pathology trip-wire: wazero slower than 75x wasmtime means a
//     hosting bug, not slow code (measured 2026-09-16, darwin/arm64 M4
//     Max: ~14x nbody, ~23x fannkuch — note/m6b-implemented.md).

import (
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// fannkuch kernel: integer/table traffic — a lexicographic permutation
// walk where each permutation costs an O(n) copy plus prefix reversals.
const m6bFannkuchSrc = `
local function fannkuch(n)
  local perm, q = {}, {}
  for i = 1, n do perm[i] = i end
  local maxFlips, checksum = 0, 0
  while true do
    for i = 1, n do q[i] = perm[i] end
    local flips = 0
    while q[1] ~= 1 do
      local i, j = 1, q[1]
      while i < j do q[i], q[j] = q[j], q[i]; i, j = i + 1, j - 1 end
      flips = flips + 1
    end
    if flips > maxFlips then maxFlips = flips end
    checksum = checksum + flips
    local i = n - 1
    while i > 0 and perm[i] >= perm[i + 1] do i = i - 1 end
    if i == 0 then break end
    local j = n
    while perm[j] <= perm[i] do j = j - 1 end
    perm[i], perm[j] = perm[j], perm[i]
    local lo, hi = i + 1, n
    while lo < hi do perm[lo], perm[hi] = perm[hi], perm[lo]; lo, hi = lo + 1, hi - 1 end
  end
  print(string.format("%d %d", checksum, maxFlips))
end
fannkuch(tonumber(arg and arg[1]) or 9)
`

// nbody kernel: float math — the benchmarks-game 5-body system (sun +
// jupiter/saturn/uranus/neptune, AU and years, G = 4*pi^2), printing the
// initial and final energies. The initial energy is the published
// -0.169075164; the final value oscillates around it by ~1e-5 (N=100000
// lands at -0.169075023) — cross-engine identity is the assertion, not
// the final digit.
const m6bNbodySrc = `
local sqrt = math.sqrt
local PI = 3.141592653589793
local SOLAR = 4 * PI * PI
local DAYS = 365.24
local x = { 0, 4.84143144246472090e+00, 8.34336671824457987e+00, 1.28943695621391310e+01, 1.53796971148509165e+01 }
local y = { 0, -1.16032004402742839e+00, 4.12479856412430479e+00, -1.51111514016986312e+01, -2.59193146099879641e+01 }
local z = { 0, -1.03622044471123109e-01, -4.03523417114321381e-01, -2.23307578892655734e-01, 1.79258772950371181e-01 }
local vx = { 0, 1.66007664274403694e-03 * DAYS, -2.76742510726862411e-03 * DAYS, 2.96460137564761618e-03 * DAYS, 2.68067772490389322e-03 * DAYS }
local vy = { 0, 7.69901118419740425e-03 * DAYS, 4.99852801234917238e-03 * DAYS, 2.37847173959480950e-03 * DAYS, 1.62824170038242295e-03 * DAYS }
local vz = { 0, -6.90460016972063023e-05 * DAYS, 2.30417297573763929e-05 * DAYS, -2.96589568540237556e-05 * DAYS, -9.51592254519715870e-05 * DAYS }
local m = { SOLAR, 9.54791938424326609e-04 * SOLAR, 2.85885980666130812e-04 * SOLAR, 4.36624404335156298e-05 * SOLAR, 5.15138902046611451e-05 * SOLAR }
local px, py, pz = 0, 0, 0
for i = 1, 5 do
  px, py, pz = px + vx[i] * m[i], py + vy[i] * m[i], pz + vz[i] * m[i]
end
vx[1], vy[1], vz[1] = -px / SOLAR, -py / SOLAR, -pz / SOLAR
local function advance(dt)
  for i = 1, 5 do
    for j = i + 1, 5 do
      local dx, dy, dz = x[i] - x[j], y[i] - y[j], z[i] - z[j]
      local dist = sqrt(dx * dx + dy * dy + dz * dz)
      local mag = dt / (dist * dist * dist)
      local mi, mj = m[i], m[j]
      vx[i] = vx[i] - dx * mj * mag
      vy[i] = vy[i] - dy * mj * mag
      vz[i] = vz[i] - dz * mj * mag
      vx[j] = vx[j] + dx * mi * mag
      vy[j] = vy[j] + dy * mi * mag
      vz[j] = vz[j] + dz * mi * mag
    end
  end
  for i = 1, 5 do
    x[i] = x[i] + dt * vx[i]
    y[i] = y[i] + dt * vy[i]
    z[i] = z[i] + dt * vz[i]
  end
end
local function energy()
  local e = 0
  for i = 1, 5 do
    e = e + 0.5 * m[i] * (vx[i] * vx[i] + vy[i] * vy[i] + vz[i] * vz[i])
    local ix, iy, iz = x[i], y[i], z[i]
    for j = i + 1, 5 do
      local dx, dy, dz = ix - x[j], iy - y[j], iz - z[j]
      e = e - (m[i] * m[j]) / sqrt(dx * dx + dy * dy + dz * dz)
    end
  end
  return e
end
print(string.format("%.9f", energy()))
local n = tonumber(arg and arg[1]) or 100000
for _ = 1, n do advance(0.01) end
print(string.format("%.9f", energy()))
`

func TestM6bPerfSanity(t *testing.T) {
	if testing.Short() {
		t.Skip("perf measurement — skipped in -short mode")
	}
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for _, k := range []struct {
		name, src, want string
		args            []string
	}{
		{"fannkuch9", m6bFannkuchSrc, "1911505 30", []string{"9"}},
		{"nbody100k", m6bNbodySrc, "-0.169075164", []string{"100000"}},
	} {
		t.Run(k.name, func(t *testing.T) {
			bin, err := CompileSource([]byte(k.src), "="+k.name+".lua")
			if err != nil {
				t.Fatalf("compile: %v", err)
			}
			c := Case{Name: k.name + ".lua", Dir: dir, Source: []byte(k.src), Args: k.args}
			run := func(e Engine) ([]string, time.Duration) {
				start := time.Now()
				log := e.Run(c)
				return log, time.Since(start)
			}
			itLog, itDur := run(NewInterp("interp"))
			wtLog, wtDur := run(&WasmEngine{name: "wasmtime", Precompiled: bin, SkipUnsupported: true})
			wzLog, wzDur := run(&WazeroEngine{name: "wazero", Precompiled: bin, SkipUnsupported: true})

			if d := DiffLogs(wtLog, wzLog); d != "" {
				t.Errorf("wasmtime vs wazero:\n%s", d)
			}
			if d := DiffLogs(itLog, wtLog); d != "" {
				t.Errorf("interp vs wasmtime:\n%s", d)
			}
			for _, lg := range [][]string{itLog, wtLog, wzLog} {
				for _, l := range lg {
					if strings.HasPrefix(l, "ENGINE-") {
						t.Errorf("engine failure: %s", l)
					}
				}
			}
			found := false
			for _, l := range wtLog {
				if strings.Contains(l, k.want) {
					found = true
				}
			}
			if !found {
				t.Errorf("reference value %q missing from log: %v", k.want, wtLog)
			}

			ratio := float64(wzDur) / float64(wtDur)
			t.Logf("%s: interp %v | wasmtime %v | wazero %v | wazero/wasmtime %.1fx (%s/%s)",
				k.name, itDur.Round(time.Millisecond), wtDur.Round(time.Millisecond),
				wzDur.Round(time.Millisecond), ratio, runtime.GOOS, runtime.GOARCH)
			if ratio > 75 {
				t.Errorf("wazero is %.1fx slower than wasmtime — pathological (measured ~14-23x on darwin/arm64, note/m6b-implemented.md); a number like this is a hosting bug, not slow code", ratio)
			}
		})
	}
}
