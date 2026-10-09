package luawasm

// Opcode emitters for the trickier lowers: arithmetic (inline
// int/float fast paths + the MMBIN* fallback slots over rt_arith),
// comparisons, calls, returns, the count-based numeric for loop,
// TFORCALL and SETLIST (design doc §4.4). Every fast path's miss lands
// in the same MMBIN* fallback the interpreter executes (_vm.go
// opMMBIN*: p1/p2 with the flip), so metamethods, coercions and error
// texts are the runtime's stock 5.5 ones.

import (
	lua "github.com/pschlump/golua55"
	"github.com/pschlump/golua55/wasm"
)

// tagIs: leaves 1/0 — register k's tag byte equals tag. The load is
// BYTE-wide: the runtime's TValue stores write tt_ as a lu_byte (one
// byte), so the tag word's upper three bytes are stale padding whenever
// a cell's last writer was C-side (rt_eq/rt_len/stack copies) — a 4-byte
// load would compare garbage (life1's false EQKs, row 61(d)). The
// backend's own tag stores stay full-width (row 45); they zero the
// padding, which C never reads.
func (fe *funcEmitter) tagIs(reg int, tag int) *wasm.Function {
	return fe.cellAddr(reg).I32Load8U(8).I32Const(int32(tag)).I32Eq()
}

// mmbinFallback: lower the MMBIN/MMBINI/MMBINK slot at pc+1 exactly as
// _vm.go does — p1/p2 from the slot's operands (the immediate form
// materializes into static scratch 1, flipped when k says the constant
// or immediate was the first source operand), rt_arith over the TM
// number, result into the fast op's destination.
func (fe *funcEmitter) mmbinFallback(dest, pc int) {
	f := fe.f
	mm := fe.code[pc+1]
	mop := lua.OpGetOpCode(mm)
	if !isMMBIN(mop) {
		panic("luawasm: arith op without its MMBIN* slot")
	}
	ma := lua.OpGetArgA(mm)
	mb := lua.OpGetArgB(mm)
	mc := lua.OpGetArgC(mm)
	mk := lua.OpGetArgk(mm)
	var p1, p2 func() *wasm.Function
	switch mop {
	case lua.OP_MMBIN:
		p1 = func() *wasm.Function { return fe.cellAddr(ma) }
		p2 = func() *wasm.Function { return fe.cellAddr(mb) }
	case lua.OP_MMBINI:
		fe.mkIntScratch(1, int64(scInt(mb)))
		if mk != 0 { // flipped: the immediate was the first operand
			p1 = fe.scratchAddrCell(1)
			p2 = func() *wasm.Function { return fe.cellAddr(ma) }
		} else {
			p1 = func() *wasm.Function { return fe.cellAddr(ma) }
			p2 = fe.scratchAddrCell(1)
		}
	default: // OP_MMBINK
		if mk != 0 { // flipped: the constant was the first operand
			p1 = func() *wasm.Function { return fe.kcell(mb) }
			p2 = func() *wasm.Function { return fe.cellAddr(ma) }
		} else {
			p1 = func() *wasm.Function { return fe.cellAddr(ma) }
			p2 = func() *wasm.Function { return fe.kcell(mb) }
		}
	}
	f.I32Const(rtOpOfTM(mc))
	p1()
	p2()
	fe.cellAddr(dest)
	f.I32Const(fe.line(pc))
	// row-71 hints: h1/h2 name p1/p2's registers (the immediate/constant
	// operand carries none — the interp's r2 = -1 for the I/K forms, with
	// the k flip swapping which side the register is); hcall is the MMBIN
	// slot's funcnamefromcode answer ("metamethod '<event>'") for a
	// non-callable metamethod value
	f.I32Const(int32(fe.hintArith(mop, ma, mb, mk, pc+1, true)))
	f.I32Const(int32(fe.hintArith(mop, ma, mb, mk, pc+1, false)))
	f.I32Const(int32(fe.hintCallSite(pc + 1)))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(dest + 1)
}

// hintArith: one MMBIN* fallback operand's hint — first=true asks for
// p1. p1 is the register for MMBIN and the unflipped I/K forms, the
// immediate/constant when the k flag flipped the operand order
// (mirroring the interp's r1/r2 in opMMBIN*).
func (fe *funcEmitter) hintArith(mop, ma, mb, mk, lastpc int, first bool) int32 {
	reg, other := ma, mb // OP_MMBIN: (R[A], R[B])
	if mop != lua.OP_MMBIN {
		other = -1 // the imm/const operand carries no register
		if mk != 0 {
			reg, other = other, reg // flipped: the imm/const is p1
		}
	}
	if !first {
		reg = other
	}
	return fe.hintSite(lastpc, reg)
}

// scratchAddrCell: scratchAddr as a value-producer (for operand slots)
func (fe *funcEmitter) scratchAddrCell(k int) func() *wasm.Function {
	return func() *wasm.Function { return fe.scratchAddr(k) }
}

// i64Op: the raw i64 arithmetic/bitwise op (ADD/SUB/MUL/BAND/BOR/BXOR)
func (fe *funcEmitter) i64Op(op int) {
	f := fe.f
	switch op {
	case lua.OP_ADD:
		f.I64Add()
	case lua.OP_SUB:
		f.I64Sub()
	case lua.OP_MUL:
		f.I64Mul()
	case lua.OP_BAND:
		f.I64And()
	case lua.OP_BOR:
		f.I64Or()
	case lua.OP_BXOR:
		f.I64Xor()
	}
}

// emitArithReg: R[A] := R[B] <op> R[C] (OP_ADD..OP_SHR). Fast paths:
// int×int for the wrap-around arithmetic and bitwise family (the two
// i64 operands make int semantics exact), float×float for + − *;
// everything else — mixed int/float (promotion), // % / ^ (float or
// error semantics), string coercion, metamethods — falls to the MMBIN
// slot's rt_arith.
func (fe *funcEmitter) emitArithReg(op, A, B, C, pc int) {
	f := fe.f
	intFast := false
	fltFast := false
	switch op {
	case lua.OP_ADD, lua.OP_SUB, lua.OP_MUL, lua.OP_BAND, lua.OP_BOR, lua.OP_BXOR, lua.OP_SHL, lua.OP_SHR:
		intFast = true
	}
	switch op {
	case lua.OP_ADD, lua.OP_SUB, lua.OP_MUL:
		fltFast = true
	}
	switch {
	case op == lua.OP_SHL || op == lua.OP_SHR:
		// shift: both operands int cells, then luaV_shiftl semantics
		fe.tagIs(B, rtTagInt)
		fe.tagIs(C, rtTagInt)
		f.I32And().If(wasm.Void)
		fe.cellAddr(B).I64Load(0).LocalSet(fe.lVlo)
		fe.cellAddr(C).I64Load(0).LocalSet(fe.lVhi)
		if op == lua.OP_SHR {
			f.I64Const(0).LocalGet(fe.lVhi).I64Sub().LocalSet(fe.lVhi) // SHR = shiftl(x, -y)
		}
		fe.emitShiftCompute()
		fe.setIntCell(A, func() { fe.f.LocalGet(fe.lV2) })
		fe.bumpTop(A + 1)
		f.Else()
		fe.mmbinFallback(A, pc)
		f.End()
	case intFast:
		fe.tagIs(B, rtTagInt)
		fe.tagIs(C, rtTagInt)
		f.I32And().If(wasm.Void)
		fe.cellAddr(A)
		fe.cellAddr(B).I64Load(0)
		fe.cellAddr(C).I64Load(0)
		fe.i64Op(op)
		f.I64Store(0)
		fe.cellAddr(A)
		f.I32Const(rtTagInt).I32Store(8) // full 4-byte tag (row 45)
		fe.bumpTop(A + 1)
		f.Else()
		fe.mmbinFallback(A, pc)
		f.End()
	case fltFast:
		fe.tagIs(B, rtTagFloat)
		fe.tagIs(C, rtTagFloat)
		f.I32And().If(wasm.Void)
		fe.cellAddr(A)
		fe.cellAddr(B).F64Load(0)
		fe.cellAddr(C).F64Load(0)
		switch op {
		case lua.OP_ADD:
			f.F64Add()
		case lua.OP_SUB:
			f.F64Sub()
		case lua.OP_MUL:
			f.F64Mul()
		}
		f.F64Store(0)
		fe.cellAddr(A)
		f.I32Const(rtTagFloat).I32Store(8)
		fe.bumpTop(A + 1)
		f.Else()
		fe.mmbinFallback(A, pc)
		f.End()
	default:
		// MOD/POW/DIV/IDIV: always the runtime (floor semantics, the
		// 'attempt to divide by zero' family, float promotion)
		fe.mmbinFallback(A, pc)
	}
}

// emitShiftCompute: luaV_shiftl with x in lVlo and y in lVhi → lV2.
// y < 0 shifts the other way; |y| >= 64 ⇒ 0; right shifts are LOGICAL
// (intop's unsigned cast: -1 >> 1 == maxint).
func (fe *funcEmitter) emitShiftCompute() {
	f := fe.f
	f.I64Const(0).LocalSet(fe.lV2)
	f.LocalGet(fe.lVhi).I64Const(0).I64LtS().If(wasm.Void)
	// y < 0: logical right by -y
	f.I64Const(0).LocalGet(fe.lVhi).I64Sub().LocalSet(fe.lVhi)
	f.LocalGet(fe.lVhi).I64Const(64).I64GeS().If(wasm.Void)
	f.I64Const(0).LocalSet(fe.lV2) // -y >= 64
	f.Else()
	f.LocalGet(fe.lVlo).LocalGet(fe.lVhi).I64ShrU().LocalSet(fe.lV2)
	f.End()
	f.Else()
	// y >= 0: left by y
	f.LocalGet(fe.lVhi).I64Const(64).I64GeS().If(wasm.Void)
	f.I64Const(0).LocalSet(fe.lV2) // y >= 64
	f.Else()
	f.LocalGet(fe.lVlo).LocalGet(fe.lVhi).I64Shl().LocalSet(fe.lV2)
	f.End()
	f.End()
}

// emitArithI: OP_ADDI — R[A] := R[B] + sC (int wrap / float promote).
func (fe *funcEmitter) emitArithI(A, B, C, pc int) {
	f := fe.f
	imm := int64(scInt(C))
	fe.tagIs(B, rtTagInt).If(wasm.Void)
	fe.cellAddr(A)
	fe.cellAddr(B).I64Load(0)
	f.I64Const(imm)
	f.I64Add().I64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtTagInt).I32Store(8)
	fe.bumpTop(A + 1)
	f.Else()
	fe.tagIs(B, rtTagFloat).If(wasm.Void)
	fe.cellAddr(A)
	fe.cellAddr(B).F64Load(0)
	f.F64Const(float64(imm))
	f.F64Add().F64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtTagFloat).I32Store(8)
	fe.bumpTop(A + 1)
	f.Else()
	fe.mmbinFallback(A, pc)
	f.End()
	f.End()
}

// emitArithK: the ADDK..BXORK family — R[A] := R[B] op K[C]. The K
// cell's type is known at compile time (the pool was staged here), so
// int constants join the int fast path and float constants the float
// one; every miss falls to the MMBINK slot.
func (fe *funcEmitter) emitArithK(op, A, B, C, pc int) {
	f := fe.f
	kv := fe.proto.Constants[C]
	switch kv.(type) {
	case lua.LInteger:
		switch op {
		case lua.OP_ADDK, lua.OP_SUBK, lua.OP_MULK, lua.OP_BANDK, lua.OP_BORK, lua.OP_BXORK:
			fe.tagIs(B, rtTagInt).If(wasm.Void)
			fe.cellAddr(A)
			fe.cellAddr(B).I64Load(0)
			fe.kcell(C).I64Load(0)
			fe.i64Op(op - lua.OP_ADDK + lua.OP_ADD)
			f.I64Store(0)
			fe.cellAddr(A)
			f.I32Const(rtTagInt).I32Store(8)
			fe.bumpTop(A + 1)
			f.Else()
			fe.mmbinFallback(A, pc)
			f.End()
			return
		}
	case lua.LNumber:
		switch op {
		case lua.OP_ADDK, lua.OP_SUBK, lua.OP_MULK:
			fe.tagIs(B, rtTagFloat).If(wasm.Void)
			fe.cellAddr(A)
			fe.cellAddr(B).F64Load(0)
			fe.kcell(C).F64Load(0)
			switch op {
			case lua.OP_ADDK:
				f.F64Add()
			case lua.OP_SUBK:
				f.F64Sub()
			case lua.OP_MULK:
				f.F64Mul()
			}
			f.F64Store(0)
			fe.cellAddr(A)
			f.I32Const(rtTagFloat).I32Store(8)
			fe.bumpTop(A + 1)
			f.Else()
			fe.mmbinFallback(A, pc)
			f.End()
			return
		}
	}
	// MODK/POWK/DIVK/IDIVK (and defensive K-type surprises): runtime
	fe.mmbinFallback(A, pc)
}

// emitShiftI: SHLI — R[A] := sC << R[B]; SHRI — R[A] := R[B] >> sC.
// The immediate is sC (|imm| <= 127): SHRI's effective shift count is a
// compile-time constant (direction/bound decided here), SHLI's register
// operand is dynamic (the generic compute).
func (fe *funcEmitter) emitShiftI(op, A, B, C, pc int) {
	f := fe.f
	imm := scInt(C)
	fe.tagIs(B, rtTagInt).If(wasm.Void)
	switch op {
	case lua.OP_SHRI:
		// shiftInt(R[B], -imm): imm > 0 → logical right by imm;
		// imm < 0 → left by -imm; imm == 0 → identity
		switch {
		case imm >= 64 || imm <= -64:
			fe.setIntCell(A, func() { fe.f.I64Const(0) })
		case imm > 0:
			fe.setIntCell(A, func() {
				fe.cellAddr(B).I64Load(0).I64Const(int64(imm)).I64ShrU()
			})
		case imm < 0:
			fe.setIntCell(A, func() {
				fe.cellAddr(B).I64Load(0).I64Const(int64(-imm)).I64Shl()
			})
		default: // imm == 0
			fe.setIntCell(A, func() { fe.cellAddr(B).I64Load(0) })
		}
	case lua.OP_SHLI:
		// shiftInt(imm, R[B]): x is the constant, y the register
		f.I64Const(int64(imm)).LocalSet(fe.lVlo)
		fe.cellAddr(B).I64Load(0).LocalSet(fe.lVhi)
		fe.emitShiftCompute()
		fe.setIntCell(A, func() { fe.f.LocalGet(fe.lV2) })
	}
	fe.bumpTop(A + 1)
	f.Else()
	fe.mmbinFallback(A, pc)
	f.End()
}

// emitUnm: R[A] := -R[B] (int negate / float negate / runtime).
func (fe *funcEmitter) emitUnm(A, B, pc int) {
	f := fe.f
	fe.tagIs(B, rtTagInt).If(wasm.Void)
	fe.cellAddr(A)
	f.I64Const(0)
	fe.cellAddr(B).I64Load(0)
	f.I64Sub().I64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtTagInt).I32Store(8)
	fe.bumpTop(A + 1)
	f.Else()
	fe.tagIs(B, rtTagFloat).If(wasm.Void)
	fe.cellAddr(A)
	fe.cellAddr(B).F64Load(0)
	f.F64Neg().F64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtTagFloat).I32Store(8)
	fe.bumpTop(A + 1)
	f.Else()
	f.I32Const(12) // RT_OP_UNM
	fe.cellAddr(B)
	fe.cellAddr(B)
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	// both operand slots name R[B] (unary: p1 == p2); funcnamefromcode
	// answers "metamethod 'unm'" for a non-callable __unm (row 71)
	f.I32Const(int32(fe.hintSite(pc, B)))
	f.I32Const(int32(fe.hintSite(pc, B)))
	f.I32Const(int32(fe.hintCallSite(pc)))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(A + 1)
	f.End()
	f.End()
}

// emitBnot: R[A] := ~R[B] — int xor -1; integral floats and the "number
// has no integer representation" text are the runtime's.
func (fe *funcEmitter) emitBnot(A, B, pc int) {
	f := fe.f
	fe.tagIs(B, rtTagInt).If(wasm.Void)
	fe.cellAddr(A)
	fe.cellAddr(B).I64Load(0)
	f.I64Const(-1)
	f.I64Xor().I64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtTagInt).I32Store(8)
	fe.bumpTop(A + 1)
	f.Else()
	f.I32Const(13) // RT_OP_BNOT
	fe.cellAddr(B)
	fe.cellAddr(B)
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	f.I32Const(int32(fe.hintSite(pc, B)))
	f.I32Const(int32(fe.hintSite(pc, B)))
	f.I32Const(int32(fe.hintCallSite(pc)))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(A + 1)
	f.End()
}

// emitCompare: the condJump family. The condition goes through rt_eq/
// rt_lt/rt_le into static scratch 0 (mixed types, strings, metamethods
// — the runtime's stock semantics); the immediate forms materialize the
// aux into scratch 1 with the C flag's type (the metamethod sees the
// float/int provenance, _vm.go opOrderI's aux). Jump taken when
// cond == k.
func (fe *funcEmitter) emitCompare(op, A, B, C, k, pc int) {
	f := fe.f
	var fn string
	var a, b func() *wasm.Function
	switch op {
	case lua.OP_EQ:
		fn = "rt_eq"
		a, b = fe.cell(A), fe.cell(B)
	case lua.OP_LT:
		fn = "rt_lt"
		a, b = fe.cell(A), fe.cell(B)
	case lua.OP_LE:
		fn = "rt_le"
		a, b = fe.cell(A), fe.cell(B)
	case lua.OP_EQK:
		// raw equality against a constant (stock luaV_rawequalobj —
		// constants never carry __eq, so rt_eq is exact)
		fn = "rt_eq"
		a, b = fe.cell(A), fe.kcellFn(B)
	case lua.OP_EQI, lua.OP_LTI, lua.OP_LEI, lua.OP_GTI, lua.OP_GEI:
		imm := int64(scInt(B))
		if C != 0 { // float-sourced immediate
			fe.mkFloatScratch(1, float64(imm))
		} else {
			fe.mkIntScratch(1, imm)
		}
		switch op {
		case lua.OP_EQI:
			fn = "rt_eq"
			a, b = fe.cell(A), fe.scratchAddrCell(1)
		case lua.OP_LTI:
			fn = "rt_lt"
			a, b = fe.cell(A), fe.scratchAddrCell(1)
		case lua.OP_LEI:
			fn = "rt_le"
			a, b = fe.cell(A), fe.scratchAddrCell(1)
		case lua.OP_GTI: // (a > imm) <=> imm < a
			fn = "rt_lt"
			a, b = fe.scratchAddrCell(1), fe.cell(A)
		default: // GEI: (a >= imm) <=> imm <= a
			fn = "rt_le"
			a, b = fe.scratchAddrCell(1), fe.cell(A)
		}
	}
	a()
	b()
	fe.scratchAddr(0)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp(fn))
	fe.checkStatus()
	// condJump: taken (to the JMP partner's target) when cond == k,
	// else skip the partner word (pc+2). v4 boolean cells carry the
	// truth in the TAG (VTRUE=17/VFALSE=1 — value_.b reads 0 either way,
	// unlike v3's shared TBOOLEAN with the bit at word0). Byte-wide tag
	// load: rt_eq wrote tt_ as one byte (see tagIs).
	fe.scratchAddr(0)
	f.I32Load8U(8)
	f.I32Const(rtTagTrue).I32Eq()
	f.I32Const(int32(k)).I32Eq().If(wasm.Void)
	fe.setBlk(fe.branchPartner(pc))
	f.Else()
	fe.setBlk(fe.blockOf(pc + 2))
	f.End()
}

// cell / kcellFn: address emitters as closures for operand slots
func (fe *funcEmitter) cell(r int) func() *wasm.Function {
	return func() *wasm.Function { return fe.cellAddr(r) }
}

func (fe *funcEmitter) kcellFn(i int) func() *wasm.Function {
	return func() *wasm.Function { return fe.kcell(i) }
}

// emitCall: OP_CALL (tail=false) and OP_TAILCALL (tail=true; C is 0).
// The callee executes through rt_call_addr — the runtime's lvm runs Lua
// closures (re-entering compiled code through the CClosure seam) and C
// functions alike.
//
// U6a (row 86(A)): rt_call_addr(funcaddr, argcells, nargs, want, line)
// reads the callee's ADDRESS in place and the args at their natural
// R(A+1).. position, moving results down to R(A) itself. The old
// save-to-scratch + shift-args-down machinery is gone — it was the
// row-86 hazard (the scratch lived above the window where multret
// arg windows legitimately reach).
func (fe *funcEmitter) emitCall(A, B, C, pc int, tail bool) {
	f := fe.f
	if tail {
		// the interpreter closes this frame before a tailcall (_vm.go
		// OP_TAILCALL: upvalues + the tbc list at lbase — U4f) — the frame
		// is not coming back
		f.LocalGet(0)
		f.Call(fe.b.imp("rt_close"))
		fe.checkStatus()
	}
	// nargs
	if B == 0 {
		f.LocalGet(fe.lTop).I32Const(int32(A + 1)).I32Sub()
	} else {
		f.I32Const(int32(B - 1))
	}
	f.LocalSet(fe.lT0)
	// want
	if C == 0 {
		f.I32Const(-1)
	} else {
		f.I32Const(int32(C - 1))
	}
	f.LocalSet(fe.lT1)

	// M5c: try a staged tailcall — a wasm-closure callee re-dispatches at
	// the same adapter level via the -2 sentinel (O(1) wasm stack, O(1)
	// frames). Anything else (C function, __call'd object) declines
	// WITHOUT touching the cells and takes the rt_call_addr fallback
	// below on the same operands; its results then flow through the
	// RETURN B=0 sentinel that follows (top = A + count).
	if tail {
		fe.cellAddr(A)  // funcell (callee in place)
		fe.cellAddr(A + 1) // argcells (natural positions)
		f.LocalGet(fe.lT0)
		fe.varargBase() // the BLOCK address — staging restores the
		//                 cursor to the allocation start (U6a)
		f.Call(fe.b.imp("rt_tail_stage")).LocalSet(fe.lSt)
		f.LocalGet(fe.lSt).I32Const(0).I32GeS().If(wasm.Void)
		f.I32Const(-2).Return()
		f.End()
	}

	// rt_call_addr(&R(A), &R(A+1), nargs, want, line, hcallee)
	fe.cellAddr(A)
	fe.cellAddr(A + 1)
	f.LocalGet(fe.lT0)
	f.LocalGet(fe.lT1)
	f.I32Const(fe.line(pc))
	// row-71: the call-error hint — funcnamefromcode at the CALL/TAILCALL
	// site ("global 'foo'", "method 'bar'", ...; -1 when unattributable,
	// the interp's register value-scan residual)
	f.I32Const(int32(fe.hintCallSite(pc)))
	f.Call(fe.b.imp("rt_call_addr")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()

	// adjust top: multret (C==0, and every tailcall) → A + count;
	// else A + (C-1)
	if C == 0 {
		f.LocalGet(fe.lSt).Call(fe.b.imp("rt_call_count"))
		f.I32Const(int32(A)).I32Add().LocalSet(fe.lTop)
	} else {
		f.I32Const(int32(A + C - 1)).LocalSet(fe.lTop)
	}
}

// emitReturn: results at R(A); B==0 → up to top. The frame's open upvalues
// and to-be-closed variables close first (the interpreter's doReturn closes
// both at LocalBase — here frame base; U4f). nret results stage at
// frame+0.. for the trampoline.
func (fe *funcEmitter) emitReturn(A, B int) {
	f := fe.f
	f.LocalGet(0)
	f.Call(fe.b.imp("rt_close"))
	fe.checkStatus()
	if B >= 2 {
		n := B - 1
		for i := 0; i < n; i++ {
			fe.frameCellAddrConst(i)
			fe.cellAddr(A + i).I64Load(0)
			f.I64Store(0)
			fe.frameCellAddrConst(i)
			fe.cellAddr(A + i).I64Load(8)
			f.I64Store(8)
		}
		f.I32Const(int32(n)).Return()
		return
	}
	if B == 1 {
		f.I32Const(0).Return()
		return
	}
	// B == 0: count = top - A, dynamic copy
	f.LocalGet(fe.lTop).I32Const(int32(A)).I32Sub().LocalSet(fe.lT0)
	f.I32Const(0).LocalSet(fe.lT1)
	f.Block(wasm.Void)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
	fe.copyDyn(A)
	f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
	f.Br(0)
	f.End()
	f.End()
	f.LocalGet(fe.lT0).Return()
}

// frameCellAddrConst: frame+16*i — the result-staging address. (A bare
// i32.const here was a latent M4 bug: fixed-count RETURNs staged their
// results at ABSOLUTE addresses 0.. — corrupting the runtime's static
// data.)
func (fe *funcEmitter) frameCellAddrConst(i int) {
	fe.f.LocalGet(0).I32Const(int32(cellSize * i)).I32Add()
}

// copyDyn: full 16-byte copy of dynCellAddr(A) → frameDynCell()
func (fe *funcEmitter) copyDyn(baseReg int) {
	f := fe.f
	fe.dynCellAddr(baseReg)
	f.I64Load(0).LocalSet(fe.lVlo)
	fe.dynCellAddr(baseReg)
	f.I64Load(8).LocalSet(fe.lVhi)
	fe.frameDynCell()
	f.LocalGet(fe.lVlo).I64Store(0)
	fe.frameDynCell()
	f.LocalGet(fe.lVhi).I64Store(8)
}

// emitForprep: rt_forprep's tristate — RT_OK enters the body (fall to
// pc+1; the runtime rewrote the cells to (count, step, init) for the
// integer flavor or (limit, step, init) all-float), RT_FORPREP_SKIP
// (2) jumps past the loop (pc+Bx+2 — one past the FORLOOP), RT_ERR
// returns -1.
func (fe *funcEmitter) emitForprep(A, Bx, pc int) {
	f := fe.f
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_forprep")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()
	f.LocalGet(fe.lSt).I32Const(2).I32Eq().If(wasm.Void)
	fe.setBlk(fe.blockOf(fe.forprepSkipTarget(pc)))
	f.Else()
	fe.setBlk(fe.blockOf(pc + 1))
	f.End()
}

// emitForloop: the count-based stock flow, both flavors inline. The
// flavor is R[A]'s tag after rt_forprep's rewrite: VNUMINT → the
// integer loop (R[A]=iterations-remaining, R[A+1]=step, R[A+2]=control;
// decrement the counter, control += step, back edge while nonzero — the
// counter can never wrap); VNUMFLT → the float loop (R[A]=limit,
// R[A+1]=step, R[A+2]=idx; idx += step, continue while within the
// limit). Non-number control falls to the exit (the interp's !ok3).
func (fe *funcEmitter) emitForloop(A, Bx, pc int) {
	f := fe.f
	back := fe.blockOf(fe.forloopTarget(pc))
	exit := fe.blockOf(pc + 1)
	fe.tagIs(A, rtTagInt).If(wasm.Void)
	{
		fe.cellAddr(A).I64Load(0).LocalSet(fe.lV2)
		f.LocalGet(fe.lV2).I64Const(0).I64Ne().If(wasm.Void)
		fe.cellAddr(A)
		f.LocalGet(fe.lV2).I64Const(1).I64Sub().I64Store(0)
		fe.cellAddr(A + 2)
		fe.cellAddr(A + 2).I64Load(0)
		fe.cellAddr(A + 1).I64Load(0)
		f.I64Add().I64Store(0)
		fe.setBlk(back)
		f.Else()
		fe.setBlk(exit)
		f.End()
	}
	f.Else()
	{
		fe.tagIs(A, rtTagFloat).If(wasm.Void)
		{
			fe.cellAddr(A + 2).F64Load(0)
			fe.cellAddr(A + 1).F64Load(0)
			f.F64Add().LocalSet(fe.lF0)
			// (step > 0 && idx <= limit) || (step <= 0 && idx >= limit)
			fe.cellAddr(A + 1).F64Load(0).F64Const(0).F64Gt()
			f.LocalGet(fe.lF0)
			fe.cellAddr(A).F64Load(0)
			f.F64Le()
			f.I32And()
			fe.cellAddr(A + 1).F64Load(0).F64Const(0).F64Le()
			f.LocalGet(fe.lF0)
			fe.cellAddr(A).F64Load(0)
			f.F64Ge()
			f.I32And()
			f.I32Or().If(wasm.Void)
			fe.cellAddr(A + 2)
			f.LocalGet(fe.lF0).F64Store(0)
			fe.setBlk(back)
			f.Else()
			fe.setBlk(exit)
			f.End()
		}
		f.Else()
		fe.setBlk(exit)
		f.End()
	}
	f.End()
}

// emitTForcall: the generic-for call, fused with the TFORLOOP at pc+1
// (the interp dispatches them back-to-back the same way). Staging per
// _vm.go: the triple goes to R(A+3..A+5) — R(A+5)←control, R(A+4)←
// state, R(A+3)←iterator. U6a (row 86(A)): the callee stays at R(A+3)
// with its two args at their natural R(A+4).. — rt_call_addr takes the
// address and moves the C results down to R(A+3) itself (the old
// save-to-scratch + shift is gone). Continue iff R(A+3) is non-nil.
func (fe *funcEmitter) emitTForcall(A, C, pc int) {
	f := fe.f
	fe.bumpTop(A + 6)
	fe.copyCell(fe.cell(A+5), fe.cell(A+3)) // control → arg slot
	fe.copyCell(fe.cell(A+4), fe.cell(A+1)) // state
	fe.copyCell(fe.cell(A+3), fe.cell(A))   // iterator (callee)
	fe.cellAddr(A + 3)                       // funcell (callee in place)
	fe.cellAddr(A + 4)                       // argcells (natural)
	f.I32Const(2)
	f.I32Const(int32(C))
	f.I32Const(fe.line(pc))
	// row-71: funcnamefromcode over TFORCALL names the for iterator
	f.I32Const(int32(fe.hintCallSite(pc)))
	f.Call(fe.b.imp("rt_call_addr")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()
	f.I32Const(int32(A + 3 + C)).LocalSet(fe.lTop)
	// TFORLOOP (at pc+1): continue iff R(A+3) ~= nil (byte-wide tag load —
	// the iterator cell is C-written)
	fe.cellAddr(A + 3)
	f.I32Load8U(8).I32Const(rtTagNil).I32Ne().If(wasm.Void)
	fe.setBlk(fe.blockOf(fe.forloopTarget(pc + 1))) // body (back edge)
	f.Else()
	fe.setBlk(fe.blockOf(pc + 2)) // exit: past the TFORLOOP
	f.End()
}

// emitVararg: R(A).. := the actual varargs. k=0 (hidden model): the
// extras at varargBase (BELOW the register window since U6a/row 86);
// nvarargs = max(0, nargs - np). C==0 → multret (copy all, top =
// A+nvarargs); C==1 → zero values (top = A); C>=2 → exactly C-1
// values, copying min(C-1, nvarargs) and nil-padding the rest (top =
// A+C-1 — registry CopyRange truncates to regv+n).
//
// k=1 (table model, U5e row 85 — the interp spec, _vm.go): B is the
// vararg TABLE's register; rt_vararg_read reads t.n + the raw values
// (raising "vararg table has no proper 'n'" under rt_run, so the raise
// carries the site's position), nil-pads to nwant, and returns the
// resolved count — multret (C==0, nwant=-1) sets top = A+count, fixed C
// knows it statically.
func (fe *funcEmitter) emitVararg(A, B, C, k, pc int) {
	f := fe.f
	if k != 0 {
		fe.cellAddr(A)
		fe.cellAddr(B)
		if C == 0 {
			f.I32Const(-1) // multret: all of t.n
		} else {
			f.I32Const(int32(C - 1))
		}
		// dstcap — the allocated cells past dst, mirroring the hidden
		// model's multret posture (writes may reach past nregs into the
		// slack push_frame sized for): framecells + 1 (the window's
		// cells past A plus the spare above it) - A. The extras are
		// BELOW the window since U6a, so they no longer bound the
		// flush. t.n is user-editable in the table model, so unlike
		// the hidden model's nargs-bounded count it needs the explicit
		// bound (the interp grows its register stack instead;
		// unobservable in the corpus).
		f.I32Const(int32(frameCells(fe.nregs) + 1)).I32Const(int32(A)).I32Sub()
		f.I32Const(fe.line(pc))
		f.Call(fe.b.imp("rt_vararg_read")).LocalSet(fe.lSt)
		// NOT checkStatus: a legitimate count can be 1 (RT_ERR's value)
		f.LocalGet(fe.lSt).I32Const(0).I32LtS().
			If(wasm.Void).
			I32Const(-1).Return().
			End()
		if C == 0 {
			f.I32Const(int32(A)).LocalGet(fe.lSt).I32Add().LocalSet(fe.lTop)
		} else {
			f.I32Const(int32(A + C - 1)).LocalSet(fe.lTop)
		}
		return
	}
	// lT0 = nvarargs
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().LocalSet(fe.lT0)
	f.LocalGet(fe.lT0).I32Const(0).I32LtS().If(wasm.Void)
	f.I32Const(0).LocalSet(fe.lT0)
	f.End()

	if C <= 1 {
		f.I32Const(int32(A)).LocalSet(fe.lTop)
		if C == 0 {
			// multret: top = A + nvarargs, dynamic copy
			f.I32Const(int32(A)).LocalGet(fe.lT0).I32Add().LocalSet(fe.lTop)
			f.I32Const(0).LocalSet(fe.lT1)
			f.Block(wasm.Void)
			f.Loop(wasm.Void)
			f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
			fe.varargCellDyn().I64Load(0).LocalSet(fe.lVlo)
			fe.varargCellDyn().I64Load(8).LocalSet(fe.lVhi)
			fe.dynCellAddr(A)
			f.LocalGet(fe.lVlo).I64Store(0)
			fe.dynCellAddr(A)
			f.LocalGet(fe.lVhi).I64Store(8)
			f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
			f.Br(0)
			f.End()
			f.End()
		}
		return
	}
	// C >= 2: exactly m = C-1 values, nil-padded past nvarargs
	m := C - 1
	for i := 0; i < m; i++ {
		f.LocalGet(fe.lT0).I32Const(int32(i)).I32GtS().If(wasm.Void)
		fe.varargCell(i).I64Load(0).LocalSet(fe.lVlo)
		fe.varargCell(i).I64Load(8).LocalSet(fe.lVhi)
		fe.cellAddr(A + i)
		f.LocalGet(fe.lVlo).I64Store(0)
		fe.cellAddr(A + i)
		f.LocalGet(fe.lVhi).I64Store(8)
		f.Else()
		fe.cellAddr(A + i)
		f.I32Const(0).I32Store(8) // tag = nil (full 4 bytes)
		f.End()
	}
	f.I32Const(int32(A + m)).LocalSet(fe.lTop)
}

// emitSetlist: R(A)[base+i] = R(A+1+i-1) for i in 1..n — keys are
// integers (table-key normalization is the runtime's). base = vC plus
// the EXTRAARG high word when k; n = vB, or top-A-1 when vB==0 (multret
// flush).
func (fe *funcEmitter) emitSetlist(inst uint32, A, pc int) {
	f := fe.f
	vB := lua.OpGetArgvB(inst)
	vC := lua.OpGetArgvC(inst)
	k := lua.OpGetArgk(inst)
	base := vC
	if k != 0 {
		base += lua.OpGetArgAx(fe.code[pc+1]) * 1024
	}
	if vB == 0 {
		f.LocalGet(fe.lTop).I32Const(int32(A + 1)).I32Sub().LocalSet(fe.lT0)
	} else {
		f.I32Const(int32(vB)).LocalSet(fe.lT0)
	}
	f.I32Const(0).LocalSet(fe.lT1)
	f.Block(wasm.Void)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
	// key = mkinteger(scratch0, base + i + 1)
	fe.scratchAddr(0)
	f.LocalGet(fe.lT1).I32Const(int32(base + 1)).I32Add().I64ExtendI32S()
	f.Call(fe.b.imp("rt_mkinteger"))
	// rt_settable(R(A), scratch0, R(A+1+i), line) — no hints: A holds a
	// fresh NEWTABLE result (no metamethod can be involved), so the
	// interp's varinfo answer here is never consulted
	fe.cellAddr(A)
	fe.scratchAddr(0)
	fe.dynCellAddr(A + 1)
	f.I32Const(fe.line(pc))
	f.I32Const(-1)
	f.I32Const(-1)
	f.Call(fe.b.imp("rt_settable")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()
	f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
	f.Br(0)
	f.End()
	f.End()
}
