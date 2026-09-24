package lua

import (
	"sort"
)

func OpenTable(L *LState) int {
	tabmod := L.RegisterModule(TabLibName, tableFuncs)
	L.Push(tabmod)
	return 1
}

var tableFuncs = map[string]LGFunction{
	"getn": tableGetN, // deprecated in 5.2 (removed in 5.3 — U3)
	"maxn": tableMaxN, // deprecated in 5.2 (removed in 5.3 — U3)
	// 5.2 additions (U1)
	"pack":   tablePack,
	"unpack": tableUnpack,
	"concat": tableConcat,
	"insert": tableInsert,
	"remove": tableRemove,
	"sort":   tableSort,
}

// tablePack implements 5.2 table.pack(...): {n = select('#', ...), ...}.
// The count is stored explicitly so trailing nils survive.
func tablePack(L *LState) int {
	n := L.GetTop()
	t := L.NewTable()
	for i := 1; i <= n; i++ {
		t.RawSetInt(i, L.Get(i))
	}
	t.RawSetString("n", LNumber(n))
	L.Push(t)
	return 1
}

// tableUnpack implements 5.2 table.unpack(list [, i [, j]]) — the 5.2
// replacement for base unpack (returns list[i..j], i default 1, j
// default #list).
func tableUnpack(L *LState) int {
	tb := L.CheckTable(1)
	i := L.OptInt(2, 1)
	var end int
	if L.GetTop() >= 3 {
		end = L.CheckInt(3)
	} else {
		end = tb.Len()
	}
	n := end - i + 1
	if n <= 0 {
		return 0
	}
	for k := i; k <= end; k++ {
		L.Push(tb.RawGetInt(k))
	}
	return n
}

func tableSort(L *LState) int {
	tbl := L.CheckTable(1)
	sorter := lValueArraySorter{L, nil, tbl.array}
	if L.GetTop() != 1 {
		sorter.Fn = L.CheckFunction(2)
	}
	sort.Sort(sorter)
	return 0
}

func tableGetN(L *LState) int {
	L.Push(LNumber(L.CheckTable(1).Len()))
	return 1
}

func tableMaxN(L *LState) int {
	L.Push(LNumber(L.CheckTable(1).MaxN()))
	return 1
}

func tableRemove(L *LState) int {
	tbl := L.CheckTable(1)
	if L.GetTop() == 1 {
		L.Push(tbl.Remove(-1))
	} else {
		L.Push(tbl.Remove(L.CheckInt(2)))
	}
	return 1
}

func tableConcat(L *LState) int {
	tbl := L.CheckTable(1)
	sep := LString(L.OptString(2, ""))
	i := L.OptInt(3, 1)
	j := L.OptInt(4, tbl.Len())
	if L.GetTop() == 3 {
		if i > tbl.Len() || i < 1 {
			L.Push(emptyLString)
			return 1
		}
	}
	i = intMax(intMin(i, tbl.Len()), 1)
	j = intMin(intMin(j, tbl.Len()), tbl.Len())
	if i > j {
		L.Push(emptyLString)
		return 1
	}
	//TODO should flushing?
	retbottom := L.GetTop()
	for ; i <= j; i++ {
		v := tbl.RawGetInt(i)
		if !LVCanConvToString(v) {
			L.RaiseError("invalid value (%s) at index %d in table for concat", v.Type().String(), i)
		}
		L.Push(v)
		if i != j {
			L.Push(sep)
		}
	}
	L.Push(stringConcat(L, L.GetTop()-retbottom, L.reg.Top()-1))
	return 1
}

func tableInsert(L *LState) int {
	tbl := L.CheckTable(1)
	nargs := L.GetTop()
	if nargs == 1 {
		L.RaiseError("wrong number of arguments")
	}

	if L.GetTop() == 2 {
		tbl.Append(L.Get(2))
		return 0
	}
	tbl.Insert(int(L.CheckInt(2)), L.CheckAny(3))
	return 0
}

//
