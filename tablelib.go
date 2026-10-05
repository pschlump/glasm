package lua

import (
	"math/rand"
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
		// stock luaL_len: lua_tointegerx failure is always "not an
		// integer" (a float like 2.5 and a string get the same text)
		L.RaiseError("object length is not an integer")
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
	// stock luaH_resize/setnodevector: an array size above 2^31 or a
	// hash size whose node count rounds past MAXHBITS (2^30) is
	// "table overflow" — raised before any allocation (sort.lua:41)
	if sizeseq > 1<<31 || sizerest > 1<<30 {
		L.RaiseError("table overflow")
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
	// stock: the span is compared BEFORE the +1 (castS2U(e) - castS2U(i),
	// so a full-range span wraps huge), then lua_checkstack rejects
	// anything past LUAI_MAXSTACK (1M) — both raise "too many results"
	n := uint64(e) - uint64(i)
	if n >= 1<<31-1 || n+1 > 1000000 {
		L.RaiseError("too many results to unpack")
	}
	n++
	for ; i < e; i++ {
		L.Push(tabGetI(L, t, i))
	}
	L.Push(tabGetI(L, t, e))
	return int(n)
}

// sortComp ports sort_comp: nil comparator = '<' via lessThan (__lt
// aware); a function's result is lua_toboolean'd.
func sortComp(L *LState, cmp *LFunction, a, b LValue) bool {
	if cmp == nil {
		// Go context (sort runs under a noyield call region): the state
		// is always mmSync
		v, _ := lessThan(L, a, b)
		return v
	}
	L.Push(cmp)
	L.Push(a)
	L.Push(b)
	L.Call(2, 1)
	return LVAsBool(L.reg.Pop())
}

// tabPartition ports stock partition: Hoare sweep with the pivot VALUE
// fixed. Both invariant violations — a[up-1] claimed less than itself,
// or a[j] > P at j < i contradicting the i-loop's exit — raise
// "invalid order function for sorting" (sort.lua:243's battery).
func tabPartition(L *LState, t LValue, cmp *LFunction, lo, up int64, P LValue) int64 {
	i, j := lo, up-1 // both pre-adjusted before first use
	for {
		for { // repeat ++i while a[i] < P
			i++
			if !sortComp(L, cmp, tabGetI(L, t, i), P) {
				break
			}
			if i == up-1 { // a[up-1] < P == a[up-1]
				L.RaiseError("invalid order function for sorting")
			}
		}
		for { // repeat --j while P < a[j]
			j--
			if !sortComp(L, cmp, P, tabGetI(L, t, j)) {
				break
			}
			if j < i { // a[j] > P contradicts the i-loop's exit
				L.RaiseError("invalid order function for sorting")
			}
		}
		if j < i { // no elements out of place
			// swap pivot (a[up-1]) with a[i] to satisfy the postcondition
			tabSetI(L, t, up-1, tabGetI(L, t, i))
			tabSetI(L, t, i, P)
			return i
		}
		ai, aj := tabGetI(L, t, i), tabGetI(L, t, j)
		tabSetI(L, t, i, aj)
		tabSetI(L, t, j, ai)
	}
}

// tabAuxSort ports stock auxsort (5.5.1 ltablib.c): median-of-three
// presort, the pivot parked at a[up-1], recursion into the smaller
// interval with a tail loop over the larger, and a randomized pivot for
// intervals past RANLIMIT once a partition comes out too imbalanced.
// Only the final order is observable between engines (equal-key order
// stays engine-specific — stock's l_randomizePivot is RNG-seeded; the
// fork uses Go's math/rand).
func tabAuxSort(L *LState, t LValue, cmp *LFunction, lo, up int64) {
	rnd := uint32(0) // 0 = deterministic middle pivot, like stock's start
	for lo < up {
		// sort elements a[lo], a[p], a[up]
		alo, aup := tabGetI(L, t, lo), tabGetI(L, t, up)
		if sortComp(L, cmp, aup, alo) { // a[up] < a[lo]?
			tabSetI(L, t, lo, aup)
			tabSetI(L, t, up, alo)
		}
		if up-lo == 1 { // only 2 elements
			return
		}
		var p int64
		if up-lo < 100 || rnd == 0 { // small interval or no randomize
			p = (lo + up) / 2
		} else { // random pivot (choosePivot)
			r4 := (up - lo) / 4
			p = lo + r4 + int64(rnd%uint32(2*r4))
		}
		ap, alo2 := tabGetI(L, t, p), tabGetI(L, t, lo)
		if sortComp(L, cmp, ap, alo2) { // a[p] < a[lo]?
			tabSetI(L, t, p, alo2)
			tabSetI(L, t, lo, ap)
		} else {
			aup2 := tabGetI(L, t, up)
			if sortComp(L, cmp, aup2, ap) { // a[up] < a[p]?
				tabSetI(L, t, p, aup2)
				tabSetI(L, t, up, ap)
			}
		}
		if up-lo == 2 { // only 3 elements
			return
		}
		// park the pivot at a[up-1]
		pivot := tabGetI(L, t, p)
		tabSetI(L, t, p, tabGetI(L, t, up-1))
		tabSetI(L, t, up-1, pivot)
		p = tabPartition(L, t, cmp, lo, up, pivot)
		var nsmall int64 // size of the recursed (smaller) interval
		if p-lo < up-p {
			tabAuxSort(L, t, cmp, lo, p-1)
			nsmall = p - lo
			lo = p + 1
		} else {
			tabAuxSort(L, t, cmp, p+1, up)
			nsmall = up - p
			up = p - 1
		}
		if (up-lo)/128 > nsmall { // partition too imbalanced
			rnd = rand.Uint32()
		}
	}
}

// tableSort ports stock sort: the checks (array-too-big, comparator
// type) run only for a non-trivial interval; n <= 1 is a no-op that
// accepts any second argument.
func tableSort(L *LState) int {
	t := L.Get(1)
	n := tabAuxGetN(L, 1, tabRead|tabWrite)
	if n > 1 {
		if n >= 1<<31-1 {
			L.ArgError(1, "array too big")
		}
		var cmp *LFunction
		if L.GetTop() >= 2 {
			if v := L.Get(2); v != LNil {
				fn, ok := v.(*LFunction)
				if !ok {
					L.TypeError(2, LTFunction)
				}
				cmp = fn
			}
		}
		tabAuxSort(L, t, cmp, 1, n)
	}
	return 0
}
