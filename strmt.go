package lua

/* string-metatable arithmetic (U3c, closes row 58) — the 5.4/5.5
 * coercion model ported from lstrlib.c's stringmetamethods block.
 *
 * 5.5.1's string metatable carries ONLY __add __sub __mul __mod __pow
 * __div __idiv __unm (plus __index = the string library): bitwise
 * metamethods do not exist, so "3" & 5 is a plain type error. Coercion
 * (tonum) is subtype-preserving and whole-string (luaO_str2num); on
 * failure trymt gives the SECOND operand's own metamethod a chance,
 * else raises the op-named error ("attempt to add a 'string' with a
 * 'number'"). The arithmetic itself runs over the existing subtype
 * correct arithCompute (the luaO_arith equivalent). */

// strArithTonum ports tonum: numbers pass verbatim (subtype preserved);
// strings must convert consuming the whole text.
func strArithTonum(L *LState, arg int) (LValue, bool) {
	v := L.Get(arg)
	switch v.(type) {
	case LInteger, LNumber:
		return v, true
	}
	if str, ok := v.(LString); ok {
		if num, err := parseNumber(string(str)); err == nil {
			return num, true
		}
	}
	return nil, false
}

// strTryMt ports trymt: on failed coercion, a non-string second operand
// with its own metamethod for the op gets the call; otherwise the
// op-named error naming both operand types.
func strTryMt(L *LState, mtkey, opname string) {
	lhs, rhs := L.Get(1), L.Get(2)
	if _, isStr := rhs.(LString); !isStr {
		if mm := L.metaOp1(rhs, mtkey); mm.Type() == LTFunction {
			L.Push(mm)
			L.Push(lhs)
			L.Push(rhs)
			L.Call(2, 1)
			return
		}
	}
	L.RaiseError("attempt to %s a '%s' with a '%s'", opname, lhs.Type().String(), rhs.Type().String())
}

// strArith ports arith(L, op, mtname): coerce both operands and compute,
// else trymt.
func strArith(L *LState, opcode int, mtname string) {
	opname := mtname[2:]
	if lv, ok1 := strArithTonum(L, 1); ok1 {
		if rv, ok2 := strArithTonum(L, 2); ok2 {
			if v, errText, res := arithCompute(opcode, lv, rv); res == arithDone {
				L.Push(v)
				return
			} else if res == arithRaise {
				L.RaiseError("%s", errText)
			}
		}
	}
	strTryMt(L, mtname, opname)
}

func strmtAdd(L *LState) int  { strArith(L, OP_ADD, "__add"); return 1 }
func strmtSub(L *LState) int  { strArith(L, OP_SUB, "__sub"); return 1 }
func strmtMul(L *LState) int  { strArith(L, OP_MUL, "__mul"); return 1 }
func strmtMod(L *LState) int  { strArith(L, OP_MOD, "__mod"); return 1 }
func strmtPow(L *LState) int  { strArith(L, OP_POW, "__pow"); return 1 }
func strmtDiv(L *LState) int  { strArith(L, OP_DIV, "__div"); return 1 }
func strmtIdiv(L *LState) int { strArith(L, OP_IDIV, "__idiv"); return 1 }

func strmtUnm(L *LState) int {
	// stock lua_arith duplicates the operand for unary ops, so tonum
	// sees it in both argument slots
	L.Push(L.Get(1))
	strArith(L, OP_UNM, "__unm")
	return 1
}

// createStringMetatable ports createmetatable: a table SEPARATE from
// the library carrying the arith metamethods and __index = the library
// (the old fork shape — the module table itself as mt — made
// getmetatable("") the library and user overrides pollute it).
func createStringMetatable(L *LState, lib *LTable) *LTable {
	mt := L.NewTable()
	mt.RawSetString("__add", L.NewFunction(strmtAdd))
	mt.RawSetString("__sub", L.NewFunction(strmtSub))
	mt.RawSetString("__mul", L.NewFunction(strmtMul))
	mt.RawSetString("__mod", L.NewFunction(strmtMod))
	mt.RawSetString("__pow", L.NewFunction(strmtPow))
	mt.RawSetString("__div", L.NewFunction(strmtDiv))
	mt.RawSetString("__idiv", L.NewFunction(strmtIdiv))
	mt.RawSetString("__unm", L.NewFunction(strmtUnm))
	mt.RawSetString("__index", lib)
	return mt
}

//
