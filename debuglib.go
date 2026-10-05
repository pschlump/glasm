package lua

import (
	"fmt"
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
	"getlocal":     debugGetLocal,
	"getmetatable": debugGetMetatable,
	"getupvalue":   debugGetUpvalue,
	"getuservalue": debugGetUserValue,
	"setlocal":     debugSetLocal,
	"setmetatable": debugSetMetatable,
	"setupvalue":   debugSetUpvalue,
	"setuservalue": debugSetUserValue,
	"traceback":    debugTraceback,
	"upvalueid":    debugUpvalueId,
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

func debugGetLocal(L *LState) int {
	level := L.CheckInt(1)
	idx := L.CheckInt(2)
	dbg, ok := L.GetStack(level)
	if !ok {
		L.ArgError(1, "level out of range")
	}
	name, value := L.GetLocal(dbg, idx)
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

func debugGetUpvalue(L *LState) int {
	fn := L.CheckFunction(1)
	idx := L.CheckInt(2)
	name, value := L.GetUpvalue(fn, idx)
	if len(name) > 0 {
		L.Push(LString(name))
		L.Push(value)
		return 2
	}
	// U5a (stock auxupvalue): no such upvalue returns ZERO values
	return 0
}

func debugSetLocal(L *LState) int {
	level := L.CheckInt(1)
	idx := L.CheckInt(2)
	value := L.CheckAny(3)
	dbg, ok := L.GetStack(level)
	if !ok {
		L.ArgError(1, "level out of range")
	}
	name := L.SetLocal(dbg, idx, value)
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
	name := L.SetUpvalue(fn, idx, value)
	if len(name) > 0 {
		L.Push(LString(name))
		return 1
	}
	// U5a (stock auxupvalue): no such upvalue returns ZERO values
	return 0
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

func debugTraceback(L *LState) int {
	msg := ""
	level := L.OptInt(2, 1)
	ls := L
	if L.GetTop() > 0 {
		if s, ok := L.Get(1).(LString); ok {
			msg = string(s)
		}
		if l, ok := L.Get(1).(*LState); ok {
			ls = l
			msg = ""
		}
	}

	traceback := strings.TrimSpace(ls.stackTrace(level))
	if len(msg) > 0 {
		traceback = fmt.Sprintf("%s\n%s", msg, traceback)
	}
	L.Push(LString(traceback))
	return 1
}
