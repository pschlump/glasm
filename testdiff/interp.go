package testdiff

// The interpreter engine: the primary differential oracle (design doc §7).
// Each run gets a fresh LState, the deterministic shim, and a working
// directory equal to the corpus directory.

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	lua "github.com/pschlump/golua55"
)

// Interp is the golua55 interpreter oracle. Instances are named so two
// of them can be diffed against each other (the M1 self-diff gate).
type Interp struct {
	name string
	// Seed: host RNG seed for the run (0 → 42, the harness contract —
	// M6c D5 determinism plumbing, mirrors the wasm engines' Seed).
	Seed int64
	// Deadline: host timeout for the run (M6d D4 parity leg) — SetContext;
	// mainLoopWithContext raises ctx.Err() ("context deadline exceeded")
	// at the next instruction boundary. This is the oracle whose wording
	// the wasm engines' rt_deadline mirrors (ledger row 37).
	Deadline time.Duration
}

// NewInterp returns an interpreter engine with the given display name.
func NewInterp(name string) *Interp { return &Interp{name: name} }

func (e *Interp) Name() string { return e.name }

func (e *Interp) Run(c Case) []string {
	var log []string
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	defer os.Chdir(wd)

	L := lua.NewState(lua.Options{
		RegistrySize:        1024 * 20,
		CallStackSize:       1024,
		IncludeGoStackTrace: true,
	})
	defer L.Close()
	if e.Deadline > 0 {
		ctx, cancel := context.WithTimeout(context.Background(), e.Deadline)
		defer cancel()
		L.SetContext(ctx)
	}
	installShim(L, emit, e.Seed)
	// The C driver exposes arg = { [0] = chunkname } (luawasm.c); align the
	// oracle so scripts touching the GLOBAL arg see the same surface. (The
	// compat `arg` LOCAL is unaffected — NeedsArg is cleared when `...` is
	// used, so such functions see this global.)
	argt := L.NewTable()
	argt.RawSetInt(0, lua.LString(c.Name))
	for i, a := range c.Args { // the CLI-arg surface glua -W feeds the wasm engine
		argt.RawSetInt(i+1, lua.LString(a))
	}
	L.SetGlobal("arg", argt)
	defer func() {
		matches, _ := filepath.Glob(filepath.Join(c.Dir, "testdiff.tmp.*"))
		for _, m := range matches {
			os.Remove(m)
		}
	}()

	fn, err := L.Load(strings.NewReader(string(c.Source)), "@"+c.Name)
	if err != nil {
		emit("ERROR", ValueRepr(lua.LString(err.Error()), 0, map[*lua.LTable]bool{}))
		emit("GLOBALS", serializeGlobals(L))
		return log
	}
	L.Push(fn)
	if err := L.PCall(0, lua.MultRet, nil); err != nil {
		// The uncaught error value: PCall returns it as a Go error whose
		// message is the Lua error's string form (with position prefix).
		emit("ERROR", ValueRepr(lua.LString(err.Error()), 0, map[*lua.LTable]bool{}))
	}
	emit("GLOBALS", serializeGlobals(L))
	return log
}

func serializeGlobals(L *lua.LState) string {
	return serializeTable(L.GetGlobal("_G").(*lua.LTable), 0, map[*lua.LTable]bool{})
}

// installShim replaces every nondeterministic surface with a deterministic
// equivalent and hooks print into the event log. The shim set is part of
// the engine contract: every engine (interp today; C-Lua-wasm at M2 and
// the backend at M4) must provide the same replacements.
func installShim(L *lua.LState, emit func(event, payload string), seed int64) {
	// print → event log
	printFn := func(L *lua.LState) int {
		top := L.GetTop()
		parts := make([]string, 0, top)
		for i := 1; i <= top; i++ {
			parts = append(parts, PrintArg(L.Get(i)))
		}
		emit("PRINT", strings.Join(parts, "\t"))
		return 0
	}
	L.SetGlobal("print", L.NewFunction(printFn))

	// os.getenv/setenv: pure map, seeded the same everywhere
	env := map[string]string{"PATH": "/bin:/usr/bin"}
	getenv := func(L *lua.LState) int {
		v, ok := env[L.CheckString(1)]
		if !ok {
			L.Push(lua.LNil)
		} else {
			L.Push(lua.LString(v))
		}
		return 1
	}
	setenv := func(L *lua.LState) int {
		env[L.CheckString(1)] = L.CheckString(2)
		L.Push(lua.LTrue)
		return 1
	}
	L.SetField(L.GetGlobal("os"), "getenv", L.NewFunction(getenv))
	L.SetField(L.GetGlobal("os"), "setenv", L.NewFunction(setenv))

	// os.execute: constant exit status (satisfies os.lua's asserts without
	// shelling out — real semantics belong to the C oracle, not the diff)
	execStub := func(L *lua.LState) int {
		L.Push(lua.LNumber(1))
		return 1
	}
	L.SetField(L.GetGlobal("os"), "execute", L.NewFunction(execStub))

	// os.time/clock/date: the pinned-instant semantics the C engines
	// implement (luawasm.c, ledger row 5): no-arg time = the pinned epoch,
	// table form does real UTC calendar arithmetic (days_from_civil),
	// date formats the fixed 2000-01-01 00:00:00 UTC instant (Saturday).
	constStub := func(v float64) func(*lua.LState) int {
		return func(L *lua.LState) int {
			L.Push(lua.LNumber(v))
			return 1
		}
	}
	osTime := func(L *lua.LState) int {
		if L.GetTop() > 0 && L.Get(1).Type() == lua.LTTable {
			t := L.Get(1).(*lua.LTable)
			geti := func(name string, def int) int {
				if v := L.GetField(t, name); v.Type() == lua.LTNumber {
					return int(v.(lua.LNumber))
				}
				return def
			}
			year := geti("year", 0)
			month := geti("month", 1)
			day := geti("day", 1)
			hour := geti("hour", 12)
			min := geti("min", 0)
			sec := geti("sec", 0)
			t.RawSetString("hour", lua.LNumber(hour))
			t.RawSetString("min", lua.LNumber(min))
			t.RawSetString("sec", lua.LNumber(sec))
			t.RawSetString("isdst", lua.LBool(false))
			days := daysFromCivil(year, month, day)
			L.Push(lua.LNumber(days*86400 + int64(hour)*3600 + int64(min)*60 + int64(sec)))
			return 1
		}
		L.Push(lua.LNumber(946684800)) // 2000-01-01 00:00:00 UTC
		return 1
	}
	L.SetField(L.GetGlobal("os"), "time", L.NewFunction(osTime))
	L.SetField(L.GetGlobal("os"), "clock", L.NewFunction(constStub(0)))
	L.SetField(L.GetGlobal("os"), "date", L.NewFunction(osDateShim))

	// math.random/randomseed: deterministic, engine-owned RNG.
	//
	// golua55's mathlib draws from Go's global math/rand, whose
	// rand.Seed is a no-op on Go >= 1.24 (GODEBUG randseednop), so
	// math.randomseed(42) does not make sequences reproducible — and the
	// unseeded stream is process-random, unlike C Lua's deterministic
	// srand(1) start. The oracle therefore replaces both functions with a
	// per-run source. Every engine must provide this same shim (design
	// doc §7); see the divergence ledger.
	if seed == 0 {
		seed = 42 // harness contract: every run starts at seed 42
	}
	rng := rand.New(rand.NewSource(seed))
	mathRandom := func(L *lua.LState) int {
		switch L.GetTop() {
		case 0:
			L.Push(lua.LNumber(rng.Float64()))
		case 1:
			L.Push(lua.LNumber(rng.Intn(L.CheckInt(1)) + 1))
		default:
			min := L.CheckInt(1)
			max := L.CheckInt(2)
			if min > max {
				L.ArgError(2, "interval is empty")
			}
			L.Push(lua.LNumber(rng.Intn(max-min+1) + min))
		}
		return 1
	}
	mathRandomseed := func(L *lua.LState) int {
		rng = rand.New(rand.NewSource(L.CheckInt64(1)))
		return 0
	}
	L.SetField(L.GetGlobal("math"), "random", L.NewFunction(mathRandom))
	L.SetField(L.GetGlobal("math"), "randomseed", L.NewFunction(mathRandomseed))

	// os.tmpname: unique per call (files.lua renames tmpname() A to
	// tmpname() B — a constant would rename onto itself), deterministic
	// per run; the engine removes the leftovers after the run.
	tmpCounter := 0
	L.SetField(L.GetGlobal("os"), "tmpname", L.NewFunction(func(L *lua.LState) int {
		tmpCounter++
		L.Push(lua.LString(fmt.Sprintf("testdiff.tmp.%d", tmpCounter)))
		return 1
	}))
}

// daysFromCivil: days since 1970-01-01 from a civil date (Howard
// Hinnant's algorithm — the same one luawasm.c uses, ledger row 5).
func daysFromCivil(y, m, d int) int64 {
	yy := int64(y)
	if m <= 2 {
		yy--
	}
	era := yy / 400
	if yy < 0 && yy%400 != 0 {
		era-- // floor division for negatives
	}
	yoe := yy - era*400
	var mp int
	if m > 2 {
		mp = m - 3
	} else {
		mp = m + 9
	}
	doy := (153*int64(mp)+2)/5 + int64(d) - 1
	doe := yoe*365 + yoe/4 - yoe/100 + doy
	return era*146097 + doe - 719468
}

// osDateShim: os.date over the pinned 2000-01-01 00:00:00 UTC instant —
// "*t" gives the fixed table; string forms walk a strftime subset with
// fixed outputs (the time argument is ignored, matching luawasm.c).
func osDateShim(L *lua.LState) int {
	f := "%c"
	if L.GetTop() > 0 && L.Get(1).Type() == lua.LTString {
		f = L.Get(1).String()
	}
	f = strings.TrimPrefix(f, "!") // UTC == local at the pinned instant
	if f == "*t" {
		t := L.NewTable()
		t.RawSetString("year", lua.LNumber(2000))
		t.RawSetString("month", lua.LNumber(1))
		t.RawSetString("day", lua.LNumber(1))
		t.RawSetString("hour", lua.LNumber(0))
		t.RawSetString("min", lua.LNumber(0))
		t.RawSetString("sec", lua.LNumber(0))
		t.RawSetString("wday", lua.LNumber(7))
		t.RawSetString("yday", lua.LNumber(1))
		t.RawSetString("isdst", lua.LBool(false))
		L.Push(t)
		return 1
	}
	fixed := map[byte]string{
		'Y': "2000", 'y': "00", 'm': "01", 'd': "01", 'H': "00", 'M': "00",
		'S': "00", 'j': "001", 'w': "6", 'W': "00", 'U': "00", 'p': "AM",
		'A': "Saturday", 'a': "Sat", 'B': "January", 'b': "Jan", 'h': "Jan",
		'c': "Sat Jan  1 00:00:00 2000", 'x': "01/01/00", 'X': "00:00:00",
	}
	var b strings.Builder
	for i := 0; i < len(f); i++ {
		if f[i] != '%' || i+1 >= len(f) {
			b.WriteByte(f[i])
			continue
		}
		i++
		if f[i] == '%' {
			b.WriteByte('%')
			continue
		}
		if v, ok := fixed[f[i]]; ok {
			b.WriteString(v)
		} else {
			b.WriteByte('%')
			b.WriteByte(f[i])
		}
	}
	L.Push(lua.LString(b.String()))
	return 1
}
