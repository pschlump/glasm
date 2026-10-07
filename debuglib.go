package lua

import (
	"strings"

	"github.com/pschlump/golua55/parse"
)

func OpenDebug(L *LState) int {
	dbgmod := L.RegisterModule(DebugLibName, debugFuncs)
	L.Push(dbgmod)
	return 1
}

var debugFuncs = map[string]LGFunction{
	"getinfo":      debugGetInfo,
	"gethook":      debugGetHook,
	"getlocal":     debugGetLocal,
	"getmetatable": debugGetMetatable,
	"getregistry":  debugGetRegistry,
	"getupvalue":   debugGetUpvalue,
	"getuservalue": debugGetUserValue,
	"sethook":      debugSetHook,
	"setlocal":     debugSetLocal,
	"setmetatable": debugSetMetatable,
	"setupvalue":   debugSetUpvalue,
	"setuservalue": debugSetUserValue,
	"traceback":    debugTraceback,
	"upvalueid":    debugUpvalueId,
	"upvaluejoin":  debugUpvalueJoin,
}

// debugGetInfo is stock db_getinfo (U5c): the thread-first form, default
// options "flnSrtu", option-filtered fields in stock's order, nil for an
// out-of-range (or negative) level, and the two argerrors — '>' as the
// first option and any option outside "flnSrtuL" outside the set.
func debugGetInfo(L *LState) int {
	arg := 0
	L1 := L
	if th, ok := L.Get(1).(*LState); ok {
		L1 = th
		arg = 1
	}
	what := L.OptString(arg+2, "flnSrtu")
	if len(what) > 0 && what[0] == '>' {
		L.ArgError(arg+2, "invalid option '>'")
	}
	var dbg *Debug
	var fn LValue
	var err error
	if f, ok := L.Get(arg + 1).(*LFunction); ok {
		dbg = &Debug{}
		fn, err = L.GetInfo(">"+what, dbg, f)
	} else {
		level := L.CheckInt(arg + 1)
		if level < 0 {
			L.Push(LNil)
			return 1
		}
		var ok bool
		dbg, ok = L1.GetStack(level)
		if !ok {
			L.Push(LNil)
			return 1
		}
		fn, err = L.GetInfo(what, dbg, LNil)
	}
	if err != nil {
		L.ArgError(arg+2, "invalid option")
	}

	tbl := L.NewTable()
	if strings.Contains(what, "S") {
		tbl.RawSetString("source", LString(dbg.Source))
		tbl.RawSetString("short_src", LString(parse.ChunkID(dbg.Source)))
		tbl.RawSetString("linedefined", LInteger(dbg.LineDefined))
		tbl.RawSetString("lastlinedefined", LInteger(dbg.LastLineDefined))
		tbl.RawSetString("what", LString(dbg.What))
	}
	if strings.Contains(what, "l") {
		tbl.RawSetString("currentline", LInteger(dbg.CurrentLine))
	}
	if strings.Contains(what, "u") {
		tbl.RawSetString("nups", LInteger(dbg.NUpvalues))
		tbl.RawSetString("nparams", LInteger(dbg.NParams))
		tbl.RawSetString("isvararg", LBool(dbg.IsVarArg))
	}
	if strings.Contains(what, "n") {
		// stock lua_pushstring(NULL) → nil: the name key is always set,
		// to nil when there is no name
		if dbg.Name == "" {
			tbl.RawSetString("name", LNil)
		} else {
			tbl.RawSetString("name", LString(dbg.Name))
		}
		tbl.RawSetString("namewhat", LString(dbg.NameWhat))
	}
	if strings.Contains(what, "r") {
		tbl.RawSetString("ftransfer", LInteger(dbg.FTransfer))
		tbl.RawSetString("ntransfer", LInteger(dbg.NTransfer))
	}
	if strings.Contains(what, "t") {
		tbl.RawSetString("istailcall", LBool(dbg.IsTailCall))
		tbl.RawSetString("extraargs", LInteger(dbg.ExtraArgs))
	}
	if strings.Contains(what, "L") {
		// a nil *LTable would land as a typed-nil LValue (prints
		// "<table>") — C functions answer a real nil key (db01 pin)
		if dbg.ActiveLines != nil {
			tbl.RawSetString("activelines", dbg.ActiveLines)
		} else {
			tbl.RawSetString("activelines", LNil)
		}
	}
	if strings.Contains(what, "f") {
		tbl.RawSetString("func", fn)
	}
	L.Push(tbl)
	return 1
}

// debugSetHook is stock db_sethook (U5f): sethook([thread,] hook, mask
// [, count]) — mask characters c/r/l (anything else ignored), a
// positive count adds the count hook; sethook([thread]) with no hook
// clears. Stock's argument order: the mask string, then the count
// (luaL_optinteger), then the function type check.
func debugSetHook(L *LState) int {
	// arg is the hook's index (getthread's offset: 1 plain, 2 with a
	// thread first) — stock db_sethook's convention
	arg := 1
	target := L
	if th, ok := L.Get(1).(*LState); ok {
		target = th
		arg = 2
	}
	if L.GetTop() < arg || L.Get(arg) == LNil {
		// no hook: turn off (stock lua_sethook(L1, NULL, 0, 0)) — stock
		// still clears the hook-table entry (hooktable[L1] = nil)
		target.hookFn = nil
		target.hookMask = 0
		target.hookCount = 0
		target.hookCountdown = 0
		getHookTable(L).RawSet(target, LNil)
		return 0
	}
	smask := L.CheckString(arg + 1)
	var mask uint8
	if strings.Contains(smask, "c") {
		mask |= hookMaskCall
	}
	if strings.Contains(smask, "r") {
		mask |= hookMaskRet
	}
	if strings.Contains(smask, "l") {
		mask |= hookMaskLine
	}
	count := 0
	if c := L.Get(arg + 2); c != LNil {
		switch n := c.(type) {
		case LInteger:
			count = int(n)
		case LNumber:
			if iv, ok := floatToIntExact(float64(n)); ok {
				count = int(iv)
			} else {
				L.ArgError(arg+2, "number has no integer representation")
			}
		default:
			L.TypeError(arg+2, LTNumber)
		}
	}
	if count > 0 {
		mask |= hookMaskCount
	}
	fn := L.CheckFunction(arg)
	target.hookFn = fn
	target.hookMask = mask
	target.hookCount = count
	target.hookCountdown = count
	target.hookOldPc = 0
	// U5f: stock db_sethook mirrors every registration into the registry's
	// hook table (hooktable[L1] = the Lua hook), weak-keyed so dead threads
	// drop out — debug.getregistry()._HOOKKEY is the Lua-visible surface
	getHookTable(L).RawSet(target, fn)
	return 0
}

// debugGetHook is stock db_gethook: nil alone when no hook is set, else
// (hook, mask string in c/r/l order, count).
func debugGetHook(L *LState) int {
	target := L
	if th, ok := L.Get(1).(*LState); ok {
		target = th
	}
	if target.hookFn == nil {
		L.Push(LNil)
		return 1
	}
	mask := ""
	if target.hookMask&hookMaskCall != 0 {
		mask += "c"
	}
	if target.hookMask&hookMaskRet != 0 {
		mask += "r"
	}
	if target.hookMask&hookMaskLine != 0 {
		mask += "l"
	}
	L.Push(target.hookFn)
	L.Push(LString(mask))
	L.Push(LInteger(target.hookCount))
	return 3
}

// debugGetLocal is stock db_getlocal: the optional-thread form, the
// function-first arm (a function argument answers its PARAMETER names —
// lua_getlocal(L, NULL, n), the locals live at function start — name
// only, no value; a non-Lua function pushes nil), and the stack-level
// arm with negative indices addressing the varargs.
func debugGetLocal(L *LState) int {
	arg := 0
	L1 := L
	if th, ok := L.Get(1).(*LState); ok {
		L1 = th
		arg = 1
	}
	idx := L.CheckInt(arg + 2)
	if fn, ok := L.Get(arg + 1).(*LFunction); ok {
		// U5f: stock's function arm — luaF_getlocalname(p, n, 0): the
		// n-th local live at pc 0 is the n-th parameter (db.lua:267's
		// parameter-names battery; debug.getlocal(print, 1) → nil)
		if fn.IsG || fn.Proto == nil {
			L.Push(LNil)
			return 1
		}
		for _, loc := range fn.Proto.DbgLocals {
			if loc.StartPc == 0 {
				idx--
				if idx == 0 {
					L.Push(LString(loc.Name))
					return 1
				}
			}
		}
		L.Push(LNil)
		return 1
	}
	level := L.CheckInt(arg + 1)
	dbg, ok := L1.GetStack(level)
	if !ok {
		L.ArgError(arg+1, "level out of range")
	}
	name, value := L1.GetLocal(dbg, idx)
	if len(name) > 0 {
		L.Push(LString(name))
		L.Push(value)
		return 2
	}
	// U5a closeout (stock db_getlocal + lauxlib.h): no such local pushes
	// FAIL — and the vendored 5.5.1 default maps luaL_pushfail to NIL
	// (LUA_FAILISFALSE is off; the earlier false was a misread — the
	// oracle prints nil here, pinned by _glua-tests/db.lua:47)
	L.Push(LNil)
	return 1
}

func debugGetMetatable(L *LState) int {
	L.Push(L.GetMetatable(L.CheckAny(1)))
	return 1
}

// debugGetRegistry is stock db_getregistry: the state's registry table
// (the fork's G.Registry — shared across a state family's threads the way
// stock's per-lua_State registry is shared by its coroutines). Holds
// _LOADED and the sethook mirror table _HOOKKEY.
func debugGetRegistry(L *LState) int {
	L.Push(L.G.Registry)
	return 1
}

// hookKeyName is ldblib's HOOKKEY: the registry field mapping threads to
// their current hook functions (U5f — the Lua-visible mirror of the fork's
// per-thread hookFn).
const hookKeyName = "_HOOKKEY"

// getHookTable is stock gethooktable (ldblib): the registry's _HOOKKEY
// subtable, created lazily on first use with __mode "k" — and, like
// stock, its own metatable (metatable(hooktable) = hooktable), which is
// what db.lua:328's getmetatable(...).__mode reads.
func getHookTable(L *LState) *LTable {
	reg := L.G.Registry
	if t, ok := reg.RawGetString(hookKeyName).(*LTable); ok {
		return t
	}
	t := L.NewTable()
	t.RawSetString("__mode", LString("k"))
	t.Metatable = t
	reg.RawSetString(hookKeyName, t)
	return t
}

func debugGetUpvalue(L *LState) int {
	fn := L.CheckFunction(1)
	idx := L.CheckInt(2)
	// U5a (stock auxupvalue): no such upvalue returns ZERO values. U5f:
	// in range is TWO values even for a C (Go) closure, whose upvalue
	// name is "" — not absent (db.lua:597)
	if idx < 1 || idx > len(fn.Upvalues) {
		return 0
	}
	name, value := L.GetUpvalue(fn, idx)
	L.Push(LString(name))
	L.Push(value)
	return 2
}

// debugSetLocal is stock db_setlocal: the optional-thread form (the
// level error's index shifts with it), negative indices writing the
// vararg cells.
func debugSetLocal(L *LState) int {
	arg := 0
	L1 := L
	if th, ok := L.Get(1).(*LState); ok {
		L1 = th
		arg = 1
	}
	level := L.CheckInt(arg + 1)
	idx := L.CheckInt(arg + 2)
	value := L.CheckAny(arg + 3)
	dbg, ok := L1.GetStack(level)
	if !ok {
		L.ArgError(arg+1, "level out of range")
	}
	name := L1.SetLocal(dbg, idx, value)
	if len(name) > 0 {
		L.Push(LString(name))
	} else {
		L.Push(LNil)
	}
	return 1
}

func debugSetMetatable(L *LState) int {
	L.CheckTypes(2, LTNil, LTTable)
	obj := L.Get(1)
	mt := L.Get(2)
	L.SetMetatable(obj, mt)
	L.SetTop(1)
	return 1
}

func debugSetUpvalue(L *LState) int {
	fn := L.CheckFunction(1)
	idx := L.CheckInt(2)
	value := L.CheckAny(3)
	// U5a (stock auxupvalue): no such upvalue returns ZERO values; U5f:
	// in range pushes the name — "" for a C (Go) closure's upvalue
	if idx < 1 || idx > len(fn.Upvalues) {
		return 0
	}
	name := L.SetUpvalue(fn, idx, value)
	L.Push(LString(name))
	return 1
}

// debugUpvalueId is stock db_upvalueid (U5c): the stable identity of a
// closure's upvalue cell as a light userdata — same cell, same object;
// out of range or a builtin (Go) function answers nil (checkupval's
// integer index check precedes the closure check, like stock).
func debugUpvalueId(L *LState) int {
	n := L.CheckInt(2)
	fn := L.CheckFunction(1)
	if !fn.IsG && n >= 1 && n <= len(fn.Upvalues) {
		L.Push(fn.Upvalues[n-1].UpvalueID())
		return 1
	}
	L.Push(LNil)
	return 1
}

// debugUpvalueJoin is stock db_upvaluejoin: f1's upvalue n1 comes to
// share f2's upvalue n2 CELL — the same *Upvalue pointer, so writes
// through either closure are visible in the other and a later close of
// the owning frame publishes into every alias. Both indices must name
// existing upvalues (checkupval's "invalid upvalue index"), and the
// argchecks fire in stock's order: both index pairs first, then "Lua
// function expected" on 1 and 3 (a builtin with in-range upvalues).
func debugUpvalueJoin(L *LState) int {
	n1 := L.CheckInt(2)
	fn1 := L.CheckFunction(1)
	n2 := L.CheckInt(4)
	fn2 := L.CheckFunction(3)
	if n1 < 1 || n1 > len(fn1.Upvalues) {
		L.ArgError(2, "invalid upvalue index")
	}
	if n2 < 1 || n2 > len(fn2.Upvalues) {
		L.ArgError(4, "invalid upvalue index")
	}
	if fn1.IsG {
		L.ArgError(1, "Lua function expected")
	}
	if fn2.IsG {
		L.ArgError(3, "Lua function expected")
	}
	fn1.Upvalues[n1-1] = fn2.Upvalues[n2-1]
	return 0
}

// debugGetUserValue is stock db_getuservalue (U5c): non-FULL userdata
// (anything else, light userdata included) answers a single nil; an
// in-range slot answers value+true; n<=0 or past the capacity answers
// the single nil lua_getiuservalue pushed.
func debugGetUserValue(L *LState) int {
	n := L.OptInt(2, 1)
	if ud, ok := L.Get(1).(*LUserData); ok && !ud.Light && n >= 1 && n <= int(ud.UserValues) {
		L.Push(ud.UserValue)
		L.Push(LTrue)
		return 2
	}
	L.Push(LNil)
	return 1
}

// debugSetUserValue is stock db_setuservalue (U5c): argument 1 must be
// FULL userdata (light userdata's "userdata expected, got light
// userdata" is errors.lua's battery); the value is any Lua value
// (lua_settop(2) drops the rest) and is returned on success; a failed
// index answers a single nil (luaL_pushfail under the vendored
// FAILISFALSE-off default).
func debugSetUserValue(L *LState) int {
	L.CheckType(1, LTUserData)
	L.CheckAny(2)
	n := L.OptInt(3, 1)
	ud := L.Get(1).(*LUserData)
	if n == 1 && ud.UserValues >= 1 {
		ud.UserValue = L.Get(2)
		L.SetTop(2)
		return 1
	}
	L.Push(LNil)
	return 1
}

// debugTraceback is stock db_traceback (U5f): traceback([thread,] msg
// [, level]). lua_tostring converts a string or NUMBER message; a
// non-string non-nil message is returned untouched (traceback(print)
// == print, db.lua:705); absent/nil traces without a message. The
// level default is 1 for the running thread, 0 cross-thread.
func debugTraceback(L *LState) int {
	arg := 0
	L1 := L
	if th, ok := L.Get(1).(*LState); ok {
		L1 = th
		arg = 1
	}
	v := L.Get(arg + 1)
	var msg *string
	switch v.(type) {
	case LString:
		s := string(v.(LString))
		msg = &s
	case LInteger, LNumber:
		s := LVAsString(v)
		msg = &s
	default:
		if v != LNil {
			// non-string 'msg': return it untouched
			L.Push(v)
			return 1
		}
	}
	deflevel := 0
	if L == L1 {
		deflevel = 1
	}
	level := L.OptInt(arg+2, deflevel)
	L.Push(LString(L1.stackTrace(msg, level)))
	return 1
}
