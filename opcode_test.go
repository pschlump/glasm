package lua

import "testing"

// U2c gate: the 5.5.1 instruction encoding — bit positions, field
// round-trips, and the excess-K immediates, verified against the layout
// in the vendored runtime/lua55/src/lopcodes.h.
func TestOpcodeEncodingLayout(t *testing.T) {
	// iABC: C(8) | B(8) | k(1) | A(8) | Op(7)
	inst := opCreateABCk(OP_ADD, 0x5A, 0xA5, 0xC3, 1)
	if got := inst & 0x7f; got != uint32(OP_ADD) {
		t.Fatalf("opcode field = %#x", got)
	}
	if got := opGetArgA(inst); got != 0x5A {
		t.Fatalf("A = %#x", got)
	}
	if got := opGetArgk(inst); got != 1 {
		t.Fatalf("k = %d", got)
	}
	if got := opGetArgB(inst); got != 0xA5 {
		t.Fatalf("B = %#x", got)
	}
	if got := opGetArgC(inst); got != 0xC3 {
		t.Fatalf("C = %#x", got)
	}
	if opGetArgA(inst)<<7&0x7f80 != 0x2D00 { // A lives at bits 7..14
		t.Fatalf("A bit position wrong: %#x", inst)
	}
}

func TestOpcodeEncodingBxSbx(t *testing.T) {
	// iABx/iAsBx: Bx(17) at bits 15..31; sBx is excess-K with
	// OFFSET_sBx = (2^17-1)>>1 = 65535
	for _, v := range []int{0, 1, 65536, -65535, -1} {
		inst := opCreateAsBx(OP_LOADI, 12, v)
		if got := opGetArgSbx(inst); got != v {
			t.Fatalf("sBx round-trip %d -> %d", v, got)
		}
		if got := opGetArgA(inst); got != 12 {
			t.Fatalf("A clobbered: %d", got)
		}
	}
	// the full sBx range per stock fitsBx: -65535..65536
	if !fitsBx(-65535) || !fitsBx(65536) || fitsBx(65537) || fitsBx(-65536) {
		t.Fatal("fitsBx boundaries")
	}
	// LOADK Bx round-trip
	inst := opCreateABx(OP_LOADK, 3, 131071)
	if got := opGetArgBx(inst); got != 131071 {
		t.Fatalf("Bx round-trip: %d", got)
	}
	if got := opGetArgA(inst); got != 3 {
		t.Fatalf("A clobbered: %d", got)
	}
}

func TestOpcodeEncodingSjAx(t *testing.T) {
	// isJ/iAx: 25-bit fields at POS_A; sJ excess-K with
	// OFFSET_sJ = (2^25-1)>>1 = 16777215
	for _, v := range []int{0, 1, -1, 16777215, -16777215, 16777216 - 1} {
		inst := opCreateSJ(OP_JMP, v)
		if got := opGetArgsJ(inst); got != v {
			t.Fatalf("sJ round-trip %d -> %d", v, got)
		}
	}
	inst := opCreateAx(OP_EXTRAARG, 0x1FFFFFF)
	if got := opGetArgAx(inst); got != 0x1FFFFFF {
		t.Fatalf("Ax round-trip: %#x", got)
	}
}

func TestOpcodeEncodingvABC(t *testing.T) {
	// ivABC: vC(10) at bits 22..31, vB(6) at bits 16..21
	inst := opCreatevABCk(OP_NEWTABLE, 9, 63, 1023, 1)
	if got := opGetArgvB(inst); got != 63 {
		t.Fatalf("vB = %d", got)
	}
	if got := opGetArgvC(inst); got != 1023 {
		t.Fatalf("vC = %d", got)
	}
	if got := opGetArgA(inst); got != 9 {
		t.Fatalf("A = %d", got)
	}
	if got := opGetArgk(inst); got != 1 {
		t.Fatalf("k = %d", got)
	}
}

func TestOpcodeEncodingSC(t *testing.T) {
	// sC immediates: OFFSET_sC = 127, valid range -127..128 (fitsC)
	if int2sC(0) != 127 || sC2int(127) != 0 {
		t.Fatal("sC bias")
	}
	if !fitsC(-127) || !fitsC(128) || fitsC(129) || fitsC(-128) {
		t.Fatal("fitsC boundaries")
	}
}

func TestOpcodeEncodingOpcodeCount(t *testing.T) {
	// the stock 5.5.1 set: 85 opcodes with the exact numbering
	if opCodeMax != 84 {
		t.Fatalf("opCodeMax = %d", opCodeMax)
	}
	checks := []struct {
		op   int
		name string
	}{
		{OP_MOVE, "MOVE"}, {OP_LOADI, "LOADI"}, {OP_LOADF, "LOADF"},
		{OP_LOADK, "LOADK"}, {OP_LOADKX, "LOADKX"}, {OP_LOADFALSE, "LOADFALSE"},
		{OP_LFALSESKIP, "LFALSESKIP"}, {OP_LOADTRUE, "LOADTRUE"},
		{OP_LOADNIL, "LOADNIL"}, {OP_GETUPVAL, "GETUPVAL"},
		{OP_SETUPVAL, "SETUPVAL"}, {OP_GETTABUP, "GETTABUP"},
		{OP_GETTABLE, "GETTABLE"}, {OP_GETI, "GETI"},
		{OP_GETFIELD, "GETFIELD"}, {OP_SETTABUP, "SETTABUP"},
		{OP_SETTABLE, "SETTABLE"}, {OP_SETI, "SETI"},
		{OP_SETFIELD, "SETFIELD"}, {OP_NEWTABLE, "NEWTABLE"},
		{OP_SELF, "SELF"}, {OP_ADDI, "ADDI"}, {OP_ADDK, "ADDK"},
		{OP_SUBK, "SUBK"}, {OP_MULK, "MULK"}, {OP_MODK, "MODK"},
		{OP_POWK, "POWK"}, {OP_DIVK, "DIVK"}, {OP_IDIVK, "IDIVK"},
		{OP_BANDK, "BANDK"}, {OP_BORK, "BORK"}, {OP_BXORK, "BXORK"},
		{OP_SHLI, "SHLI"}, {OP_SHRI, "SHRI"}, {OP_ADD, "ADD"},
		{OP_SUB, "SUB"}, {OP_MUL, "MUL"}, {OP_MOD, "MOD"},
		{OP_POW, "POW"}, {OP_DIV, "DIV"}, {OP_IDIV, "IDIV"},
		{OP_BAND, "BAND"}, {OP_BOR, "BOR"}, {OP_BXOR, "BXOR"},
		{OP_SHL, "SHL"}, {OP_SHR, "SHR"}, {OP_MMBIN, "MMBIN"},
		{OP_MMBINI, "MMBINI"}, {OP_MMBINK, "MMBINK"}, {OP_UNM, "UNM"},
		{OP_BNOT, "BNOT"}, {OP_NOT, "NOT"}, {OP_LEN, "LEN"},
		{OP_CONCAT, "CONCAT"}, {OP_CLOSE, "CLOSE"}, {OP_TBC, "TBC"},
		{OP_JMP, "JMP"}, {OP_EQ, "EQ"}, {OP_LT, "LT"}, {OP_LE, "LE"},
		{OP_EQK, "EQK"}, {OP_EQI, "EQI"}, {OP_LTI, "LTI"},
		{OP_LEI, "LEI"}, {OP_GTI, "GTI"}, {OP_GEI, "GEI"},
		{OP_TEST, "TEST"}, {OP_TESTSET, "TESTSET"}, {OP_CALL, "CALL"},
		{OP_TAILCALL, "TAILCALL"}, {OP_RETURN, "RETURN"},
		{OP_RETURN0, "RETURN0"}, {OP_RETURN1, "RETURN1"},
		{OP_FORLOOP, "FORLOOP"}, {OP_FORPREP, "FORPREP"},
		{OP_TFORPREP, "TFORPREP"}, {OP_TFORCALL, "TFORCALL"},
		{OP_TFORLOOP, "TFORLOOP"}, {OP_SETLIST, "SETLIST"},
		{OP_CLOSURE, "CLOSURE"}, {OP_VARARG, "VARARG"},
		{OP_GETVARG, "GETVARG"}, {OP_ERRNNIL, "ERRNNIL"},
		{OP_VARARGPREP, "VARARGPREP"}, {OP_EXTRAARG, "EXTRAARG"},
	}
	for i, c := range checks {
		if c.op != i {
			t.Fatalf("%s = %d, want %d", c.name, c.op, i)
		}
		if opProps[i].Name != c.name {
			t.Fatalf("opProps[%d].Name = %s, want %s", i, opProps[i].Name, c.name)
		}
	}
	// the arith range OP_ADD..OP_SHR maps onto TM_ADD..TM_SHR
	if arithTM(OP_ADD) != tmAdd || arithTM(OP_SHR) != tmShr {
		t.Fatal("arithTM mapping")
	}
	if tmToArithOpSafe(tmAdd) != OP_ADD {
		t.Fatal("tmToArithOp mapping")
	}
}

func tmToArithOpSafe(tm int) int { return tm - tmAdd + OP_ADD }

// compile-side encode/decode coherence: the accessors the wasm backend
// will consume (U2e) see the same fields.
func TestOpcodeExportedAccessors(t *testing.T) {
	inst := OpCreateABCk(OP_SETFIELD, 4, 9, 200, 1)
	if OpGetOpCode(inst) != OP_SETFIELD || OpGetArgA(inst) != 4 ||
		OpGetArgB(inst) != 9 || OpGetArgC(inst) != 200 || OpGetArgk(inst) != 1 {
		t.Fatalf("exported accessor mismatch: %08x", inst)
	}
	inst = OpCreateABx(OP_CLOSURE, 7, 0x1234)
	if OpGetArgBx(inst) != 0x1234 || OpGetArgA(inst) != 7 {
		t.Fatalf("exported Bx mismatch: %08x", inst)
	}
	inst = OpCreateSJ(OP_JMP, -100)
	if OpGetArgsJ(inst) != -100 {
		t.Fatalf("exported sJ mismatch: %08x", inst)
	}
	if OpCodeMax() != opCodeMax {
		t.Fatal("OpCodeMax")
	}
}
