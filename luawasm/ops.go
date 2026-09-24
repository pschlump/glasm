package luawasm

// Opcode emitters for the trickier lowers: arithmetic (inline number
// fast path + ABI fallback), comparisons, calls, returns, the numeric
// for loop, TFORLOOP and SETLIST (design doc §4.4).

import (
	"github.com/pschlump/golua55"
	"github.com/pschlump/golua55/wasm"
)

// emitArith: R(A) := RK(B) <op> RK(C). Inline fast path when both
// operands are registers holding numbers; ABI otherwise (string
// coercion, metamethods, errors).
func (fe *funcEmitter) emitArith(op, A, B, C, pc int) {
	f := fe.f
	bothRegs := B&0x100 == 0 && C&0x100 == 0
	fastable := op == lua.OP_ADD || op == lua.OP_SUB || op == lua.OP_MUL || op == lua.OP_DIV
	if bothRegs && fastable {
		fe.cellAddr(B)
		f.I32Load8U(8).I32Const(rtNumTag).I32Eq()
		fe.cellAddr(C)
		f.I32Load8U(8).I32Const(rtNumTag).I32Eq()
		f.I32And().If(wasm.Void)
		// fast: f64 op inline into R(A)
		fe.cellAddr(A)
		fe.cellAddr(B).F64Load(0)
		fe.cellAddr(C).F64Load(0)
		fe.arithF64(op)
		f.F64Store(0)
		fe.cellAddr(A)
		f.I32Const(rtNumTag).I32Store(8) // full 4-byte tag (recycled-cell safety)
		fe.bumpTop(A + 1)
		f.Else()
		f.I32Const(int32(op))
		fe.rkAddr(B)
		fe.rkAddr(C)
		fe.cellAddr(A)
		f.I32Const(fe.line(pc))
		f.Call(fe.b.imp("rt_arith"))
		fe.checkStatus()
		fe.bumpTop(A + 1)
		f.End()
		return
	}
	f.I32Const(int32(op))
	fe.rkAddr(B)
	fe.rkAddr(C)
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(A + 1)
}

func (fe *funcEmitter) arithF64(op int) {
	f := fe.f
	switch op {
	case lua.OP_ADD:
		f.F64Add()
	case lua.OP_SUB:
		f.F64Sub()
	case lua.OP_MUL:
		f.F64Mul()
	case lua.OP_DIV:
		f.F64Div()
	}
}

// emitUnm: R(A) := -R(B)
func (fe *funcEmitter) emitUnm(A, B, pc int) {
	f := fe.f
	fe.cellAddr(B)
	f.I32Load8U(8).I32Const(rtNumTag).I32Eq().If(wasm.Void)
	fe.cellAddr(A)
	fe.cellAddr(B).F64Load(0)
	f.F64Neg().F64Store(0)
	fe.cellAddr(A)
	f.I32Const(rtNumTag).I32Store(8) // full 4-byte tag (recycled-cell safety)
	fe.bumpTop(A + 1)
	f.Else()
	f.I32Const(int32(lua.OP_UNM))
	fe.cellAddr(B)
	fe.cellAddr(B)
	fe.cellAddr(A)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_arith"))
	fe.checkStatus()
	fe.bumpTop(A + 1)
	f.End()
}

// emitCompare: EQ/LT/LE fused with the following JMP. cond via the ABI
// (handles mixed types, strings, metamethods) into a scratch cell.
func (fe *funcEmitter) emitCompare(op, A, B, C, pc int) {
	f := fe.f
	var fn string
	switch op {
	case lua.OP_EQ:
		fn = "rt_eq"
	case lua.OP_LT:
		fn = "rt_lt"
	default:
		fn = "rt_le"
	}
	// rt_eq/rt_lt/rt_le(a, b, dst, line) — dst THIRD
	fe.rkAddr(B)
	fe.rkAddr(C)
	fe.scratchAddr(0)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp(fn))
	fe.checkStatus()
	// if (cond ~= A) then pc++ (skip the partner) else take the partner
	// (a NOP partner — a rewritten jump-to-next — falls to pc+2; row 41)
	fe.scratchAddr(0)
	f.I32Load(0)
	f.I32Const(int32(A)).I32Ne().If(wasm.Void)
	fe.setBlk(fe.blockOf(pc + 2))
	f.Else()
	fe.setBlk(fe.branchPartner(pc))
	f.End()
}

// emitCall: OP_CALL (tailcall=false) and OP_TAILCALL (tailcall=true).
// The callee executes through rt_call — the runtime's lvm runs Lua
// closures and C functions alike.
//
// rt_call(funcell, argcells, nargs, want, line) reads args from argcells
// and writes results back over the SAME cells. OP_CALL has the callee at
// R(A), args at R(A+1).., and results at R(A).. — so before the call the
// callee is saved to a scratch cell and the args are shifted one cell
// down into R(A)..; results then land exactly where the VM expects them
// (static and multret alike).
func (fe *funcEmitter) emitCall(A, B, C, pc int, tail bool) {
	f := fe.f
	if tail {
		// the interpreter closes this frame's upvalues before a tailcall
		// (_vm.go:610) — the frame is not coming back
		f.LocalGet(0)
		f.Call(fe.b.imp("rt_close_upvals"))
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

	// save the callee: scratch0 := R(A)
	fe.copyCell(func() { fe.scratchAddr(0) }, func() { fe.cellAddr(A) })

	// shift args down one cell: R(A+i) := R(A+1+i), ascending (each src is
	// read before the next step overwrites it)
	if B == 0 {
		// dynamic count = nargs (lT0); index lives in lSt — free until the
		// rt_call result lands there. lT1 must survive: it holds want.
		f.I32Const(0).LocalSet(fe.lSt)
		f.Block(wasm.Void)
		f.Loop(wasm.Void)
		f.LocalGet(fe.lSt).LocalGet(fe.lT0).I32GeS().BrIf(1)
		fe.copyCell(func() { fe.dynCellAddrSt(A) }, func() { fe.dynCellAddrSt(A + 1) })
		f.LocalGet(fe.lSt).I32Const(1).I32Add().LocalSet(fe.lSt)
		f.Br(0)
		f.End()
		f.End()
	} else {
		for i := 0; i < B-1; i++ {
			fe.copyCell(func() { fe.cellAddr(A + i) }, func() { fe.cellAddr(A + 1 + i) })
		}
	}

	// M5c: try a staged tailcall — a wasm-closure callee re-dispatches at
	// the same adapter level via the -2 sentinel (O(1) wasm stack, O(1)
	// frames). Anything else (C function, __call'd object — precall's
	// tryfuncTM resolves those) declines and takes the rt_call fallback,
	// which matches the interpreter: C tailcalls don't recurse.
	if tail {
		fe.scratchAddr(0)
		fe.cellAddr(A)
		f.LocalGet(fe.lT0)
		f.LocalGet(0) // frame — staging restores the cursor to here
		f.Call(fe.b.imp("rt_tail_stage")).LocalSet(fe.lSt)
		f.LocalGet(fe.lSt).I32Const(0).I32GeS().If(wasm.Void)
		f.I32Const(-2).Return()
		f.End()
	}

	// rt_call(scratch0, &R(A), nargs, want, line)
	fe.scratchAddr(0)
	fe.cellAddr(A)
	f.LocalGet(fe.lT0)
	f.LocalGet(fe.lT1)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_call")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()

	if tail {
		// results (count = rt_call_count(status)) copied to the frame
		// base, then returned — a real tailcall optimization lands with
		// the v2 structured emitter
		f.LocalGet(fe.lSt).Call(fe.b.imp("rt_call_count")).LocalSet(fe.lT0)
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
		return
	}

	// adjust top: want<0 → A + count; else A + (C-1)
	if C == 0 {
		f.LocalGet(fe.lSt).Call(fe.b.imp("rt_call_count"))
		f.I32Const(int32(A)).I32Add().LocalSet(fe.lTop)
	} else {
		f.I32Const(int32(A + C - 1)).LocalSet(fe.lTop)
	}
}

// dynCellAddrSt: like dynCellAddr but indexed by lSt — for copy loops
// where lT0/lT1 hold live values (nargs/want).
func (fe *funcEmitter) dynCellAddrSt(baseReg int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * baseReg)).I32Add().
		LocalGet(fe.lSt).I32Const(16).I32Mul().I32Add()
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

// emitReturn: results at R(A); B==0 → up to top. The frame's open
// upvalues close first (the interpreter's OP_RETURN, _vm.go:659).
func (fe *funcEmitter) emitReturn(A, B int) {
	f := fe.f
	f.LocalGet(0)
	f.Call(fe.b.imp("rt_close_upvals"))
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
// data; main-chunk returns rarely had B>=2 so the corpus never saw it.
// Called functions made it visible immediately: add(3,4) returned its
// untouched arg0 cell.)
func (fe *funcEmitter) frameCellAddrConst(i int) {
	fe.f.LocalGet(0).I32Const(int32(cellSize * i)).I32Add()
}

// emitForloop: init += step (cells); if (step>0 && init<=limit) ||
// (step<=0 && init>=limit) then R(A+3)=init, jump back; else fall through.
func (fe *funcEmitter) emitForloop(A, pc int) {
	f := fe.f
	// f0 = init + step; store to R(A)
	fe.cellAddr(A)
	fe.cellAddr(A).F64Load(0)
	fe.cellAddr(A + 2).F64Load(0)
	f.F64Add().LocalSet(fe.lF0)
	fe.cellAddr(A)
	f.LocalGet(fe.lF0).F64Store(0)
	// cond
	fe.cellAddr(A + 2).F64Load(0).F64Const(0).F64Gt()
	fe.cellAddr(A).F64Load(0)
	fe.cellAddr(A + 1).F64Load(0)
	f.F64Le()
	f.I32And()
	fe.cellAddr(A + 2).F64Load(0).F64Const(0).F64Le()
	fe.cellAddr(A).F64Load(0)
	fe.cellAddr(A + 1).F64Load(0)
	f.F64Ge()
	f.I32And()
	f.I32Or().If(wasm.Void)
	fe.cellAddr(A + 3)
	f.LocalGet(fe.lF0).F64Store(0)
	fe.cellAddr(A + 3)
	f.I32Const(3).I32Store(8) // loop var is a number: set the tag too (full 4 bytes)
	fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
	f.Else()
	fe.setBlk(fe.blockOf(pc + 1))
	f.End()
}

// emitTForloop: generic for. The bytecode is `TFORLOOP A C; JMP sBx` where
// the JMP is a pseudo-instruction — TFORLOOP's encoded back-edge to the
// loop body, never executed on its own (vm.go applies the FOLLOWING
// instruction's sBx to continue; exit falls through past it).
//
// Mirrors the interpreter: stage the iterator triple at R(A+3..A+5) (so
// the call's results cannot clobber the loop's state/control cells), call
// R(A+3)(R(A+4), R(A+5)) with C results at R(A+3)..; continue iff the
// first result (new control) is non-nil — then R(A+2) = R(A+3) and jump
// to the pseudo-JMP's target (the body); else fall to pc+2.
func (fe *funcEmitter) emitTForloop(A, C, pc int) {
	f := fe.f
	// stage the triple at R(A+3..A+5), then apply the OP_CALL shape: the
	// callee goes to scratch and the two args shift down one cell, so
	// rt_call's results (written over argcells) land at R(A+3).. — where
	// callR(2, nret, RA+3) puts them in the interpreter
	fe.copyCell(func() { fe.cellAddr(A + 3) }, func() { fe.cellAddr(A) })     // iterator
	fe.copyCell(func() { fe.cellAddr(A + 4) }, func() { fe.cellAddr(A + 1) }) // state
	fe.copyCell(func() { fe.cellAddr(A + 5) }, func() { fe.cellAddr(A + 2) }) // control
	fe.copyCell(func() { fe.scratchAddr(0) }, func() { fe.cellAddr(A + 3) })  // callee → scratch
	fe.copyCell(func() { fe.cellAddr(A + 3) }, func() { fe.cellAddr(A + 4) }) // args ↓ 1
	fe.copyCell(func() { fe.cellAddr(A + 4) }, func() { fe.cellAddr(A + 5) })
	// rt_call(scratch0, &R(A+3), 2, C, line) → C results at R(A+3)..
	fe.scratchAddr(0)
	fe.cellAddr(A + 3)
	f.I32Const(2)
	f.I32Const(int32(C))
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_call")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()
	fe.bumpTop(A + 3 + C)
	// continue iff R(A+3) ~= nil (raw tag byte != 0)
	fe.cellAddr(A + 3)
	f.I32Load8U(8).I32Const(0).I32Ne().If(wasm.Void)
	fe.copyCell(func() { fe.cellAddr(A + 2) }, func() { fe.cellAddr(A + 3) })
	fe.setBlk(fe.blockOf(fe.jumpTarget(pc + 1))) // body (pseudo-JMP target)
	f.Else()
	fe.setBlk(fe.blockOf(pc + 2)) // exit: past the pseudo-JMP
	f.End()
}

// emitVararg: R(A).. := the actual varargs (varargBase, above the
// register window). nvarargs = max(0, nargs - np) — dynamic, from the
// nargs param. Mirrors the interpreter's OP_VARARG (_vm.go): B==0 →
// multret (copy all, top = A+nvarargs); B==1 → zero values (a bare
// `...` statement); B>=2 → exactly B-1 values, copying
// min(B-1, nvarargs) and nil-padding the rest.
func (fe *funcEmitter) emitVararg(A, B int) {
	f := fe.f
	// lT0 = nvarargs
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().LocalSet(fe.lT0)
	f.LocalGet(fe.lT0).I32Const(0).I32LtS().If(wasm.Void)
	f.I32Const(0).LocalSet(fe.lT0)
	f.End()

	if B == 1 {
		return // nwant = 0
	}
	if B == 0 {
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
		return
	}
	// B >= 2: exactly m = B-1 values, nil-padded past nvarargs
	m := B - 1
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
	fe.bumpTop(A + m)
}

// emitSetlist: R(A)[base+i+1] = R(A+1+i) for i in 0..count-1
func (fe *funcEmitter) emitSetlist(A, B, C, pc int) {
	f := fe.f
	base := (C - 1) * 50
	if B == 0 {
		f.LocalGet(fe.lTop).I32Const(int32(A + 1)).I32Sub().LocalSet(fe.lT0)
	} else {
		f.I32Const(int32(B)).LocalSet(fe.lT0)
	}
	f.I32Const(0).LocalSet(fe.lT1)
	f.Block(wasm.Void)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lT1).LocalGet(fe.lT0).I32GeS().BrIf(1)
	// key = mknumber(base + i + 1) into scratch 0
	fe.scratchAddr(0)
	f.LocalGet(fe.lT1).I32Const(int32(base + 1)).I32Add().F64ConvertI32S()
	f.Call(fe.b.imp("rt_mknumber"))
	// rt_settable(R(A), scratch0, R(A+1+i), line)
	fe.cellAddr(A)
	fe.scratchAddr(0)
	fe.dynCellAddr(A + 1)
	f.I32Const(fe.line(pc))
	f.Call(fe.b.imp("rt_settable")).LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().If(wasm.Void)
	f.I32Const(-1).Return()
	f.End()
	f.LocalGet(fe.lT1).I32Const(1).I32Add().LocalSet(fe.lT1)
	f.Br(0)
	f.End()
	f.End()
}
