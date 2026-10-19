package testdiff

// Canonical value formatting. These rules are part of the cross-engine
// contract (design doc §7): two engines may only produce equal logs if
// they agree on these normal forms. Any change here invalidates every
// recorded golden and must be deliberate.

import (
	"fmt"
	"math"
	"sort"
	"strconv"

	"github.com/pschlump/glasm"
)

const (
	maxDepth   = 8   // table serialization depth cap
	maxEntries = 512 // entries per serialized table
)

// NumRepr formats a Lua number canonically: %.14g with C-style inf/nan.
func NumRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	return strconv.FormatFloat(f, 'g', 14, 64)
}

// PrintArg normalizes one print() argument using Lua print semantics:
// strings print raw, other values use their tostring form, tables never
// leak addresses. (Blindness of print-diffing: "1" and 1 print the same;
// corpus asserts cover those distinctions.)
func PrintArg(v lua.LValue) string {
	switch x := v.(type) {
	case lua.LString:
		return string(x)
	case lua.LNumber:
		return NumRepr(float64(x))
	case lua.LInteger:
		// U6e: integers render lossless (the wire gained PT_INTEGER with
		// it); equals NumRepr below 1e15, correct above — where the f64
		// detour printed 14-sig-fig 'g' forms
		return strconv.FormatInt(int64(x), 10)
	case *lua.LNilType:
		return "nil"
	case lua.LBool:
		if bool(x) {
			return "true"
		}
		return "false"
	case *lua.LTable:
		return "<table>"
	case *lua.LFunction:
		return "<function>"
	case *lua.LUserData:
		return "<userdata>"
	case *lua.LState:
		return "<thread>"
	case lua.LChannel:
		return "<channel>"
	}
	return fmt.Sprintf("<%s>", v.Type().String())
}

// ValueRepr is the full serialization form used for globals and error
// values: strings quoted, tables recursively serialized with sorted keys.
func ValueRepr(v lua.LValue, depth int, seen map[*lua.LTable]bool) string {
	switch x := v.(type) {
	case lua.LString:
		return strconv.Quote(string(x))
	case lua.LNumber:
		return NumRepr(float64(x))
	case lua.LInteger:
		return strconv.FormatInt(int64(x), 10) // U6e: lossless (see PrintArg)
	case *lua.LNilType:
		return "nil"
	case lua.LBool:
		if bool(x) {
			return "true"
		}
		return "false"
	case *lua.LTable:
		return serializeTable(x, depth, seen)
	case *lua.LFunction:
		return "<function>"
	case *lua.LUserData:
		return "<userdata>"
	case *lua.LState:
		return "<thread>"
	case lua.LChannel:
		return "<channel>"
	}
	return fmt.Sprintf("<%s>", v.Type().String())
}

// serializeTable renders a table deterministically:
//   - entries sorted by normalized key text (byte order)
//   - cycles rendered as <cycle> (DFS marking), depth capped at maxDepth
//   - at most maxEntries entries, then a plain <...> marker (no counts —
//     approximate entry counting would differ across engines)
func serializeTable(t *lua.LTable, depth int, seen map[*lua.LTable]bool) string {
	if seen[t] {
		return "<cycle>"
	}
	if depth >= maxDepth {
		return "<deep>"
	}
	seen[t] = true
	defer delete(seen, t)

	type kv struct{ k, v string }
	var entries []kv
	truncated := false
	t.ForEach(func(k, val lua.LValue) {
		if len(entries) >= maxEntries {
			truncated = true
			return
		}
		entries = append(entries, kv{ValueRepr(k, depth+1, seen), ValueRepr(val, depth+1, seen)})
	})
	sort.Slice(entries, func(i, j int) bool { return entries[i].k < entries[j].k })

	out := "{"
	for i, e := range entries {
		if i > 0 {
			out += ", "
		}
		out += e.k + "=" + e.v
	}
	if truncated {
		if len(entries) > 0 {
			out += ", "
		}
		out += "<...>"
	}
	return out + "}"
}
