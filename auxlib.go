package lua

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
)

/* checkType {{{ */

func (ls *LState) CheckAny(n int) LValue {
	if n > ls.GetTop() {
		ls.ArgError(n, "value expected")
	}
	return ls.Get(n)
}

func (ls *LState) CheckInt(n int) int {
	v := ls.Get(n)
	if intv, ok := v.(LInteger); ok {
		return int(intv)
	}
	if intv, ok := v.(LNumber); ok {
		return int(intv)
	}
	ls.TypeError(n, LTNumber)
	return 0
}

func (ls *LState) CheckInt64(n int) int64 {
	v := ls.Get(n)
	if intv, ok := v.(LInteger); ok {
		return int64(intv)
	}
	if intv, ok := v.(LNumber); ok {
		return int64(intv)
	}
	ls.TypeError(n, LTNumber)
	return 0
}

func (ls *LState) CheckNumber(n int) LNumber {
	v := ls.Get(n)
	if lv, ok := v.(LNumber); ok {
		return lv
	}
	if lv, ok := v.(LInteger); ok {
		return LNumber(float64(lv))
	}
	if lv, ok := v.(LString); ok {
		if num, err := parseNumber(string(lv)); err == nil {
			return LVAsNumber(num)
		}
	}
	ls.TypeError(n, LTNumber)
	return 0
}

func (ls *LState) CheckString(n int) string {
	v := ls.Get(n)
	if lv, ok := v.(LString); ok {
		return string(lv)
	} else if LVCanConvToString(v) {
		return ls.ToString(n)
	}
	ls.TypeError(n, LTString)
	return ""
}

func (ls *LState) CheckBool(n int) bool {
	v := ls.Get(n)
	if lv, ok := v.(LBool); ok {
		return bool(lv)
	}
	ls.TypeError(n, LTBool)
	return false
}

func (ls *LState) CheckTable(n int) *LTable {
	v := ls.Get(n)
	if lv, ok := v.(*LTable); ok {
		return lv
	}
	ls.TypeError(n, LTTable)
	return nil
}

func (ls *LState) CheckFunction(n int) *LFunction {
	v := ls.Get(n)
	if lv, ok := v.(*LFunction); ok {
		return lv
	}
	ls.TypeError(n, LTFunction)
	return nil
}

func (ls *LState) CheckUserData(n int) *LUserData {
	v := ls.Get(n)
	if lv, ok := v.(*LUserData); ok {
		return lv
	}
	ls.TypeError(n, LTUserData)
	return nil
}

func (ls *LState) CheckThread(n int) *LState {
	v := ls.Get(n)
	if lv, ok := v.(*LState); ok {
		return lv
	}
	ls.TypeError(n, LTThread)
	return nil
}

func (ls *LState) CheckType(n int, typ LValueType) {
	v := ls.Get(n)
	if v.Type() != typ {
		ls.TypeError(n, typ)
	}
}

func (ls *LState) CheckTypes(n int, typs ...LValueType) {
	vt := ls.Get(n).Type()
	for _, typ := range typs {
		// the integer subtype satisfies a "number" expectation
		if vt == typ || (typ == LTNumber && vt == LTInteger) {
			return
		}
	}
	buf := []string{}
	for _, typ := range typs {
		buf = append(buf, typ.String())
	}
	ls.ArgError(n, strings.Join(buf, " or ")+" expected, got "+ls.Get(n).Type().String())
}

func (ls *LState) CheckOption(n int, options []string) int {
	str := ls.CheckString(n)
	for i, v := range options {
		if v == str {
			return i
		}
	}
	ls.ArgError(n, fmt.Sprintf("invalid option: %s (must be one of %s)", str, strings.Join(options, ",")))
	return 0
}

/* }}} */

/* optType {{{ */

func (ls *LState) OptInt(n int, d int) int {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if intv, ok := v.(LInteger); ok {
		return int(intv)
	}
	if intv, ok := v.(LNumber); ok {
		return int(intv)
	}
	ls.TypeError(n, LTNumber)
	return 0
}

func (ls *LState) OptInt64(n int, d int64) int64 {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if intv, ok := v.(LInteger); ok {
		return int64(intv)
	}
	if intv, ok := v.(LNumber); ok {
		return int64(intv)
	}
	ls.TypeError(n, LTNumber)
	return 0
}

func (ls *LState) OptNumber(n int, d LNumber) LNumber {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if lv, ok := v.(LInteger); ok {
		return LNumber(float64(lv))
	}
	if lv, ok := v.(LNumber); ok {
		return lv
	}
	ls.TypeError(n, LTNumber)
	return 0
}

func (ls *LState) OptString(n int, d string) string {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if lv, ok := v.(LString); ok {
		return string(lv)
	}
	ls.TypeError(n, LTString)
	return ""
}

func (ls *LState) OptBool(n int, d bool) bool {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if lv, ok := v.(LBool); ok {
		return bool(lv)
	}
	ls.TypeError(n, LTBool)
	return false
}

func (ls *LState) OptTable(n int, d *LTable) *LTable {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if lv, ok := v.(*LTable); ok {
		return lv
	}
	ls.TypeError(n, LTTable)
	return nil
}

func (ls *LState) OptFunction(n int, d *LFunction) *LFunction {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if lv, ok := v.(*LFunction); ok {
		return lv
	}
	ls.TypeError(n, LTFunction)
	return nil
}

func (ls *LState) OptUserData(n int, d *LUserData) *LUserData {
	v := ls.Get(n)
	if v == LNil {
		return d
	}
	if lv, ok := v.(*LUserData); ok {
		return lv
	}
	ls.TypeError(n, LTUserData)
	return nil
}

/* }}} */

/* error operations {{{ */

// argErrorName resolves the current (G) function's name for argument
// errors: the call-site name if there is one (stock getfuncname), else
// the registry/globals search (stock pushglobalfuncname — how pcall'd
// library functions still report 'math.random'), else '?'.
func (ls *LState) argErrorName() (string, bool) {
	name, isMethod, isChunk := ls.frameFuncNameFull(ls.currentFrame)
	if isChunk || name == "(anonymous)" {
		if path, ok := ls.pushGlobalFuncName(ls.currentFrame.Fn); ok {
			return path, isMethod
		}
		return "?", isMethod
	}
	return name, isMethod
}

// pushGlobalFuncName ports lauxlib.c pushglobalfuncname: a depth-2 DFS
// over the registry's _LOADED table (then a depth-1 pass over the
// globals table) for a path naming the function value, with stock's
// "_G." prefix strip (_LOADED["_G"] IS the globals table).
func (ls *LState) pushGlobalFuncName(fn *LFunction) (string, bool) {
	var find func(t *LTable, level int) (string, bool)
	seen := map[*LTable]bool{}
	find = func(t *LTable, level int) (string, bool) {
		if t == nil || seen[t] || level <= 0 {
			return "", false
		}
		seen[t] = true
		// first pass: direct function fields; second: one level deeper
		// through sub-tables (string keys only, like stock findfield)
		var hit string
		t.ForEach(func(key, v LValue) {
			if hit != "" {
				return
			}
			if _, ok := key.(LString); !ok {
				return
			}
			if f, ok := v.(*LFunction); ok && f == fn {
				hit = keyStr(key)
			}
		})
		if hit != "" {
			return hit, true
		}
		t.ForEach(func(key, v LValue) {
			if hit != "" {
				return
			}
			if _, ok := key.(LString); !ok {
				return
			}
			if sub, ok := v.(*LTable); ok {
				if name, ok2 := find(sub, level-1); ok2 {
					hit = keyStr(key) + "." + name
				}
			}
		})
		return hit, hit != ""
	}
	if loaded, ok := ls.G.Registry.RawGetString("_LOADED").(*LTable); ok {
		if name, ok2 := find(loaded, 2); ok2 {
			return strings.TrimPrefix(name, "_G."), true
		}
	}
	if name, ok := find(ls.G.Global, 1); ok {
		return name, true
	}
	return "", false
}

func keyStr(key LValue) string {
	if s, ok := key.(LString); ok {
		return string(s)
	}
	return fmt.Sprint(key)
}

// argPresent distinguishes a MISSING argument (stock LUA_TNONE → "no
// value" in type errors) from an explicit nil.
func (ls *LState) argPresent(n int) bool {
	return n >= 1 && ls.currentLocalBase()+n-1 < ls.reg.Top()
}

// argError is stock 5.5 luaL_argerror: colon-syntax call sites do not
// count the implicit self in the argument number, with a dedicated
// bad-self error for the first argument.
func (ls *LState) argError(n int, message string) {
	name, isMethod := ls.argErrorName()
	arg := n
	if isMethod {
		if arg == 1 {
			ls.RaiseError("calling '%s' on bad self (%s)", name, message)
		}
		arg--
	}
	ls.RaiseError("bad argument #%v to '%v' (%v)", arg, name, message)
}

func (ls *LState) ArgError(n int, message string) {
	ls.argError(n, message)
}

// TypeError is luaL_typeerror with the fork's type-tag spelling; the
// actual-operand side honors __name and reports "no value" when the
// argument is absent.
func (ls *LState) TypeError(n int, typ LValueType) {
	ls.typeErrorStr(n, typ.String())
}

func (ls *LState) typeErrorStr(n int, tname string) {
	actual := "no value"
	if ls.argPresent(n) {
		v := ls.Get(n)
		actual = v.Type().String()
		if name, ok := ls.GetMetaField(v, "__name").(LString); ok {
			actual = string(name)
		}
	}
	ls.argError(n, tname+" expected, got "+actual)
}

/* }}} */

/* debug operations {{{ */

func (ls *LState) Where(level int) string {
	return ls.where(level, false)
}

/* }}} */

/* table operations {{{ */

func (ls *LState) FindTable(obj *LTable, n string, size int) LValue {
	names := strings.Split(n, ".")
	curobj := obj
	for _, name := range names {
		if curobj.Type() != LTTable {
			return LNil
		}
		nextobj := ls.RawGet(curobj, LString(name))
		if nextobj == LNil {
			tb := ls.CreateTable(0, size)
			ls.RawSet(curobj, LString(name), tb)
			curobj = tb
		} else if nextobj.Type() != LTTable {
			return LNil
		} else {
			curobj = nextobj.(*LTable)
		}
	}
	return curobj
}

/* }}} */

/* register operations {{{ */

func (ls *LState) RegisterModule(name string, funcs map[string]LGFunction) LValue {
	tb := ls.FindTable(ls.Get(RegistryIndex).(*LTable), "_LOADED", 1)
	mod := ls.GetField(tb, name)
	if mod.Type() != LTTable {
		newmod := ls.FindTable(ls.Get(GlobalsIndex).(*LTable), name, len(funcs))
		if newmodtb, ok := newmod.(*LTable); !ok {
			ls.RaiseError("name conflict for module(%v)", name)
		} else {
			for fname, fn := range funcs {
				newmodtb.RawSetString(fname, ls.NewFunction(fn))
			}
			ls.SetField(tb, name, newmodtb)
			return newmodtb
		}
	}
	return mod
}

func (ls *LState) SetFuncs(tb *LTable, funcs map[string]LGFunction, upvalues ...LValue) *LTable {
	for fname, fn := range funcs {
		tb.RawSetString(fname, ls.NewClosure(fn, upvalues...))
	}
	return tb
}

/* }}} */

/* metatable operations {{{ */

func (ls *LState) NewTypeMetatable(typ string) *LTable {
	regtable := ls.Get(RegistryIndex)
	mt := ls.GetField(regtable, typ)
	if tb, ok := mt.(*LTable); ok {
		return tb
	}
	mtnew := ls.NewTable()
	ls.SetField(regtable, typ, mtnew)
	return mtnew
}

func (ls *LState) GetMetaField(obj LValue, event string) LValue {
	return ls.metaOp1(obj, event)
}

func (ls *LState) GetTypeMetatable(typ string) LValue {
	return ls.GetField(ls.Get(RegistryIndex), typ)
}

func (ls *LState) CallMeta(obj LValue, event string) LValue {
	op := ls.metaOp1(obj, event)
	if op.Type() == LTFunction {
		ls.reg.Push(op)
		ls.reg.Push(obj)
		ls.Call(1, 1)
		return ls.reg.Pop()
	}
	return LNil
}

/* }}} */

/* load and function call operations {{{ */

func (ls *LState) LoadFile(path string) (*LFunction, error) {
	var file *os.File
	var err error
	if len(path) == 0 {
		file = os.Stdin
	} else {
		file, err = os.Open(path)
		defer file.Close()
		if err != nil {
			return nil, newApiErrorE(ApiErrorFile, err)
		}
	}

	reader := bufio.NewReader(file)
	// get the first character.
	c, err := reader.ReadByte()
	if err != nil && err != io.EOF {
		return nil, newApiErrorE(ApiErrorFile, err)
	}
	if c == byte('#') {
		// Unix exec. file?
		// skip first line
		_, err, _ = readBufioLine(reader)
		if err != nil {
			return nil, newApiErrorE(ApiErrorFile, err)
		}
	}

	if err != io.EOF {
		// if the file is not empty,
		// unread the first character of the file or newline character(readBufioLine's last byte).
		err = reader.UnreadByte()
		if err != nil {
			return nil, newApiErrorE(ApiErrorFile, err)
		}
	}

	return ls.Load(reader, "@"+path)
}

func (ls *LState) LoadString(source string) (*LFunction, error) {
	return ls.Load(strings.NewReader(source), "=<string>")
}

func (ls *LState) DoFile(path string) error {
	if fn, err := ls.LoadFile(path); err != nil {
		return err
	} else {
		ls.Push(fn)
		return ls.PCall(0, MultRet, nil)
	}
}

func (ls *LState) DoString(source string) error {
	if fn, err := ls.LoadString(source); err != nil {
		return err
	} else {
		ls.Push(fn)
		return ls.PCall(0, MultRet, nil)
	}
}

/* }}} */

/* GopherLua original APIs {{{ */

// ToStringMeta returns string representation of given LValue — the
// luaL_tolstring port (U3h): __tostring's result must be a string (a
// number converts, like lua_isstring); the default rendering for
// tables/functions/etc. is "kind: 0x..." with kind from a string
// __name metafield when present.
func (ls *LState) ToStringMeta(lv LValue) LValue {
	if fn, ok := ls.metaOp1(lv, "__tostring").(*LFunction); ok {
		ls.Push(fn)
		ls.Push(lv)
		ls.Call(1, 1)
		ret := ls.reg.Pop()
		switch ret.(type) {
		case LString, LNumber, LInteger:
			return LString(LVAsString(ret))
		}
		ls.RaiseError("'__tostring' must return a string")
	}
	if name, ok := ls.GetMetaField(lv, "__name").(LString); ok {
		return LString(fmt.Sprintf("%s: %p", string(name), lv))
	}
	return LString(lv.String())
}

// Set a module loader to the package.preload table.
func (ls *LState) PreloadModule(name string, loader LGFunction) {
	preload := ls.GetField(ls.GetField(ls.Get(EnvironIndex), "package"), "preload")
	if _, ok := preload.(*LTable); !ok {
		ls.RaiseError("package.preload must be a table")
	}
	ls.SetField(preload, name, ls.NewFunction(loader))
}

// Checks whether the given index is an LChannel and returns this channel.
func (ls *LState) CheckChannel(n int) chan LValue {
	v := ls.Get(n)
	if ch, ok := v.(LChannel); ok {
		return (chan LValue)(ch)
	}
	ls.TypeError(n, LTChannel)
	return nil
}

// If the given index is a LChannel, returns this channel. If this argument is absent or is nil, returns ch. Otherwise, raises an error.
func (ls *LState) OptChannel(n int, ch chan LValue) chan LValue {
	v := ls.Get(n)
	if v == LNil {
		return ch
	}
	if ch, ok := v.(LChannel); ok {
		return (chan LValue)(ch)
	}
	ls.TypeError(n, LTChannel)
	return nil
}

/* }}} */

//
