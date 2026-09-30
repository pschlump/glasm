package lua

import (
	"fmt"
	"io"
	"os"
	"runtime"
	"strings"
)

/* basic functions {{{ */

func OpenBase(L *LState) int {
	global := L.Get(GlobalsIndex).(*LTable)
	L.SetGlobal("_G", global)
	L.SetGlobal("_VERSION", LString(LuaVersion))
	L.SetGlobal("_GOPHER_LUA_VERSION", LString(PackageName+" "+PackageVersion))
	basemod := L.RegisterModule("_G", baseFuncs)
	global.RawSetString("ipairs", L.NewClosure(baseIpairs, L.NewFunction(ipairsaux)))
	global.RawSetString("pairs", L.NewClosure(basePairs, L.NewFunction(pairsaux)))
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
	"rawset":         baseRawSet,
	"select":         baseSelect,
	"_printregs":     base_PrintRegs,
	"setmetatable":   baseSetMetatable,
	"tonumber":       baseToNumber,
	"tostring":       baseToString,
	"type":           baseType,
	"xpcall":         baseXPCall,
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
// not a type complaint about the message).
func baseAssert(L *LState) int {
	if L.ToBool(1) {
		return L.GetTop()
	}
	if L.GetTop() >= 2 {
		L.Error(L.Get(2), 1)
	} else {
		L.Error(LString("assertion failed!"), 1)
	}
	return 0
}

func baseCollectGarbage(L *LState) int {
	runtime.GC()
	return 0
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

func ipairsaux(L *LState) int {
	tb := L.CheckTable(1)
	i := L.CheckInt(2)
	i++
	v := tb.RawGetInt(i)
	if v == LNil {
		return 0
	} else {
		L.Pop(1)
		L.Push(LNumber(i))
		L.Push(LNumber(i))
		L.Push(v)
		return 2
	}
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
func loadaux(L *LState, reader io.Reader, chunkname string, env *LTable) int {
	if fn, err := L.LoadEnv(reader, chunkname, env); err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	} else {
		L.Push(fn)
		return 1
	}
}

// checkLoadMode validates the 5.2 mode argument ("b", "t", "bt"; default
// "bt"). The fork has no binary-chunk loader (undump is not implemented),
// so mode "b" rejects every chunk with stock's text-chunk error — the
// exact behavior a text chunk sees under stock — and "t"/"bt" accept text.
func checkLoadMode(L *LState, idx int, chunkname string) bool {
	mode := L.OptString(idx, "bt")
	if !strings.Contains(mode, "t") {
		L.Push(LNil)
		// stock's text (lbaselib loadaux): no position prefix
		L.Push(LString(fmt.Sprintf("attempt to load a text chunk (mode is '%s')", mode)))
		return false
	}
	return true
}

// loadEnvArg validates load/loadfile's env argument (nil or table) and
// returns it (nil → thread default).
func loadEnvArg(L *LState, idx int) *LTable {
	v := L.Get(idx)
	if v == LNil {
		return nil
	}
	if tb, ok := v.(*LTable); ok {
		return tb
	}
	L.ArgError(idx, fmt.Sprintf("table expected, got %s", v.Type().String()))
	return nil
}

// baseLoad implements 5.2 load: load(chunk [, chunkname [, mode [, env]]]).
// chunk is a string or a reader function; a string chunk's default
// chunkname is the chunk text itself ([string "..."] rendering), a
// function chunk's is "=(load)" (stock lbaselib).
func baseLoad(L *LState) int {
	var reader io.Reader
	var chunkname string
	switch lv := L.Get(1).(type) {
	case LString:
		reader = strings.NewReader(string(lv))
		chunkname = L.OptString(2, string(lv))
	case *LFunction:
		fn := lv
		chunkname = L.OptString(2, "=(load)")
		top := L.GetTop()
		buf := []string{}
		for {
			L.SetTop(top)
			L.Push(fn)
			L.Call(0, 1)
			ret := L.reg.Pop()
			if ret == LNil {
				break
			} else if LVCanConvToString(ret) {
				str := ret.String()
				if len(str) > 0 {
					buf = append(buf, string(str))
				} else {
					break
				}
			} else {
				L.Push(LNil)
				L.Push(LString("reader function must return a string"))
				return 2
			}
		}
		reader = strings.NewReader(strings.Join(buf, ""))
	default:
		L.ArgError(1, "string or function expected")
		return 0
	}
	if !checkLoadMode(L, 3, chunkname) {
		return 2
	}
	return loadaux(L, reader, chunkname, loadEnvArg(L, 4))
}

// baseLoadFile implements 5.2 loadfile([filename [, mode [, env]]]).
func baseLoadFile(L *LState) int {
	var reader io.Reader
	var chunkname string
	var err error
	if L.GetTop() < 1 || L.Get(1) == LNil {
		reader = os.Stdin
		chunkname = "=stdin"
	} else {
		chunkname = L.CheckString(1)
		reader, err = os.Open(chunkname)
		if err != nil {
			// stock 5.5 loadfile: "cannot open <file>: <strerror>"
			L.Push(LNil)
			L.Push(LString(fmt.Sprintf("cannot open %s: No such file or directory", chunkname)))
			return 2
		}
		defer reader.(*os.File).Close()
	}
	if !checkLoadMode(L, 2, chunkname) {
		return 2
	}
	return loadaux(L, reader, "@"+chunkname, loadEnvArg(L, 3))
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

func basePairs(L *LState) int {
	tb := L.CheckTable(1)
	L.Push(L.Get(UpvalueIndex(1)))
	L.Push(tb)
	L.Push(LNil)
	return 3
}

func basePCall(L *LState) int {
	L.CheckAny(1)
	v := L.Get(1)
	if v.Type() != LTFunction && L.GetMetaField(v, "__call").Type() != LTFunction {
		L.Push(LFalse)
		L.Push(LString("attempt to call a " + v.Type().String() + " value"))
		return 2
	}
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
		L.Push(LNumber(L.GetTop() - 1))
		return 1
	}
	return 0
}

func baseSetMetatable(L *LState) int {
	L.CheckTypes(2, LTNil, LTTable)
	obj := L.Get(1)
	if obj == LNil {
		L.RaiseError("cannot set metatable to a nil object.")
	}
	mt := L.Get(2)
	if m := L.metatable(obj, true); m != LNil {
		if tb, ok := m.(*LTable); ok && tb.RawGetString("__metatable") != LNil {
			L.RaiseError("cannot change a protected metatable")
		}
	}
	L.SetMetatable(obj, mt)
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
// are passed through to f (5.1 dropped them).
func baseXPCall(L *LState) int {
	fn := L.CheckFunction(1)
	errfunc := L.CheckFunction(2)
	nargs := L.GetTop() - 2

	// call a copy of fn with copies of the args (the originals, plus the
	// handler between them, stay below untouched)
	L.Push(fn)
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
		L.RaiseError("package.loaders must be a table")
	}
	messages := []string{}
	var modasfunc LValue
	for i := 1; ; i++ {
		loader := L.RawGetInt(loaders, i)
		if loader == LNil {
			L.RaiseError("module %s not found:\n\t%s, ", name, strings.Join(messages, "\n\t"))
		}
		L.Push(loader)
		L.Push(LString(name))
		L.Call(1, 1)
		ret := L.reg.Pop()
		switch retv := ret.(type) {
		case *LFunction:
			modasfunc = retv
			goto loopbreak
		case LString:
			messages = append(messages, string(retv))
		}
	}
loopbreak:
	L.SetField(loaded, name, loopdetection)
	L.Push(modasfunc)
	L.Push(LString(name))
	L.Call(1, 1)
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
