package luawasm

// The function emitter: lowers one FunctionProto's bytecode to a wasm
// function with flattened control flow (design doc §4.3/§4.4). Registers
// are TValue cells at frame+16*k; the frame is allocated and arg-filled
// by the C adapter (precall_wasm) — params at R(0..np-1), varargs above
// the register window at frame+16*framecells (M5b reads them there).
//
// Signature (ABI v3): (frame, cl, nargs, want) -> nret — the common
// convention every protoFn and lua_dispatch share. nret >= 0: that many
// results staged at frame+0..; -1: error, exact TValue staged in the
// runtime; -2: tailcall sentinel (M5c).

import (
	"fmt"

	"github.com/pschlump/golua55"
	"github.com/pschlump/golua55/wasm"
)

const opMaxArgSbx = (1<<18 - 1) >> 1

type funcEmitter struct {
	b     *backend
	f     *wasm.Function
	proto *lua.FunctionProto
	pi    protoInfo
	nregs int
	np    int
	code  []uint32

	// local indices (params 0..3 = frame, cl, nargs, want)
	lTop, lBlk, lSt, lT0, lT1 uint32 // i32
	lVlo, lVhi                uint32 // i64
	lF0                       uint32 // f64

	blocks  map[int]int  // pc -> block id
	blockPC []int        // block id -> start pc
	pseudo  map[int]bool // pc is an upvalue-capture pseudo (never real code)
}

func (b *backend) newFuncEmitter(p *lua.FunctionProto) *funcEmitter {
	f := b.m.NewFunction([]wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32}, []wasm.ValueType{wasm.I32})
	f.Local(wasm.I32).Local(wasm.I32).Local(wasm.I32).Local(wasm.I32).Local(wasm.I32) // 4..8
	f.Local(wasm.I64).Local(wasm.I64)                                                 // 9,10
	f.Local(wasm.F64)                                                                 // 11
	return &funcEmitter{
		b: b, f: f, proto: p,
		pi:    b.protoInfoOf(p),
		nregs: int(p.NumUsedRegisters),
		np:    int(p.NumParameters),
		code:  p.Code,
		lTop:  4, lBlk: 5, lSt: 6, lT0: 7, lT1: 8,
		lVlo: 9, lVhi: 10, lF0: 11,
	}
}

/* ---------- small emission helpers ---------- */

func (b *backend) protoInfoOf(p *lua.FunctionProto) protoInfo {
	for _, pi := range b.protos {
		if pi.proto == p {
			return pi
		}
	}
	panic("luawasm: proto not in layout")
}

func (fe *funcEmitter) cellAddr(k int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * k)).I32Add()
}

// scratch cell 0 — the adapter's +1 spare cell ABOVE the vararg staging
// area. Dynamic for vararg protos: multret VARARG destinations reach
// R(A+nv-1) with A up to nregs-1, so any static nregs+k position can
// collide (M5b bug: `g(2, ...)` saved its callee over the third vararg).
// frame + 16*(nregs+4) + 16*max(0, nargs-np) is provably above both the
// register window and every vararg copy.
func (fe *funcEmitter) scratchAddr(k int) *wasm.Function {
	f := fe.f
	base := f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 4 + k))).I32Add()
	if fe.proto.IsVarArg&lua.VarArgIsVarArg == 0 {
		return base
	}
	// + 16 * max(0, nargs - np) — Select keeps live locals untouched
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().I32Const(16).I32Mul()
	f.I32Const(0)
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().I32Const(0).I32GtS()
	f.Select()
	return f.I32Add()
}

// constant cell: i in Constants (global pool offset per proto)
func (fe *funcEmitter) kcell(i int) *wasm.Function {
	return fe.f.GlobalGet(fe.b.gKCells).I32Const(int32(cellSize * (fe.pi.koff + i))).I32Add()
}

// string-constant cell: i in StringConstants (after the proto's
// Constants in the global pool)
func (fe *funcEmitter) kscell(i int) *wasm.Function {
	return fe.f.GlobalGet(fe.b.gKCells).I32Const(int32(cellSize * (fe.pi.koff + fe.pi.nc + i))).I32Add()
}

// RK operand address: register or constant cell
func (fe *funcEmitter) rkAddr(v int) *wasm.Function {
	if v&0x100 != 0 {
		return fe.kcell(v &^ 0x100)
	}
	return fe.cellAddr(v)
}

// rkString operand address (the *KS opcodes): K bit → string-constant
// cell, else the register holds the string
func (fe *funcEmitter) rksAddr(v int) *wasm.Function {
	if v&0x100 != 0 {
		return fe.kscell(v &^ 0x100)
	}
	return fe.cellAddr(v)
}

// copyCell: 16-byte TValue copy (dst, src are address emitters)
func (fe *funcEmitter) copyCell(dst func(), src func()) {
	f := fe.f
	src()
	f.I64Load(0).LocalSet(fe.lVlo)
	src()
	f.I64Load(8).LocalSet(fe.lVhi)
	dst()
	f.LocalGet(fe.lVlo).I64Store(0)
	dst()
	f.LocalGet(fe.lVhi).I64Store(8)
}

// dynamic cell copy: base register + 16*t (t in lT1)
func (fe *funcEmitter) dynCellAddr(baseReg int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * baseReg)).I32Add().
		LocalGet(fe.lT1).I32Const(16).I32Mul().I32Add()
}

// vararg cells live above the register window (the adapter's split —
// plan §3.2): varargBase = frame + 16*framecells, framecells = nregs+4
func (fe *funcEmitter) varargCell(j int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 4 + j))).I32Add()
}

// vararg cell indexed by lT1 (dynamic copy loops)
func (fe *funcEmitter) varargCellDyn() *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 4))).I32Add().
		LocalGet(fe.lT1).I32Const(16).I32Mul().I32Add()
}

func (fe *funcEmitter) frameDynCell() *wasm.Function {
	return fe.f.LocalGet(0).LocalGet(fe.lT1).I32Const(16).I32Mul().I32Add()
}

// bumpTop: if top < n { top = n } (interpreter registry semantics)
func (fe *funcEmitter) bumpTop(n int) {
	f := fe.f
	f.LocalGet(fe.lTop).I32Const(int32(n)).I32LtS().
		If(wasm.Void).
		I32Const(int32(n)).LocalSet(fe.lTop).
		End()
}

// truth: leaves 1/0 on the stack — Lua truthiness of register k
// (false iff nil, or boolean with value.b == 0)
func (fe *funcEmitter) truth(reg int) {
	f := fe.f
	fe.cellAddr(reg)
	f.I32Load8U(8).I32Eqz() // tag == nil(0)
	fe.cellAddr(reg)
	f.I32Load8U(8).I32Const(1).I32Eq()
	fe.cellAddr(reg)
	f.I32Load(0).I32Eqz()
	f.I32And()
	f.I32Or()
}

// setBoolCellA: R(A) := boolean; cond is an address-less emitter leaving
// the 0/1 i32 on the stack
func (fe *funcEmitter) setBoolCellA(a int, cond func()) {
	f := fe.f
	cond()
	f.LocalSet(fe.lT0)
	fe.cellAddr(a)
	f.LocalGet(fe.lT0).I32Store(0)
	fe.cellAddr(a)
	// full 4-byte tag store: tt is an int — a 1-byte store leaves recycled
	// (non-zero-fresh) cell memory with garbage upper tag bytes, which the
	// C runtime's ttype() then reads whole (fuzzer rows 45+: reused frames)
	f.I32Const(1).I32Store(8)
}

// checkStatus: st := <status on stack>; if st == RT_ERR return -1 (the
// ABI v3 error convention — -1 is unambiguous with "1 result")
func (fe *funcEmitter) checkStatus() {
	f := fe.f
	f.LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Const(1).I32Eq().
		If(wasm.Void).
		I32Const(-1).Return().
		End()
}

func (fe *funcEmitter) setBlk(id int) {
	fe.f.I32Const(int32(id)).LocalSet(fe.lBlk)
}

func (fe *funcEmitter) line(pc int) int32 {
	return int32(fe.proto.DbgSourcePositions[pc])
}

/* ---------- body ---------- */

func (fe *funcEmitter) emitBody() {
	f := fe.f

	// gFrameCells (main proto only): registers + 2-cell TFORLOOP margin +
	// the scratch spare — the TOTAL cell count the engine must allocate
	// for lua_main's frame (main is vararg with nargs=0, so its scratch
	// sits at nregs+4). The interpreter can write 2 cells past
	// NumUsedRegisters (TFORLOOP stages its triple at R(A+3..A+5), and
	// the compiler doesn't always reserve those); the margin absorbs
	// that, and scratch lives above everything.
	if fe.proto == fe.b.main {
		fe.b.m.ExportGlobal("gFrameCells", fe.b.m.GlobalI32(int32(fe.nregs+5), false))
	}

	// prologue: nil-fill R(min(nargs,np) .. nregs). The adapter copied
	// actual params into R(0..np-1) (and dropped extras for non-vararg
	// protos, like stock luaD_precall), so the fill starts at min(nargs,
	// np) — covering missing params and unused registers alike.
	f.LocalGet(2).I32Const(int32(fe.np)).I32LtS().If(wasm.Void)
	f.LocalGet(2).LocalSet(fe.lT0)
	f.Else()
	f.I32Const(int32(fe.np)).LocalSet(fe.lT0)
	f.End()
	f.Block(wasm.Void)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lT0).I32Const(int32(fe.nregs)).I32GeS().BrIf(1)
	f.LocalGet(0).LocalGet(fe.lT0).I32Const(16).I32Mul().I32Add().
		I32Const(0).I32Store(8) // tag = nil (full 4 bytes — see setBoolCellA)
	f.LocalGet(fe.lT0).I32Const(1).I32Add().LocalSet(fe.lT0)
	f.Br(0)
	f.End()
	f.End()

	// vararg prologue: the 5.0-compat `arg` table at R(np) when the
	// compiler reserved the hidden local (IsVarArg & VarArgNeedsArg,
	// cleared on `...` use — compile.go:1188; never a local for main —
	// compile.go:1326, and unobservable there with nargs=0). The table
	// overwrites the nil the fill just left at R(np).
	if fe.proto != fe.b.main && fe.proto.IsVarArg&lua.VarArgNeedsArg != 0 {
		f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().LocalSet(fe.lT0)
		f.LocalGet(fe.lT0).I32Const(0).I32LtS().If(wasm.Void)
		f.I32Const(0).LocalSet(fe.lT0)
		f.End()
		fe.cellAddr(fe.np)
		f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 4))).I32Add() // varargBase
		f.LocalGet(fe.lT0)
		f.Call(fe.b.imp("rt_compat_arg"))
		fe.checkStatus()
	}
	f.I32Const(int32(fe.nregs)).LocalSet(fe.lTop)

	fe.partitionBlocks()

	// flattened dispatch. N+1 blocks: the innermost is a dummy so EVERY
	// body is enclosed by a dispatch block — copy-loops inside a body exit
	// with BrIf(1) to "the rest of this body", which must never resolve to
	// the dispatch LOOP itself (that would re-dispatch with a stale lBlk
	// and loop forever — the {1,2,3} constructor OOM).
	f.Loop(wasm.Void)
	// M6d (D4): the deadline poll — every basic-block transition passes
	// here, so every loop iteration of every loop observes the flag (the
	// interpreter's ctx check is the same per-instruction granularity).
	fe.b.emitDeadlinePoll(f)
	for i := 0; i <= len(fe.blockPC); i++ {
		f.Block(wasm.Void)
	}
	depths := make([]uint32, len(fe.blockPC))
	for i := range depths {
		depths[i] = uint32(i)
	}
	f.LocalGet(fe.lBlk).BrTable(depths, uint32(len(fe.blockPC)-1))
	for k := 0; k < len(fe.blockPC); k++ {
		f.End() // close the dummy (k=0) / dispatch block N-1-k (k>0)
		start := fe.blockPC[k]
		end := len(fe.code)
		if k+1 < len(fe.blockPC) {
			end = fe.blockPC[k+1]
		}
		fe.emitBlockBody(start, end)
		f.Br(uint32(len(fe.blockPC) - k)) // → the dispatch loop (past B0..B(N-1-k))
	}
	f.End() // close B0
	f.End() // close loop
	f.I32Const(0)
	f.End() // unreachable; wasm requires a result
}

func (fe *funcEmitter) partitionBlocks() {
	// map of upvalue-capture pseudo-instructions (the slots after each
	// OP_CLOSURE — see the OP_CLOSURE case below). A jump whose target
	// lands on one (the compiler leaves UNREACHABLE jumps with stale
	// backward targets, e.g. `while false` under a constant-false `if`)
	// must never start a block there: the pseudo would be emitted as real
	// code, clobbering the closure register with an upvalue read (fuzzer
	// finding c0119720: "attempt to call a non-function object").
	fe.markPseudos()
	leaders := map[int]bool{0: true}
	code := fe.code
	for pc := 0; pc < len(code); pc++ {
		inst := code[pc]
		switch int(inst >> 26) {
		case lua.OP_JMP, lua.OP_FORLOOP, lua.OP_FORPREP:
			leaders[fe.realPc(fe.jumpTarget(pc))] = true
			leaders[fe.realPc(pc+1)] = true
		case lua.OP_EQ, lua.OP_LT, lua.OP_LE, lua.OP_TEST, lua.OP_TESTSET, lua.OP_TFORLOOP:
			leaders[fe.realPc(pc+2)] = true
			if pc+1 < len(code) && int(code[pc+1]>>26) == lua.OP_JMP {
				leaders[fe.realPc(fe.jumpTarget(pc+1))] = true
			}
		case lua.OP_RETURN, lua.OP_TAILCALL:
			leaders[fe.realPc(pc+1)] = true
		case lua.OP_LOADBOOL:
			if int(inst>>9)&0x1ff != 0 { // C
				leaders[fe.realPc(pc+2)] = true
			}
		case lua.OP_MOVEN:
			pc += int(inst>>9) & 0x1ff // consume the C fused MOVEs
		case lua.OP_CLOSURE:
			// consume the upvalue-capture pseudo-instructions that follow
			// (OP_MOVE B → capture register B; OP_GETUPVAL B → parent's
			// upvalue B — read at _vm.go:793-803). FunctionProto carries no
			// upvalue descriptors; the backend derives captures from these.
			pc += int(fe.proto.FunctionPrototypes[int(inst&0x3ffff)].NumUpvalues)
		}
	}
	ids := []int{}
	for pc := range leaders {
		if pc < len(code) {
			ids = append(ids, pc)
		}
	}
	sortInts(ids)
	fe.blocks = make(map[int]int, len(ids))
	fe.blockPC = ids
	for id, pc := range ids {
		fe.blocks[pc] = id
	}
}

func sortInts(a []int) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0 && a[j-1] > a[j]; j-- {
			a[j-1], a[j] = a[j], a[j-1]
		}
	}
}

// jumpTarget: the VM increments Pc past the instruction, then pc += sBx.
func (fe *funcEmitter) jumpTarget(pc int) int {
	sbx := int(fe.code[pc]&0x3ffff) - opMaxArgSbx
	return pc + 1 + sbx
}

// markPseudos: flag the upvalue-capture pseudo-instruction slots (the
// NumUpvalues instructions following each OP_CLOSURE).
func (fe *funcEmitter) markPseudos() {
	fe.pseudo = make(map[int]bool)
	for pc := 0; pc < len(fe.code); pc++ {
		if int(fe.code[pc]>>26) == lua.OP_CLOSURE {
			n := int(fe.proto.FunctionPrototypes[int(fe.code[pc]&0x3ffff)].NumUpvalues)
			for k := 1; k <= n && pc+k < len(fe.code); k++ {
				fe.pseudo[pc+k] = true
			}
			pc += n
		}
	}
}

// realPc: advance a pc that may sit on a capture pseudo to the next real
// instruction. Unreachable-but-present jump targets can land there.
func (fe *funcEmitter) realPc(pc int) int {
	for fe.pseudo[pc] && pc < len(fe.code) {
		pc++
	}
	return pc
}

func (fe *funcEmitter) blockOf(pc int) int {
	if id, ok := fe.blocks[pc]; ok {
		return id
	}
	if id, ok := fe.blocks[fe.realPc(pc)]; ok {
		return id
	}
	return len(fe.blockPC) - 1
}

// branchPartner: the successor when a compare/TEST does NOT skip. The
// fork emits these ops with a JMP partner, but the finish pass rewrites
// a jump-to-next partner to OP_NOP — SetOpCode only, operand bits left
// stale (compile.go:1909). A NOP partner executes as nothing on the
// interpreter and falls through to pc+2; decoding the stale bits as
// sBx landed on blockOf's last-block fallback, skipping whole loop
// bodies or silently returning early (row 41, found by the M6e
// fuzzer: `while ("A" or "hello") do … end` never ran its body).
func (fe *funcEmitter) branchPartner(pc int) int {
	if pc+1 < len(fe.code) && int(fe.code[pc+1]>>26) == lua.OP_JMP {
		return fe.blockOf(fe.jumpTarget(pc + 1))
	}
	return fe.blockOf(pc + 2)
}

func (fe *funcEmitter) emitBlockBody(start, end int) {
	code := fe.code
	f := fe.f
	terminal := false
	pc := start
	for pc < end {
		inst := code[pc]
		op := int(inst >> 26)
		A := int(inst>>18) & 0xff
		B := int(inst) & 0x1ff
		C := int(inst>>9) & 0x1ff
		Bx := int(inst) & 0x3ffff
		_ = Bx
		switch op {
		case lua.OP_MOVE:
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.cellAddr(B) })
			fe.bumpTop(A + 1)
		case lua.OP_MOVEN:
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.cellAddr(B) })
			fe.bumpTop(A + 1)
			for i := 0; i < C; i++ {
				pc++
				m := code[pc]
				fe.copyCell(func() { fe.cellAddr(int(m>>18) & 0xff) }, func() { fe.cellAddr(int(m) & 0x1ff) })
				fe.bumpTop(int(m>>18)&0xff + 1)
			}
		case lua.OP_LOADK:
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.kcell(Bx) })
			fe.bumpTop(A + 1)
		case lua.OP_LOADBOOL:
			fe.setBoolCellA(A, func() { fe.f.I32Const(int32(B)) })
			fe.bumpTop(A + 1)
			if C != 0 {
				fe.setBlk(fe.blockOf(pc + 2))
				terminal = true
			}
		case lua.OP_LOADNIL:
			for k := A; k <= B; k++ {
				fe.cellAddr(k)
				fe.f.I32Const(0).I32Store(8)
			}
			fe.bumpTop(B + 1)
		case lua.OP_GETGLOBAL:
			fe.cellAddr(A)
			fe.kscell(Bx) // the interpreter keys globals from stringConstants
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_getglobal"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SETGLOBAL:
			fe.kscell(Bx)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_setglobal"))
			fe.checkStatus()
		case lua.OP_GETTABLE:
			// rt_gettable(tblcell, keycell, dstcell, line) — table first
			fe.cellAddr(B)
			fe.rkAddr(C)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_GETTABLEKS:
			// rt_gettable(tblcell, keycell, dstcell, line) — table first
			fe.cellAddr(B)
			fe.rksAddr(C) // rkString semantics: K bit → stringConstants, else register
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SETTABLE:
			fe.cellAddr(A)
			fe.rkAddr(B)
			fe.rkAddr(C)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_SETTABLEKS:
			fe.cellAddr(A)
			fe.rksAddr(B) // rkString semantics
			fe.rkAddr(C)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_NEWTABLE:
			fe.cellAddr(A)
			fe.f.I32Const(0).I32Const(0)
			fe.f.Call(fe.b.imp("rt_newtable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SELF:
			fe.copyCell(func() { fe.cellAddr(A + 1) }, func() { fe.cellAddr(B) })
			fe.cellAddr(B)
			fe.rkAddr(C)
			fe.scratchAddr(0)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.scratchAddr(0) })
			fe.bumpTop(A + 2)
		case lua.OP_ADD, lua.OP_SUB, lua.OP_MUL, lua.OP_DIV, lua.OP_MOD, lua.OP_POW:
			fe.emitArith(op, A, B, C, pc)
		case lua.OP_UNM:
			fe.emitUnm(A, B, pc)
		case lua.OP_NOT:
			// not = truth inverted
			fe.setBoolCellA(A, func() { fe.truth(B) })
			fe.bumpTop(A + 1)
		case lua.OP_LEN:
			fe.cellAddr(B)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_len"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_CONCAT:
			fe.cellAddr(B) // cells base
			fe.f.I32Const(int32(C - B + 1))
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_concat"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_JMP:
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
			terminal = true
		case lua.OP_EQ, lua.OP_LT, lua.OP_LE:
			fe.emitCompare(op, A, B, C, pc)
			terminal = true
		case lua.OP_TEST:
			// if truth(R(A)) == C, the following JMP executes
			fe.truth(A)
			fe.f.LocalSet(fe.lT0)
			fe.f.LocalGet(fe.lT0).I32Const(int32(C)).I32Eq().If(wasm.Void)
			// falsiness == C ⟺ truthiness == (C==0): pc++ (skip the partner)
			fe.setBlk(fe.blockOf(pc + 2))
			fe.f.Else()
			// the partner runs → its target (a NOP partner → pc+2; row 41)
			fe.setBlk(fe.branchPartner(pc))
			fe.f.End()
			terminal = true
		case lua.OP_TESTSET:
			fe.truth(B)
			fe.f.LocalSet(fe.lT0)
			fe.f.LocalGet(fe.lT0).I32Const(int32(C)).I32Ne().If(wasm.Void)
			// falsiness != C ⟺ truthiness != (C==0): R(A) := R(B), partner runs
			fe.copyCell(func() { fe.cellAddr(A) }, func() { fe.cellAddr(B) })
			fe.bumpTop(A + 1)
			fe.setBlk(fe.branchPartner(pc))
			fe.f.Else()
			// falsiness == C: pc++ (skip the partner) → pc+2
			fe.setBlk(fe.blockOf(pc + 2))
			fe.f.End()
			terminal = true
		case lua.OP_CALL:
			fe.emitCall(A, B, C, pc, false)
		case lua.OP_TAILCALL:
			fe.emitCall(A, B, 0, pc, true)
			terminal = true
		case lua.OP_RETURN:
			fe.emitReturn(A, B)
			terminal = true
		case lua.OP_FORLOOP:
			fe.emitForloop(A, pc)
			terminal = true
		case lua.OP_FORPREP:
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_forprep"))
			fe.checkStatus()
			// init -= step
			fe.cellAddr(A)
			fe.cellAddr(A).F64Load(0)
			fe.cellAddr(A + 2).F64Load(0)
			fe.f.F64Sub().F64Store(0)
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
			terminal = true
		case lua.OP_TFORLOOP:
			fe.emitTForloop(A, C, pc)
			terminal = true
		case lua.OP_SETLIST:
			fe.emitSetlist(A, B, C, pc)
		case lua.OP_VARARG:
			fe.emitVararg(A, B)
		case lua.OP_CLOSURE:
			// R(A) := closure over child proto Bx. The capture
			// pseudo-instructions that follow (OP_MOVE/OP_GETUPVAL) were
			// registered at init (rt_wasm_upval scan) — consume them.
			child := fe.proto.FunctionPrototypes[Bx]
			ln := fe.line(pc)
			pc += int(child.NumUpvalues)
			fe.cellAddr(A)
			f.I32Const(int32(fe.b.protoIdx[child]))
			f.LocalGet(1) // cl — this closure is the parent
			f.LocalGet(0) // frame — capture addresses are frame+16*k
			f.I32Const(ln)
			f.Call(fe.b.imp("rt_newclosure"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_GETUPVAL:
			// R(A) := upvalue B of the running closure
			f.LocalGet(1)
			f.I32Const(int32(B))
			fe.cellAddr(A)
			f.Call(fe.b.imp("rt_getupval"))
			fe.bumpTop(A + 1)
		case lua.OP_SETUPVAL:
			// upvalue B := R(A) (write-through: open → the frame cell)
			f.LocalGet(1)
			f.I32Const(int32(B))
			fe.cellAddr(A)
			f.Call(fe.b.imp("rt_setupval"))
		case lua.OP_CLOSE:
			// close open upvalues at or above frame+16*A (the interpreter
			// closes at exactly TAILCALL/RETURN/CLOSE — _vm.go:610,659,780)
			f.LocalGet(0).I32Const(int32(cellSize * A)).I32Add()
			f.Call(fe.b.imp("rt_close_upvals"))
		case lua.OP_NOP:
		default:
			panic(fmt.Sprintf("luawasm: unhandled opcode %d", op))
		}
		pc++
		if terminal {
			// the instruction routed all successors itself (JMP, branches,
			// TFORLOOP's pseudo-JMP, returns) — emitting anything after it
			// would overwrite lBlk at runtime
			break
		}
	}
	if !terminal {
		fe.setBlk(fe.blockOf(end))
	}
}
