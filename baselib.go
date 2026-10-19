package lua

import (
	"bytes"
	"fmt"
	"io"
	"math/bits"
	"os"
	"runtime"
	"strings"
	"time"
)

/* basic functions {{{ */

func OpenBase(L *LState) int {
	global := L.Get(GlobalsIndex).(*LTable)
	L.SetGlobal("_G", global)
	L.SetGlobal("_VERSION", LString(LuaVersion))
	// D12 breaking release: the gopher-lua-era identity global retired for
	// the glasm one (nothing in the tree or corpora pinned the old name).
	L.SetGlobal("_GLASM_VERSION", LString(PackageName+" "+PackageVersion))
	basemod := L.RegisterModule("_G", baseFuncs)
	global.RawSetString("ipairs", L.NewClosure(baseIpairs, L.NewFunction(ipairsaux)))
	global.RawSetString("pairs", L.NewClosure(basePairs, L.NewFunction(pairsaux)))
	// U5d (row 22): stash the marker-intercept identities — the VM's call
	// dispatch compares the RESOLVED callee against these (any alias of
	// the builtin object intercepts; a replaced global is an ordinary
	// function and does not).
	L.G.builtinPcall, _ = global.RawGetString("pcall").(*LFunction)
	L.G.builtinXPCall, _ = global.RawGetString("xpcall").(*LFunction)
	L.G.builtinPairs, _ = global.RawGetString("pairs").(*LFunction)
	L.Push(basemod)
	return 1
}

var baseFuncs = map[string]LGFunction{
	"assert":         baseAssert,
	"collectgarbage": baseCollectGarbage,
	"dofile":         baseDoFile,
	"error":          baseError,
	"getmetatable":   baseGetMetatable,
	"load":           baseLoad,
	"loadfile":       baseLoadFile,
	"next":           baseNext,
	"pcall":          basePCall,
	"print":          basePrint,
	"rawequal":       baseRawEqual,
	"rawget":         baseRawGet,
	// U4e: rawlen (5.2+) — the length WITHOUT metamethods
	"rawlen":       baseRawLen,
	"rawset":       baseRawSet,
	"select":       baseSelect,
	"_printregs":   base_PrintRegs,
	"setmetatable": baseSetMetatable,
	"tonumber":     baseToNumber,
	"tostring":     baseToString,
	"type":         baseType,
	"xpcall":       baseXPCall,
	// U3h: warn (5.4) with the @on/@off control-message protocol; the
	// default handler writes "Lua warning: <msg>" lines to stderr (not
	// captured by the engines — unobservable in diffs, like the oracle).
	"warn": baseWarn,
	// loadlib
	"require": loRequire,
	// U1 (D8 hard cut): setfenv, getfenv, module, loadstring, unpack and
	// the hidden newproxy are gone — environments are _ENV (D3) and
	// unpack moved to table.unpack.
}

// baseAssert is stock luaB_assert (U2f re-pin): on failure the message
// (arg 2, ANY value, or the default) routes through Error(msg, 1) —
// string messages gain the caller's position only when it is a Lua
// frame, non-string values raise raw (assert(nil, 42) errors with 42,
// not a type complaint about the message). U5f: a failed assert with NO
// argument is luaL_checkany's "value expected" argerror (errors.lua:671
// — pcall(assert)), not "assertion failed!"; a present-but-nil arg 2
// raises nil, which the boundary renders "<no error object>" (a string).
func baseAssert(L *LState) int {
	if L.ToBool(1) {
		return L.GetTop()
	}
	L.CheckAny(1)
	if L.GetTop() >= 2 {
		L.Error(L.Get(2), 1)
	} else {
		L.Error(LString("assertion failed!"), 1)
	}
	return 0
}

// gcParam indices (stock lgc.h's LUA_GCP* order).
const (
	gcpMinormul = iota
	gcpMajorminor
	gcpMinormajor
	gcpPause
	gcpStepmul
	gcpStepsize
)

// defaultGCParams — stock lstate.c's setgcparam defaults, coded. The
// wasm32 oracle's readbacks: minormajor's 70 quantizes to 68, stepsize
// is 200 * sizeof(Table) = 5600 on wasm32 (the 64-bit build's 8000
// differs; the interp pins the oracle's).
func defaultGCParams() [6]uint8 {
	return [6]uint8{
		gcCodeParam(20),   // minormul
		gcCodeParam(50),   // majorminor
		gcCodeParam(70),   // minormajor
		gcCodeParam(250),  // pause
		gcCodeParam(200),  // stepmul
		gcCodeParam(5600), // stepsize
	}
}

// maxLMem is stock MAX_LMEM over the interp's 64-bit l_mem.
const maxLMem = int64(1)<<62 - 1

// gcCodeParam is luaO_codeparam: a percentage as one floating-point
// byte (eeeexxxx — excess-7 exponent, 5-bit normalized mantissa,
// subnormal below 0x10, 0xFF saturates).
func gcCodeParam(p uint) uint8 {
	if p >= (0x1F<<(0xF-7-1))*100 {
		return 0xFF
	}
	p = uint((uint32(p)*128 + 99) / 100)
	if p < 0x10 {
		return uint8(p)
	}
	log := uint(bits.Len32(uint32(p+1))) - 5
	return uint8(((p >> log) - 0x10) | ((log + 1) << 4))
}

// gcApplyParam is luaO_applyparam: p/100 * x with stock's overflow
// rules (multiply-then-shift or shift-then-multiply by which avoids the
// 2^62-1 saturation first).
func gcApplyParam(p uint8, x int64) int64 {
	m := int64(p & 0xF)
	e := int(p >> 4)
	if e > 0 {
		e--
		m += 0x10
	}
	e -= 7
	if e >= 0 {
		if x < (maxLMem/0x1F)>>e {
			return (x * m) << e
		}
		return maxLMem
	}
	e = -e
	if x < maxLMem/0x1F {
		return (x * m) >> e
	}
	if (x >> e) < maxLMem/0x1F {
		return (x >> e) * m
	}
	return maxLMem
}

// baseCollectGarbage ports stock luaB_collectgarbage's option surface:
// "count" reports the synthetic accounting (gcMemBytes), the mode knobs
// are accepted as no-ops with stock result shapes, and an unknown option
// is luaL_checkoption's "invalid option '%s'". The GC itself is the host
// runtime's — the deep surface (step granularity, param, real
// generational mode) is row 79 / U5c territory.
// gcMode tracks the reported collector mode ("incremental" default —
// pushmode's oldmode protocol; the fork has no real mode machinery).
var gcMode = "incremental"

func baseCollectGarbage(L *LState) int {
	switch opt := L.OptString(1, "collect"); opt {
	case "count":
		L.Push(LNumber(float64(gcMemBytes) / 1024))
		return 1
	case "collect":
		runtime.GC()
		// U5f: the full cycle runs pending finalizers synchronously in
		// stock (luaC_fullgc's GCTM loops). Go's SetFinalizer callbacks
		// only enqueue, on another goroutine — give them a bounded
		// moment, then run the metamethods (drainFinalizers loops until
		// the queue empties, including objects a finalizer itself kills)
		for i := 0; i < 100 && !L.G.finPending.Load(); i++ {
			time.Sleep(100 * time.Microsecond)
		}
		L.drainFinalizers()
		// U5f (row 79): the full-collection arm also clears the weak
		// tables via the reachability walk (gc.go) — stock does this in
		// the atomic step of the cycle this call forces
		L.clearWeakTables()
		L.Push(LInteger(0))
	case "stop", "restart":
		L.Push(LInteger(0))
	case "step":
		L.Push(LFalse) // no cycle was in progress to finish
	case "isrunning":
		L.Push(LTrue)
	case "generational", "incremental":
		// pushmode: report the previous mode and switch the tracking
		// (gc.lua:15-17's oldmode battery) — the collection itself is
		// the host runtime's
		L.Push(LString(gcMode))
		gcMode = opt
	case "param":
		// U5c (stock LUA_GCPARAM, upgrade plan D10): the six tunables'
		// protocol — answer the OLD value as a percent integer, set a
		// non-negative argument. Storage is protocol-exact but a no-op
		// (no incremental GC to drive); luaL_checkoption's texts verbatim.
		name := L.CheckString(2)
		var idx int
		switch name {
		case "minormul":
			idx = gcpMinormul
		case "majorminor":
			idx = gcpMajorminor
		case "minormajor":
			idx = gcpMinormajor
		case "pause":
			idx = gcpPause
		case "stepmul":
			idx = gcpStepmul
		case "stepsize":
			idx = gcpStepsize
		default:
			L.ArgError(2, "invalid option '"+name+"'")
		}
		// luaL_optinteger(L,3,-1), then lua_gc's (int) cast: the set
		// value truncates to 32 bits BEFORE the >=0 guard and the coding
		// — collectgarbage("param","pause",2^53) stores codeparam(0)=0
		// (not the 0xFF saturation), and 2^31 wraps negative and reads
		// get-only (probed; gcparam01.lua)
		value := int32(-1)
		if lv := L.Get(3); lv != LNil {
			switch n := lv.(type) {
			case LInteger:
				value = int32(n)
			case LNumber:
				if iv, ok := floatToIntExact(float64(n)); ok {
					value = int32(iv)
				} else {
					L.ArgError(3, "number has no integer representation")
				}
			default:
				L.TypeError(3, LTNumber)
			}
		}
		old := gcApplyParam(L.G.gcparams[idx], 100)
		if value >= 0 {
			L.G.gcparams[idx] = gcCodeParam(uint(uint32(value)))
		}
		L.Push(LInteger(old))
	default:
		L.RaiseError("invalid option '%s'", opt)
	}
	return 1
}

func baseDoFile(L *LState) int {
	src := L.ToString(1)
	top := L.GetTop()
	fn, err := L.LoadFile(src)
	if err != nil {
		L.Push(LString(err.Error()))
		L.Panic(L)
	}
	L.Push(fn)
	L.Call(0, MultRet)
	return L.GetTop() - top
}

// baseError is stock luaB_error (U2f re-pin): no CheckAny — error()
// with no argument raises nil like stock (5.5 renders the missing
// object downstream), and only STRING messages at level > 0 gain the
// level-target frame's position (a G target adds none).
func baseError(L *LState) int {
	var obj LValue = LNil
	if L.GetTop() >= 1 {
		obj = L.Get(1)
	}
	level := L.OptInt(2, 1)
	L.Error(obj, level)
	return 0
}

func baseGetMetatable(L *LState) int {
	L.Push(L.GetMetatable(L.CheckAny(1)))
	return 1
}

// ipairsaux ports stock 5.5 ipairsaux (lbaselib.c:304): i+1 with integer
// wrap-around (nextvar.lua:119 — maxinteger steps to mininteger, the
// iterator then finds the {[mininteger]=…} entry); t[i] reads through
// lua_geti — __index-aware, not raw (nextvar.lua:925's __index battery) —
// and the nil case returns 1 value, the pushed nil itself, so the control
// value reads as nil and the loop (and a direct call's first assignee)
// sees k == nil.
func ipairsaux(L *LState) int {
	L.CheckAny(1)
	i := int64(L.CheckInt(2))
	i++
	L.Push(LInteger(i))
	v := L.GetTable(L.Get(1), LInteger(i))
	L.Push(v)
	if v == LNil {
		return 1
	}
	return 2
}

func baseIpairs(L *LState) int {
	tb := L.CheckTable(1)
	L.Push(L.Get(UpvalueIndex(1)))
	L.Push(tb)
	L.Push(LNumber(0))
	return 3
}

// loadaux is the shared tail of load/loadfile: parse+compile with the
// given environment and normalize the error shape to (nil, message).
// U1 (D3): env seeds the chunk's _ENV upvalue; nil means the thread env.
// U5f: any value is a valid env (5.5 dropped the table check —
// calls.lua:415's load(..., 123)); a Go-nil env (argument absent) seeds
// the global table into upvalue 1 in newMainFunction, an explicit nil
// seeds the nil — it must NOT fold here, so it goes to loadAux directly.
func loadaux(L *LState, reader io.Reader, chunkname string, env LValue) int {
	if fn, err := L.loadAux(reader, chunkname, env); err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	} else {
		L.Push(fn)
		return 1
	}
}

// loadauxBinary is loadaux's binary arm (U5f, dump.go): the chunk is a
// precompiled blob (first byte = the binary signature).
func loadauxBinary(L *LState, data []byte, chunkname string, env LValue) int {
	if fn, err := L.loadBinaryAux(data, chunkname, env); err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	} else {
		L.Push(fn)
		return 1
	}
}

// checkLoadMode validates the mode argument against the chunk's kind
// (binary = first byte is the LUA_SIGNATURE byte): "b" accepts only
// binary, "t" only text, "bt" (default) both — stock luaD_protected-
// parser's checkmode texts.
func checkLoadMode(L *LState, idx int, binary bool) bool {
	mode := L.OptString(idx, "bt")
	if binary && !strings.Contains(mode, "b") {
		L.Push(LNil)
		L.Push(LString(fmt.Sprintf("attempt to load a binary chunk (mode is '%s')", mode)))
		return false
	}
	if !binary && !strings.Contains(mode, "t") {
		L.Push(LNil)
		L.Push(LString(fmt.Sprintf("attempt to load a text chunk (mode is '%s')", mode)))
		return false
	}
	return true
}

// loadEnvArg returns load/loadfile's env argument (a Go-nil LValue when
// the argument is absent). Stock's lua_isnone distinguishes ABSENT
// (lua_load seeds the global table into upvalue 1) from an EXPLICIT nil
// (load_aux seeds the nil itself — calls.lua:460's `load(dump, "", "b",
// nil)` leaves upvalue 1 nil), so absence travels as a nil interface,
// not LNil (U5f; 5.5 accepts any value for env).
func loadEnvArg(L *LState, idx int) LValue {
	if L.GetTop() < idx {
		return nil
	}
	return L.Get(idx)
}

// baseLoad implements 5.2 load: load(chunk [, chunkname [, mode [, env]]]).
// chunk is a string or a reader function; a string chunk's default
// chunkname is the chunk text itself ([string "..."] rendering), a
// function chunk's is "=(load)" (stock lbaselib).
func baseLoad(L *LState) int {
	var chunk []byte
	var chunkname string
	switch lv := L.Get(1).(type) {
	case LString:
		chunk = []byte(lv)
		chunkname = L.OptString(2, string(lv))
		// stock's luaL_optstring yields a C string: the name (the chunk
		// text itself by default) truncates at its first NUL —
		// load("\0=1") reports `[string ""]` (literals.lua:127's battery).
		// The chunk BODY keeps its NULs (luaL_checklstring carries length).
		if i := strings.IndexByte(chunkname, 0); i >= 0 {
			chunkname = chunkname[:i]
		}
	case *LFunction:
		fn := lv
		chunkname = L.OptString(2, "=(load)")
		top := L.GetTop()
		buf := []byte{}
		for {
			L.SetTop(top)
			L.Push(fn)
			// U5f: stock luaD_protectedparser wraps the reader calls —
			// an erroring reader becomes load's own (nil, object) return
			// (calls.lua:412's `cannotload("hhi", ...)`), not a raise out
			// of load
			if perr := L.PCall(0, 1, nil); perr != nil {
				L.Push(LNil)
				if ae, ok := perr.(*ApiError); ok {
					L.Push(ae.Object)
				} else {
					L.Push(LString(perr.Error()))
				}
				return 2
			}
			ret := L.reg.Pop()
			if ret == LNil {
				break
			} else if LVCanConvToString(ret) {
				str := ret.String()
				if len(str) > 0 {
					buf = append(buf, str...)
				} else {
					break
				}
			} else {
				// stock generic_reader raises luaL_error inside the
				// protected parser — the error object load returns carries
				// load's CALLER position (luaL_where(1) skips load's own C
				// frame): "chunk:N: reader function must return a string"
				L.Push(LNil)
				if prefix := L.stockWhere(1); prefix != "" {
					L.Push(LString(fmt.Sprintf("%v reader function must return a string", prefix)))
				} else {
					L.Push(LString("reader function must return a string"))
				}
				return 2
			}
		}
		chunk = buf
	default:
		L.ArgError(1, "string or function expected")
		return 0
	}
	// a binary chunk starts with the LUA_SIGNATURE byte (stock
	// luaD_protectedparser's dispatch)
	binary := len(chunk) > 0 && chunk[0] == 0x1b
	if !checkLoadMode(L, 3, binary) {
		return 2
	}
	if binary {
		return loadauxBinary(L, chunk, chunkname, loadEnvArg(L, 4))
	}
	return loadaux(L, strings.NewReader(string(chunk)), chunkname, loadEnvArg(L, 4))
}

// baseLoadFile implements 5.2 loadfile([filename [, mode [, env]]]).
func baseLoadFile(L *LState) int {
	var data []byte
	var chunkname string
	if L.GetTop() < 1 || L.Get(1) == LNil {
		reader := io.Reader(os.Stdin)
		data, _ = io.ReadAll(reader)
		chunkname = "=stdin"
	} else {
		chunkname = L.CheckString(1)
		f, err := os.Open(chunkname)
		if err != nil {
			// stock 5.5 loadfile: "cannot open <file>: <strerror>"
			L.Push(LNil)
			L.Push(LString(fmt.Sprintf("cannot open %s: No such file or directory", chunkname)))
			return 2
		}
		defer f.Close()
		data, _ = io.ReadAll(f)
	}
	binary := len(data) > 0 && data[0] == 0x1b
	if !checkLoadMode(L, 2, binary) {
		return 2
	}
	if binary {
		return loadauxBinary(L, data, "@"+chunkname, loadEnvArg(L, 3))
	}
	return loadaux(L, bytes.NewReader(data), "@"+chunkname, loadEnvArg(L, 3))
}

func baseNext(L *LState) int {
	tb := L.CheckTable(1)
	index := LNil
	if L.GetTop() >= 2 {
		index = L.Get(2)
	}
	if index != LNil && !nextKeyPresent(tb, index) {
		// stock raises from the C function's frame (luaG_runerror), so
		// the message carries no position prefix
		L.raiseError(0, "invalid key to 'next'")
	}
	key, value := tb.Next(index)
	if key == LNil {
		L.Push(LNil)
		return 1
	}
	L.Push(key)
	L.Push(value)
	return 2
}

// nextKeyPresent mirrors stock 5.5 ltable.c findindex's acceptance test
// for lua_next's key: an integer key within the array part is accepted by
// range alone (even a nil slot — the scan just continues from there);
// anything else must be an existing hash-part key in its stored spelling.
// Integral floats never match (they are never a stored spelling), which
// is why next(t, 2.0) is an error even though t[2.0] reads fine.
func nextKeyPresent(tb *LTable, key LValue) bool {
	if iv, ok := key.(LInteger); ok {
		if iv >= 1 && int(iv) <= tb.arrayLen() {
			return true
		}
		return tb.RawGetH(iv) != LNil
	}
	if fv, ok := key.(LNumber); ok {
		if _, exact := floatToIntExact(float64(fv)); exact {
			return false
		}
	}
	return tb.RawGetH(key) != LNil
}

func pairsaux(L *LState) int {
	tb := L.CheckTable(1)
	key, value := tb.Next(L.Get(2))
	if key == LNil {
		return 0
	} else {
		L.Pop(1)
		L.Push(key)
		L.Push(key)
		L.Push(value)
		return 2
	}
}

// basePairs ports stock 5.5 luaB_pairs: with a __pairs metamethod it is
// called with the object and yields EXACTLY 4 results (the 4th is the
// loop's to-be-closed value — nextvar.lua:908); without one the results
// are (next, t, nil, nil) — the nil tbc slot included, so a generic-for
// always stages a 4th control slot.
func basePairs(L *LState) int {
	L.CheckAny(1)
	if mm := L.GetMetaField(L.Get(1), "__pairs"); mm != LNil {
		L.Push(mm)
		L.Push(L.Get(1))
		L.Call(1, 4)
		return 4
	}
	L.Push(L.Get(UpvalueIndex(1)))
	L.Push(L.Get(1))
	L.Push(LNil)
	L.Push(LNil)
	return 4
}

// basePCall is stock luaB_pcall (U5c): no callee pre-check — a non-callable
// first argument (through the '__call' chain, whose cap and naming the VM
// owns) surfaces as the pcall's false + error exactly as a mid-call raise
// would; a C-framed raise carries no position, so pcall(42) answers a bare
// "attempt to call a number value".
func basePCall(L *LState) int {
	L.CheckAny(1)
	nargs := L.GetTop() - 1
	if err := L.PCall(nargs, MultRet, nil); err != nil {
		L.Push(LFalse)
		if aerr, ok := err.(*ApiError); ok {
			L.Push(aerr.Object)
		} else {
			L.Push(LString(err.Error()))
		}
		return 2
	} else {
		L.Insert(LTrue, 1)
		return L.GetTop()
	}
}

func basePrint(L *LState) int {
	top := L.GetTop()
	for i := 1; i <= top; i++ {
		fmt.Print(L.ToStringMeta(L.Get(i)).String())
		if i != top {
			fmt.Print("\t")
		}
	}
	fmt.Println("")
	return 0
}

func base_PrintRegs(L *LState) int {
	L.printReg()
	return 0
}

func baseRawEqual(L *LState) int {
	if L.CheckAny(1) == L.CheckAny(2) {
		L.Push(LTrue)
	} else {
		L.Push(LFalse)
	}
	return 1
}

func baseRawGet(L *LState) int {
	L.Push(L.RawGet(L.CheckTable(1), L.CheckAny(2)))
	return 1
}

func baseRawSet(L *LState) int {
	L.RawSet(L.CheckTable(1), L.CheckAny(2), L.CheckAny(3))
	return 0
}

// baseRawLen ports luaB_rawlen (5.2+, U4e): the length WITHOUT
// metamethods — a table's raw border or a string's byte length;
// luaL_argexpected's "table or string expected" otherwise (a missing
// argument reports "no value" via typeErrorStr's argPresent check).
func baseRawLen(L *LState) int {
	switch v := L.Get(1).(type) {
	case *LTable:
		L.Push(LInteger(v.Len()))
	case LString:
		L.Push(LInteger(len(v)))
	default:
		L.typeErrorStr(1, "table or string")
	}
	return 1
}

func baseSelect(L *LState) int {
	L.CheckTypes(1, LTNumber, LTString)
	switch lv := L.Get(1).(type) {
	case LNumber:
		idx := int(lv)
		num := L.GetTop()
		if idx < 0 {
			idx = num + idx
		} else if idx > num {
			idx = num
		}
		if 1 > idx {
			L.ArgError(1, "index out of range")
		}
		return num - idx
	case LInteger:
		idx := int(lv)
		num := L.GetTop()
		if idx < 0 {
			idx = num + idx
		} else if idx > num {
			idx = num
		}
		if 1 > idx {
			L.ArgError(1, "index out of range")
		}
		return num - idx
	case LString:
		if string(lv) != "#" {
			L.ArgError(1, "invalid string '"+string(lv)+"'")
		}
		// stock pushes an INTEGER (lbaselib.c) — LNumber here printed
		// "3.0" and failed math.type (caught by the U5b var03 boundary
		// probe, row 81)
		L.Push(LInteger(L.GetTop() - 1))
		return 1
	}
	return 0
}

func baseSetMetatable(L *LState) int {
	// U5c: stock 5.5.1 luaB_setmetatable is table-only (debug.setmetatable
	// covers the other types) — 5.1's is table-only too, so the clua gate
	// is unaffected. Light userdata answers "table expected, got light
	// userdata" through CheckType's distinct-tag rule.
	L.CheckType(1, LTTable)
	L.CheckAny(2)
	if mt := L.metatable(L.Get(1), true); mt != LNil {
		if tb, ok := mt.(*LTable); ok && tb.RawGetString("__metatable") != LNil {
			L.RaiseError("cannot change a protected metatable")
		}
	}
	L.SetMetatable(L.Get(1), L.Get(2))
	L.SetTop(1)
	return 1
}

// str2intBase ports stock lbaselib.c b_str2int (tonumber with an explicit
// base): skip spaces, optional sign, then alphanumerics while they last
// (letters are valued past 'F' — 'z' is a digit 25 for base 36); a digit
// outside the base fails outright; trailing spaces; unsigned accumulate
// with wraparound. The whole string must be consumed.
func str2intBase(s string, base int) (int64, bool) {
	neg := false
	i := 0
	for ; i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0; i++ {
	}
	if i < len(s) && s[i] == '-' {
		neg = true
		i++
	} else if i < len(s) && s[i] == '+' {
		i++
	}
	if i >= len(s) || !isAlnumByte(s[i]) {
		return 0, false // no digit at all
	}
	var n uint64
	for ; i < len(s) && isAlnumByte(s[i]); i++ {
		c := s[i]
		var d int
		switch {
		case '0' <= c && c <= '9':
			d = int(c - '0')
		case 'a' <= c && c <= 'z':
			d = int(c-'a') + 10
		default: // 'A'..'Z'
			d = int(c-'A') + 10
		}
		if d >= base {
			return 0, false // invalid numeral
		}
		n = n*uint64(base) + uint64(d)
	}
	for ; i < len(s) && strings.IndexByte(" \t\n\v\f\r", s[i]) >= 0; i++ {
	}
	if i != len(s) {
		return 0, false // trailing garbage
	}
	v := int64(n)
	if neg {
		v = -v
	}
	return v, true
}

func isAlnumByte(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'z' || 'A' <= c && c <= 'Z'
}

func baseToNumber(L *LState) int {
	if L.Get(2) != LNil {
		// explicit base: strict integer conversion (stock luaB_tonumber's
		// b_str2int path — the result is always an integer)
		base := L.CheckInt(2)
		str := string(L.CheckString(1))
		if base < 2 || base > 36 {
			L.ArgError(2, "base out of range")
		}
		if v, ok := str2intBase(str, base); ok {
			L.Push(LInteger(v))
		} else {
			L.Push(LNil)
		}
		return 1
	}

	switch lv := L.CheckAny(1).(type) {
	case LNumber, LInteger:
		L.Push(lv)
	case LString:
		str := strings.Trim(string(lv), " \n\t")
		// no base: full numeral grammar via parseNumber (hex floats
		// like "0x1p-2" and trailing-dot forms like "3." are valid —
		// the old integer-only paths returned nil for them); the
		// syntactic subtype is preserved ("10" → integer, "10.0" → float)
		if v, err := parseNumber(str); err == nil {
			L.Push(v)
		} else {
			L.Push(LNil)
		}
	default:
		L.Push(LNil)
	}
	return 1
}

func baseToString(L *LState) int {
	v1 := L.CheckAny(1)
	L.Push(L.ToStringMeta(v1))
	return 1
}

func baseType(L *LState) int {
	L.Push(LString(L.CheckAny(1).Type().String()))
	return 1
}

// warnOn mirrors the default handler's state (lauxlib warnfon/warnfoff):
// warnings print to stderr until an "@off" control message arrives.
var warnOn = true

// baseWarn ports luaB_warn: all arguments must be strings (checked
// before emitting anything); every argument but the last continues the
// message; a complete message starting with '@' is a control message
// ("@on" re-enables, "@off" silences, others are swallowed).
func baseWarn(L *LState) int {
	n := L.GetTop()
	if n == 0 {
		L.CheckString(1)
	}
	msgs := make([]string, n)
	for i := 1; i <= n; i++ {
		msgs[i-1] = L.CheckString(i)
	}
	for i, m := range msgs {
		last := i == len(msgs)-1
		if last && len(m) > 0 && m[0] == '@' {
			switch m {
			case "@on":
				warnOn = true
			case "@off":
				warnOn = false
			}
			continue
		}
		if warnOn {
			if i == 0 {
				fmt.Fprintf(os.Stderr, "Lua warning: %s", m)
			} else {
				fmt.Fprintf(os.Stderr, "%s", m)
			}
			if last {
				fmt.Fprintln(os.Stderr)
			}
		}
	}
	return 0
}

// baseXPCall implements 5.2 xpcall(f, msgh [, ...]): the extra arguments
// are passed through to f (5.1 dropped them). U5c: stock checks only the
// handler (luaL_checktype(2)) — f runs through the '__call' chain like
// any call.
func baseXPCall(L *LState) int {
	L.CheckAny(1)
	errfunc := L.CheckFunction(2)
	nargs := L.GetTop() - 2

	// call a copy of fn with copies of the args (the originals, plus the
	// handler between them, stay below untouched)
	L.Push(L.Get(1))
	for i := 0; i < nargs; i++ {
		L.Push(L.Get(3 + i))
	}
	if err := L.PCall(nargs, MultRet, errfunc); err != nil {
		L.SetTop(2)
		L.Push(LFalse)
		if aerr, ok := err.(*ApiError); ok {
			L.Push(aerr.Object)
		} else {
			L.Push(LString(err.Error()))
		}
		return 2
	}
	// [fn errf a1..aN | results...] -> [true | results...]
	nres := L.GetTop() - (2 + nargs)
	L.Insert(LTrue, 2+nargs+1)
	for i := 0; i < nres+1; i++ {
		L.Replace(1+i, L.Get(2+nargs+1+i))
	}
	L.SetTop(nres + 1)
	return nres + 1
}

/* }}} */

/* load lib {{{ */

var loopdetection = &LUserData{}

func loRequire(L *LState) int {
	name := L.CheckString(1)
	loaded := L.GetField(L.Get(RegistryIndex), "_LOADED")
	lv := L.GetField(loaded, name)
	if LVAsBool(lv) {
		if lv == loopdetection {
			L.RaiseError("loop or previous error loading module: %s", name)
		}
		L.Push(lv)
		return 1
	}
	loaders, ok := L.GetField(L.Get(RegistryIndex), "_LOADERS").(*LTable)
	if !ok {
		L.RaiseError("'package.searchers' must be a table")
	}
	// U4g re-pin (attrib.lua:67, stock findloader): each searcher is
	// called for TWO results — (loader, loader-data) or its message —
	// and the not-found error is "module 'name' not found:" followed by
	// "\n\t" + the messages joined by "\n\t" (stock builds "\n\t" +
	// msg1 + "\n\t" + msg2 ... and trims the trailing prefix).
	var modasfunc LValue
	var loaderData LValue = LNil
	messages := []string{}
	for i := 1; ; i++ {
		loader := L.RawGetInt(loaders, i)
		if loader == LNil {
			detail := ""
			if len(messages) > 0 {
				detail = "\n\t" + strings.Join(messages, "\n\t")
			}
			L.RaiseError("module '%s' not found:%s", name, detail)
		}
		L.Push(loader)
		L.Push(LString(name))
		L.Call(1, 2)
		ret := L.Get(-2)
		if fn, ok := ret.(*LFunction); ok {
			modasfunc = fn
			loaderData = L.Get(-1)
			L.Pop(2)
			break
		}
		if s, ok := ret.(LString); ok {
			messages = append(messages, string(s))
		}
		L.Pop(2)
	}
	L.SetField(loaded, name, loopdetection)
	L.Push(modasfunc)
	L.Push(LString(name))
	L.Push(loaderData)
	L.Call(2, 1)
	ret := L.reg.Pop()
	modv := L.GetField(loaded, name)
	if ret != LNil && modv == loopdetection {
		L.SetField(loaded, name, ret)
		L.Push(ret)
	} else if modv == loopdetection {
		L.SetField(loaded, name, LTrue)
		L.Push(LTrue)
	} else {
		L.Push(modv)
	}
	return 1
}

/* }}} */

/* hidden features {{{ */

/* }}} */

//
