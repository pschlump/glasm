package lua

import (
	"sort"
)

/* table library — the stock 5.5.1 surface (U3d), ported from
 * runtime/lua55/src/ltablib.c. Since 5.3 every function honors
 * metamethods: element access goes through __index/__newindex (lua_geti/
 * lua_seti) and length through __len (luaL_len), per-function per the
 * checktab read/write/len requirements — non-tables need the matching
 * metamethods or the "table expected" type error (strings are exempt
 * from __len: they have a native length). */

const (
	tabRead  = 1
	tabWrite = 2
	tabLen   = 4
)

// tabCheckTab ports checktab: accept a table, or a non-table whose
// metatable provides the required metamethods (strings need no __len).
func tabCheckTab(L *LState, arg int, what int) {
	v := L.Get(arg)
	if v.Type() == LTTable {
		return
	}
	mt := L.GetMetatable(v)
	if mt != LNil {
		has := func(key string) bool {
			return L.GetMetaField(v, key) != LNil
		}
		ok := true
		if what&tabRead != 0 && !has("__index") {
			ok = false
		}
		if ok && what&tabWrite != 0 && !has("__newindex") {
			ok = false
		}
		if ok && what&tabLen != 0 && v.Type() != LTString && !has("__len") {
			ok = false
		}
		if ok {
			return
		}
	}
	L.TypeError(arg, LTTable)
}

// tabGetI ports lua_geti: __index-honoring integer-keyed read.
func tabGetI(L *LState, obj LValue, i int64) LValue {
	return L.GetTable(obj, LInteger(i))
}

// tabSetI ports lua_seti: __newindex-honoring integer-keyed write.
func tabSetI(L *LState, obj LValue, i int64, v LValue) {
	L.SetTable(obj, LInteger(i), v)
}

// tabAuxGetN ports aux_getn: checktab(R|W|len flags) then luaL_len.
func tabAuxGetN(L *LState, arg int, what int) int64 {
	tabCheckTab(L, arg, what|tabLen)
	return tabLenOf(L, L.Get(arg))
}

// tabLenOf ports luaL_len: __len-honoring length; the metamethod's
// result must be a number convertible to an integer.
func tabLenOf(L *LState, v LValue) int64 {
	if v.Type() == LTString {
		return int64(len(string(v.(LString))))
	}
	if op := L.metaOp1(v, "__len"); op.Type() == LTFunction {
		L.Push(op)
		L.Push(v)
		L.Call(1, 1)
		ret := L.reg.Pop()
		if iv, ok := toIntegerX(ret); ok {
			return iv
		}
		if isNumberValue(ret) {
			L.RaiseError("object length is not an integer")
		}
		L.RaiseError("object length is not a number")
	}
	if v.Type() == LTTable {
		return int64(v.(*LTable).Len())
	}
	return 0
}

func OpenTable(L *LState) int {
	tabmod := L.RegisterModule(TabLibName, tableFuncs)
	L.Push(tabmod)
	return 1
}

var tableFuncs = map[string]LGFunction{
	"concat": tableConcat,
	"create": tableCreate,
	"insert": tableInsert,
	"pack":   tablePack,
	"unpack": tableUnpack,
	"remove": tableRemove,
	"move":   tableMove,
	"sort":   tableSort,
	// U3b (D8): getn/maxn are gone — # and table.unpack replaced them in
	// 5.2/5.3.
}

// tableCreate ports tcreate (5.5): sequence-size and hash-size hints,
// each capped at INT_MAX ("out of range").
func tableCreate(L *LState) int {
	sizeseq := uint64(checkInteger(L, 1))
	sizerest := uint64(optInteger(L, 2, 0))
	if sizeseq > 1<<31-1 {
		L.ArgError(1, "out of range")
	}
	if sizerest > 1<<31-1 {
		L.ArgError(2, "out of range")
	}
	L.Push(L.CreateTable(int(sizeseq), int(sizerest)))
	return 1
}

// tableInsert ports tinsert: 2 args append at e+1; 3 args insert at pos
// in [1, e] ("position out of bounds"), shifting up through geti/seti;
// any other arity is "wrong number of arguments to 'insert'".
func tableInsert(L *LState) int {
	t := L.Get(1)
	e := tabAuxGetN(L, 1, tabRead|tabWrite) + 1 // first empty element
	var pos int64
	var v LValue
	switch L.GetTop() {
	case 2:
		pos = e
		v = L.Get(2)
	case 3:
		pos = checkInteger(L, 2)
		v = L.Get(3)
		if !(uint64(pos)-1 < uint64(e)) {
			L.ArgError(2, "position out of bounds")
		}
		for i := e; i > pos; i-- {
			tabSetI(L, t, i, tabGetI(L, t, i-1))
		}
	default:
		L.RaiseError("wrong number of arguments to 'insert'")
	}
	tabSetI(L, t, pos, v)
	return 0
}

// tableRemove ports tremove: pos defaults to the size; a given pos is
// validated in [1, size+1]; shifts down and nils the last slot.
func tableRemove(L *LState) int {
	t := L.Get(1)
	size := tabAuxGetN(L, 1, tabRead|tabWrite)
	pos := optInteger(L, 2, size)
	if pos != size {
		if !(uint64(pos)-1 <= uint64(size)) {
			L.ArgError(2, "position out of bounds")
		}
	}
	L.Push(tabGetI(L, t, pos))
	for ; pos < size; pos++ {
		tabSetI(L, t, pos, tabGetI(L, t, pos+1))
	}
	tabSetI(L, t, pos, LNil)
	return 1
}

// tableMove ports tmove: copy 1[f..e] into tt[t..]; ascending unless the
// ranges overlap forward in the same table; overflow argchecks; returns
// the destination.
func tableMove(L *LState) int {
	f := checkInteger(L, 2)
	e := checkInteger(L, 3)
	t := checkInteger(L, 4)
	tt := 1 // destination table: arg 1 by default
	if v := L.Get(5); v != LNil && v.Type() != LTNil {
		tt = 5
	}
	src := L.Get(1)
	dst := L.Get(tt)
	tabCheckTab(L, 1, tabRead)
	tabCheckTab(L, tt, tabWrite)
	if e >= f {
		if !(f > 0 || e < 1<<63-1+f) {
			L.ArgError(3, "too many elements to move")
		}
		n := e - f + 1
		if !(t <= 1<<63-1-n+1) {
			L.ArgError(4, "destination wrap around")
		}
		// ascending whenever the destination is wholly after the source,
		// wholly before it, or a different table (stock tmove)
		destDiffers := tt != 1 && src != dst
		if t > e || t <= f || destDiffers {
			for i := int64(0); i < n; i++ {
				tabSetI(L, dst, t+i, tabGetI(L, src, f+i))
			}
		} else {
			for i := n - 1; i >= 0; i-- {
				tabSetI(L, dst, t+i, tabGetI(L, src, f+i))
			}
		}
	}
	L.Push(dst)
	return 1
}

// tableConcat ports tconcat: elements [i..j] (i default 1, j default
// length) joined by sep (default ""), each element readable through
// __index and convertible to a string.
func tableConcat(L *LState) int {
	t := L.Get(1)
	last := tabAuxGetN(L, 1, tabRead)
	sep := L.OptString(2, "")
	i := optInteger(L, 3, 1)
	last = optInteger(L, 4, last)
	buf := ""
	addfield := func(idx int64) {
		v := tabGetI(L, t, idx)
		switch v.(type) {
		case LString, LNumber, LInteger:
			buf += LVAsString(v)
		default:
			L.RaiseError("invalid value (%s) at index %d in table for 'concat'", v.Type().String(), idx)
		}
	}
	for ; i < last; i++ {
		addfield(i)
		buf += sep
	}
	if i == last { // interval not empty
		addfield(i)
	}
	L.Push(LString(buf))
	return 1
}

// tablePack ports tpack: {n = count, ...}.
func tablePack(L *LState) int {
	n := L.GetTop()
	t := L.NewTable()
	for i := 1; i <= n; i++ {
		t.RawSetInt(i, L.Get(i))
	}
	t.RawSetString("n", LInteger(n))
	L.Push(t)
	return 1
}

// tableUnpack ports tunpack: list[i..j] (i default 1, j default length),
// i > j → no results.
func tableUnpack(L *LState) int {
	t := L.Get(1)
	length := tabAuxGetN(L, 1, tabRead)
	i := optInteger(L, 2, 1)
	e := optInteger(L, 3, length)
	if i > e {
		return 0
	}
	n := uint64(e) - uint64(i) + 1
	if n >= 1<<31-1 {
		L.RaiseError("too many results to unpack")
	}
	for ; i < e; i++ {
		L.Push(tabGetI(L, t, i))
	}
	L.Push(tabGetI(L, t, e))
	return int(n)
}

// tabSorter adapts 1-based metamethod-aware table access to
// sort.Interface (the old raw-array lValueArraySorter stays in table.go
// for other users).
type tabSorter struct {
	L  *LState
	Fn *LFunction
	t  LValue
	n  int64
}

func (ts *tabSorter) get(i int64) LValue {
	return tabGetI(ts.L, ts.t, i)
}

func (ts *tabSorter) set(i int64, v LValue) {
	tabSetI(ts.L, ts.t, i, v)
}

func (ts *tabSorter) Len() int { return int(ts.n) }

func (ts *tabSorter) Less(a, b int) bool {
	if ts.Fn == nil {
		return lessThan(ts.L, ts.get(int64(a+1)), ts.get(int64(b+1)))
	}
	ts.L.Push(ts.Fn)
	ts.L.Push(ts.get(int64(a + 1)))
	ts.L.Push(ts.get(int64(b + 1)))
	ts.L.Call(2, 1)
	return LVAsBool(ts.L.reg.Pop())
}

func (ts *tabSorter) Swap(a, b int) {
	va := ts.get(int64(a + 1))
	vb := ts.get(int64(b + 1))
	ts.set(int64(a+1), vb)
	ts.set(int64(b+1), va)
}

// tableSort: the fork's Go sort over the metamethod-aware accessors
// with stock's error surface (comparator errors propagate as the sort's
// error; stock's "invalid order function for sorting" fires on
// inconsistent comparators — the corpus avoids relying on either
// engine's equal-key order, which stays engine-specific: stock's pivot
// randomization is makeseed-pinned and hash-order-dependent, the interp
// uses sort.Stable).
func tableSort(L *LState) int {
	t := L.Get(1)
	n := tabAuxGetN(L, 1, tabRead|tabWrite)
	if n >= 1<<31-1 {
		L.ArgError(1, "array too big")
	}
	var cmp *LFunction
	if L.GetTop() != 1 {
		cmp = L.CheckFunction(2)
	}
	sort.Stable(&tabSorter{L: L, Fn: cmp, t: t, n: n})
	return 0
}
