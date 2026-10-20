package luawasm

// The function emitter: lowers one FunctionProto's bytecode to a wasm
// function with flattened control flow (design doc §4.3/§4.4). Registers
// are TValue cells at frame+16*k; the frame is allocated and arg-filled
// by the CClosure trampoline (rt_wasm_push_frame) — params at R(0..np-1),
// varargs above the register window at frame+16*framecells.
//
// Signature (ABI v4): (frame, cl, nargs, want) -> nret — the common
// convention every protoFn and lua_dispatch share. nret >= 0: that many
// results staged at frame+0..; -1: error, exact TValue staged in the
// runtime; -2: tailcall sentinel (M5c). `want` is honored C-side: the
// trampoline copies nret results to the stack top and luaD_poscall
// trims to the CallInfo's nresults, so emitted code returns everything.
//
// All decode goes through the exported lua.OpGet* accessors (U2c's
// decode API); nothing re-implements the 5.5 bit layout here.

import (
	"fmt"
	"sort"

	lua "github.com/pschlump/glasm"
	"github.com/pschlump/glasm/wasm"
)

// multretHeadroomCells (row 92): call results stage at R(A).. with a
// count only the callee knows (want=-1 multret) — stock grows the Lua
// stack there, the wasm window is fixed. Every frame region therefore
// carries this many spare cells ABOVE the window: the main block through
// gFrameCells (below) and the rt's bump chunks through RT_HEADROOM_CELLS
// in runtime/rt_abi55.c — the twin constant; keep them equal. limits01's
// 254-wide select('#', loaded()) is the covering corpus case; a call
// returning more than this past the window is memory corruption until
// growable windows exist.
const multretHeadroomCells = 4096

// scInt decodes an sC immediate (opcode.go keeps int2sC/sC2int
// unexported; the bias is the stable stock encoding OFFSET_sC = 127).
func scInt(v int) int { return v - 127 }

type funcEmitter struct {
	b      *backend
	f      *wasm.Function
	proto  *lua.FunctionProto
	pi     protoInfo
	nregs  int
	np     int
	isMain bool
	code   []uint32

	// local indices (params 0..3 = frame, cl, nargs, want)
	lTop, lBlk, lSt, lT0, lT1 uint32 // i32
	lVlo, lVhi, lV2           uint32 // i64 (copy + shift temporaries)
	lF0                       uint32 // f64
	lDd0, lDd1                uint32 // i32 (U9c: direct dispatch — saved cursor, callee frame)

	blocks  map[int]int  // pc -> block id
	blockPC []int        // block id -> start pc
	pseudo  map[int]bool // pc is an MMBIN*/EXTRAARG skip slot (never real code)
}

func (b *backend) newFuncEmitter(p *lua.FunctionProto) *funcEmitter {
	f := b.m.NewFunction([]wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32}, []wasm.ValueType{wasm.I32})
	f.Local(wasm.I32).Local(wasm.I32).Local(wasm.I32).Local(wasm.I32).Local(wasm.I32) // 4..8
	f.Local(wasm.I64).Local(wasm.I64).Local(wasm.I64)                                 // 9,10,11
	f.Local(wasm.F64)                                                                 // 12
	f.Local(wasm.I32).Local(wasm.I32)                                                 // 13,14 (U9c)
	return &funcEmitter{
		b: b, f: f, proto: p,
		pi:     b.protoInfoOf(p),
		nregs:  int(p.NumUsedRegisters),
		np:     int(p.NumParameters),
		isMain: p == b.main,
		code:   p.Code,
		lTop:   4, lBlk: 5, lSt: 6, lT0: 7, lT1: 8,
		lVlo: 9, lVhi: 10, lV2: 11, lF0: 12, lDd0: 13, lDd1: 14,
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

// hint wrappers (U5e, row 71): the varinfo-hint registry indexes for
// this proto's raise sites — the backend's precomputed interp answers.
func (fe *funcEmitter) hintSite(lastpc, reg int) int32 {
	return fe.b.hintSite(fe.pi.proto, lastpc, reg)
}

func (fe *funcEmitter) hintUpv(uv int) int32 {
	return fe.b.hintUpv(fe.pi.proto, uv)
}

func (fe *funcEmitter) hintCallSite(pc int) int32 {
	return fe.b.hintCallSite(fe.pi.proto, pc)
}

func (fe *funcEmitter) hintRun(pc, base, count int) int32 {
	return fe.b.hintRun(fe.pi.proto, pc, base, count)
}

// static scratch cell k (k=0,1): frame+16*(nregs+2+k) — per-instruction
// lowering temporaries (compare results, materialized immediates, env
// cells, SETLIST keys). Safe because every such use lives entirely
// within one instruction's lowering, and the only writes that can reach
// past nregs come from OTHER instructions' dynamic multret forms.
func (fe *funcEmitter) scratchAddr(k int) *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 2 + k))).I32Add()
}

// nvOff: pushes 16*max(0, nargs-np) for hidden-vararg protos — the
// byte distance from the window base DOWN to the extras' first cell
// (and to the block base). U6a (row 86(B)): the extras sit BELOW the
// register window — block = [extras nv][window framecells][1 spare] —
// so dynamic multret destinations reaching past the window top land in
// the spare/virgin chunk space, never on the extras (vgap01/var07/
// limits01, row 61(d)). Zero for non-vararg protos (extras are dropped
// at push, stock luaD_precall).
func (fe *funcEmitter) nvOff() *wasm.Function {
	f := fe.f
	if fe.proto.IsVarArg&lua.VarArgAny == 0 {
		return f.I32Const(0)
	}
	// max(0, nargs - np) * 16 — Select keeps live locals untouched
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().I32Const(16).I32Mul()
	f.I32Const(0)
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().I32Const(0).I32GtS()
	f.Select()
	return f
}

// constant cell: i in Constants (the single global pool)
func (fe *funcEmitter) kcell(i int) *wasm.Function {
	return fe.f.GlobalGet(fe.b.gKCells).I32Const(int32(cellSize * (fe.pi.koff + i))).I32Add()
}

// copyCell: 16-byte TValue copy (dst, src are address emitters)
func (fe *funcEmitter) copyCell(dst, src func() *wasm.Function) {
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

func (fe *funcEmitter) frameDynCell() *wasm.Function {
	return fe.f.LocalGet(0).LocalGet(fe.lT1).I32Const(16).I32Mul().I32Add()
}

// vararg cell j: the extras' j-th cell — BELOW the register window
// since the U6a layout (rt_wasm_push_frame's split): frame - 16*nv + 16*j.
// Like varargCellDyn, the base subtraction folds before the index term —
// frame-(nvOff-16j) would read BELOW the block for j>0 (var12's nil b).
func (fe *funcEmitter) varargCell(j int) *wasm.Function {
	f := fe.f
	f.LocalGet(0)
	fe.nvOff()
	f.I32Sub()
	f.I32Const(int32(cellSize * j)).I32Add()
	return f
}

// vararg cell indexed by lT1 (dynamic copy loops): frame - 16*nv + 16*lT1.
// The subtraction folds BEFORE the index term — (frame-nvOff) + 16*lT1 —
// matching varargCell(j); the natural spelling frame-(nvOff-16*lT1) reads
// the spare cells above the window (var00's nil flush).
func (fe *funcEmitter) varargCellDyn() *wasm.Function {
	f := fe.f
	f.LocalGet(0)
	fe.nvOff()
	f.I32Sub()
	f.LocalGet(fe.lT1).I32Const(16).I32Mul()
	return f.I32Add()
}

// varargBase: the extras' first cell — rt_wasm_push_frame staged the
// call's extra arguments below the window (U5e's rt_varargtab/
// rt_getvarg src/vabase operand; U6a's below-window placement). For
// non-vararg protos this is the frame itself = the BLOCK base, which
// is what rt_tail_stage's cursor operand needs.
func (fe *funcEmitter) varargBase() *wasm.Function {
	f := fe.f
	f.LocalGet(0)
	fe.nvOff()
	return f.I32Sub()
}

// pushNextra: max(0, nargs - np) — the live extra count (the same
// Select idiom as nvOff, as a count not a byte offset)
func (fe *funcEmitter) pushNextra() {
	f := fe.f
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub()
	f.I32Const(0)
	f.LocalGet(2).I32Const(int32(fe.np)).I32Sub().I32Const(0).I32GtS()
	f.Select()
}

// bumpTop: if top < n { top = n } (interpreter registry semantics)
func (fe *funcEmitter) bumpTop(n int) {
	f := fe.f
	f.LocalGet(fe.lTop).I32Const(int32(n)).I32LtS().
		If(wasm.Void).
		I32Const(int32(n)).LocalSet(fe.lTop).
		End()
}

// truth: leaves 1/0 on the stack — Lua truthiness of register k. v4
// variant tags: falsy iff tt==VNIL(0) or tt==VFALSE(1) — distinct tags,
// unlike v3's shared TBOOLEAN. Byte-wide tag loads (see tagIs): a
// C-written false would read as a nonzero tag word otherwise — truthy.
func (fe *funcEmitter) truth(reg int) {
	f := fe.f
	fe.cellAddr(reg)
	f.I32Load8U(8)
	f.I32Const(rtTagNil)
	f.I32Ne()
	fe.cellAddr(reg)
	f.I32Load8U(8)
	f.I32Const(rtTagFalse)
	f.I32Ne()
	f.I32And()
}

// setBoolCellA: R(A) := boolean; cond is an address-less emitter leaving
// the 0/1 i32 on the stack. Value word 0/1 and tt 1/17 (VFALSE/VTRUE),
// both full-width stores (row 45: recycled-cell tag garbage).
func (fe *funcEmitter) setBoolCellA(a int, cond func()) {
	f := fe.f
	cond()
	f.LocalSet(fe.lT0)
	fe.cellAddr(a)
	f.LocalGet(fe.lT0)
	f.I32Store(0)
	fe.cellAddr(a)
	f.LocalGet(fe.lT0).I32Const(16).I32Mul().I32Const(rtTagFalse).I32Add().I32Store(8)
}

// setIntCell: R(r) := int64 (value + VNUMINT tag, full 4-byte tag store)
func (fe *funcEmitter) setIntCell(r int, val func()) {
	f := fe.f
	val()
	f.LocalSet(fe.lV2)
	fe.cellAddr(r)
	f.LocalGet(fe.lV2).I64Store(0)
	fe.cellAddr(r)
	f.I32Const(rtTagInt).I32Store(8)
}

// checkStatus: st := <status on stack>; if st == RT_ERR return -1 (the
// ABI v4 error convention — -1 is unambiguous with "1 result")
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

// lineCell: the spare cell inside frameCells (nregs+4) carries the
// function's CURRENT source line for the row-60 seam (U5e). Its value
// word (offset 0) is the line; the tag word stays untouched (the cell
// is never a TValue).
func (fe *funcEmitter) lineCellAddr() *wasm.Function {
	return fe.f.LocalGet(0).I32Const(int32(cellSize * (fe.nregs + 4))).I32Add()
}

// storeLine: publish pc's source line into the line cell — emitted
// first for EVERY real instruction, so any rt_* dispatch (call,
// metamethod, close, return) resolves error()'s level target to the
// line of the instruction in flight (the seam's luaL_where reads the
// cell through the shadow stack).
func (fe *funcEmitter) storeLine(pc int) {
	fe.lineCellAddr()
	fe.f.I32Const(fe.line(pc)).I32Store(0)
}

/* ---------- body ---------- */

func (fe *funcEmitter) emitBody() {
	f := fe.f

	// gFrameCells (main proto only): the TOTAL cell count the host must
	// allocate for lua_main's frame — the registered window plus the one
	// dynamic spare above it (main is vararg with nargs=0, so the spare
	// sits directly at framecells) plus multretHeadroomCells: the hosts
	// malloc EXACTLY this many bytes (wasmengine/wazeroengine/vm.go), and
	// a tight block made limits01's 254-wide multret staging run off its
	// end into the malloc heap (row 92).
	if fe.isMain {
		fe.b.m.ExportGlobal("gFrameCells", fe.b.m.GlobalI32(int32(frameCells(fe.nregs)+1+multretHeadroomCells), false))
	}

	// prologue: bind this frame's line cell into the CURRENT shadow
	// entry (row 60 seam, U5e). The trampoline pushed the entry; its
	// linecell pointer is NULL until here, so no C code running between
	// the push and this call can see a stale line (none runs). The
	// M5c restage re-dispatches a successor proto at the same entry —
	// this call rebinds it to the successor's cell.
	fe.lineCellAddr()
	f.Call(fe.b.imp("rt_sea_setline"))

	// nil-fill R(min(nargs,np) .. nregs). The trampoline copied
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
		I32Const(0).I32Store(8) // tag = nil (full 4 bytes — row 45)
	f.LocalGet(fe.lT0).I32Const(1).I32Add().LocalSet(fe.lT0)
	f.Br(0)
	f.End()
	f.End()

	f.I32Const(int32(fe.nregs)).LocalSet(fe.lTop)

	fe.partitionBlocks()

	// flattened dispatch. N+1 blocks: the innermost is a dummy so EVERY
	// body is enclosed by a dispatch block — copy-loops inside a body exit
	// with BrIf(1) to "the rest of this body", which must never resolve to
	// the dispatch LOOP itself (that would re-dispatch with a stale lBlk
	// and loop forever — the {1,2,3} constructor OOM).
	f.Loop(wasm.Void)
	// U6b (D4/row 37): the deadline poll — every basic-block transition
	// passes here, so every loop iteration of every loop observes the
	// flag (calls do not split blocks: a pcall-catch and the straight-
	// line statements of its own block complete without re-raising; the
	// next back edge re-raises — the v3-validated granularity).
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

// arithOps: the opcode families whose emissions pair with an MMBIN*
// fallback slot (compile.go emits one after every binary arith op;
// UNM/BNOT/NOT/LEN are self-contained).
var arithOps = map[int]bool{
	lua.OP_ADDI: true, lua.OP_ADDK: true, lua.OP_SUBK: true, lua.OP_MULK: true,
	lua.OP_MODK: true, lua.OP_POWK: true, lua.OP_DIVK: true, lua.OP_IDIVK: true,
	lua.OP_BANDK: true, lua.OP_BORK: true, lua.OP_BXORK: true,
	lua.OP_SHLI: true, lua.OP_SHRI: true,
	lua.OP_ADD: true, lua.OP_SUB: true, lua.OP_MUL: true, lua.OP_MOD: true,
	lua.OP_POW: true, lua.OP_DIV: true, lua.OP_IDIV: true, lua.OP_BAND: true,
	lua.OP_BOR: true, lua.OP_BXOR: true, lua.OP_SHL: true, lua.OP_SHR: true,
}

func isMMBIN(op int) bool {
	return op == lua.OP_MMBIN || op == lua.OP_MMBINI || op == lua.OP_MMBINK
}

// compareTests: the condJump family — each conditionally skips the JMP
// word that follows it (compile.go guarantees the partner).
func isCompareTest(op int) bool {
	switch op {
	case lua.OP_EQ, lua.OP_LT, lua.OP_LE, lua.OP_EQK, lua.OP_EQI,
		lua.OP_LTI, lua.OP_LEI, lua.OP_GTI, lua.OP_GEI,
		lua.OP_TEST, lua.OP_TESTSET:
		return true
	}
	return false
}

func (fe *funcEmitter) partitionBlocks() {
	fe.markPseudos()
	leaders := map[int]bool{0: true}
	code := fe.code
	for pc := 0; pc < len(code); pc++ {
		inst := code[pc]
		op := lua.OpGetOpCode(inst)
		switch op {
		case lua.OP_JMP:
			leaders[fe.realPc(fe.jumpTarget(pc))] = true
			leaders[fe.realPc(pc+1)] = true
		case lua.OP_FORLOOP:
			// unsigned Bx back-edge to the loop body
			leaders[fe.realPc(fe.forloopTarget(pc))] = true
			leaders[fe.realPc(pc+1)] = true
		case lua.OP_FORPREP:
			leaders[fe.realPc(fe.forprepSkipTarget(pc))] = true // never runs: one past FORLOOP
			leaders[fe.realPc(pc+1)] = true                     // body entry
		case lua.OP_TFORPREP:
			leaders[fe.realPc(fe.tforprepTarget(pc))] = true // the TFORCALL (always jumps)
			leaders[fe.realPc(pc+1)] = true                  // body entry (TFORLOOP's back target)
		case lua.OP_TFORLOOP:
			// TFORCALL+TFORLOOP lower as one fused unit (the emitter walks
			// the pair at the TFORCALL) — the TFORLOOP word itself is never
			// a leader. Its back target is the body; its fall-through the
			// loop exit.
			leaders[fe.realPc(pc+1)] = true
			leaders[fe.realPc(fe.forloopTarget(pc))] = true
		case lua.OP_TAILCALL:
			// NOT terminal: the RETURN B=0 sentinel that follows is the
			// continuation for declined tailcalls (C functions, __call
			// objects) — it stages the results
			leaders[fe.realPc(pc+1)] = true
		case lua.OP_RETURN, lua.OP_RETURN0, lua.OP_RETURN1:
			leaders[fe.realPc(pc+1)] = true
		default:
			if isCompareTest(op) || op == lua.OP_LFALSESKIP {
				// conditionally skips the JMP partner word at pc+1
				leaders[fe.realPc(pc+2)] = true
				if pc+1 < len(code) && lua.OpGetOpCode(code[pc+1]) == lua.OP_JMP {
					leaders[fe.realPc(fe.jumpTarget(pc+1))] = true
				}
			}
		}
	}
	ids := []int{}
	for pc := range leaders {
		if pc < len(code) {
			ids = append(ids, pc)
		}
	}
	sort.Ints(ids)
	fe.blocks = make(map[int]int, len(ids))
	fe.blockPC = ids
	for id, pc := range ids {
		fe.blocks[pc] = id
	}
}

// markPseudos: flag the skip-slot instructions — the MMBIN* fallback
// slot after every binary arith op and the EXTRAARG word after
// LOADKX/NEWTABLE(k)/SETLIST(k). They are never emitted as real code
// and never branch targets (the compiler pairs them atomically with
// their primary; defensive realPc still advances past stale targets).
func (fe *funcEmitter) markPseudos() {
	fe.pseudo = make(map[int]bool)
	code := fe.code
	for pc := 0; pc < len(code); pc++ {
		op := lua.OpGetOpCode(code[pc])
		skip := 0
		if arithOps[op] {
			skip = 1
		} else if op == lua.OP_LOADKX || op == lua.OP_NEWTABLE ||
			(op == lua.OP_SETLIST && lua.OpGetArgk(code[pc]) != 0) {
			skip = 1
		}
		for k := 1; k <= skip && pc+k < len(code); k++ {
			fe.pseudo[pc+k] = true
		}
	}
}

// jump targets. The VM post-increments Pc before applying the offset:
// JMP sJ → pc+1+sJ; FORLOOP/TFORLOOP back-edges → pc+1−Bx; FORPREP's
// skip → pc+Bx+2 (one past the FORLOOP); TFORPREP → pc+1+Bx (the
// TFORCALL).
func (fe *funcEmitter) jumpTarget(pc int) int {
	return pc + 1 + lua.OpGetArgsJ(fe.code[pc])
}

func (fe *funcEmitter) forloopTarget(pc int) int {
	return pc + 1 - lua.OpGetArgBx(fe.code[pc])
}

func (fe *funcEmitter) forprepSkipTarget(pc int) int {
	return pc + lua.OpGetArgBx(fe.code[pc]) + 2
}

func (fe *funcEmitter) tforprepTarget(pc int) int {
	return pc + 1 + lua.OpGetArgBx(fe.code[pc])
}

// realPc: advance a pc that may sit on a skip slot to the next real
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

// branchPartner: the successor when a compare/TEST's condition takes
// its JMP partner — the JMP's target, or pc+2 when the partner word is
// not a JMP (defensive; the compiler always pairs one).
func (fe *funcEmitter) branchPartner(pc int) int {
	if pc+1 < len(fe.code) && lua.OpGetOpCode(fe.code[pc+1]) == lua.OP_JMP {
		return fe.blockOf(fe.jumpTarget(pc + 1))
	}
	return fe.blockOf(pc + 2)
}

func (fe *funcEmitter) emitBlockBody(start, end int) {
	code := fe.code
	terminal := false
	pc := start
	for pc < end {
		inst := code[pc]
		fe.storeLine(pc) // row-60 seam: the in-flight line for where()
		op := lua.OpGetOpCode(inst)
		A := lua.OpGetArgA(inst)
		B := lua.OpGetArgB(inst)
		C := lua.OpGetArgC(inst)
		Bx := lua.OpGetArgBx(inst)
		k := lua.OpGetArgk(inst)
		switch op {
		case lua.OP_MOVE:
			fe.copyCell(fe.cell(A), fe.cell(B))
			fe.bumpTop(A + 1)
		case lua.OP_LOADI:
			fe.setIntCell(A, func() { fe.f.I64Const(int64(lua.OpGetArgSbx(inst))) })
			fe.bumpTop(A + 1)
		case lua.OP_LOADF:
			f := fe.f
			fe.cellAddr(A)
			f.F64Const(float64(lua.OpGetArgSbx(inst)))
			f.F64Store(0)
			fe.cellAddr(A)
			f.I32Const(rtTagFloat).I32Store(8)
			fe.bumpTop(A + 1)
		case lua.OP_LOADK:
			fe.copyCell(fe.cell(A), fe.kcellFn(Bx))
			fe.bumpTop(A + 1)
		case lua.OP_LOADKX:
			// the k index rides the following EXTRAARG word (stock lvm.c)
			kidx := lua.OpGetArgAx(code[pc+1])
			fe.copyCell(fe.cell(A), fe.kcellFn(kidx))
			fe.bumpTop(A + 1)
		case lua.OP_LOADFALSE:
			fe.setBoolCellA(A, func() { fe.f.I32Const(0) })
			fe.bumpTop(A + 1)
		case lua.OP_LFALSESKIP:
			fe.setBoolCellA(A, func() { fe.f.I32Const(0) })
			fe.bumpTop(A + 1)
			fe.setBlk(fe.blockOf(pc + 2))
			terminal = true
		case lua.OP_LOADTRUE:
			fe.setBoolCellA(A, func() { fe.f.I32Const(1) })
			fe.bumpTop(A + 1)
		case lua.OP_LOADNIL:
			for r := A; r <= A+B; r++ {
				fe.cellAddr(r)
				fe.f.I32Const(0).I32Store(8)
			}
			fe.bumpTop(A + B + 1)
		case lua.OP_GETUPVAL:
			if fe.isMain {
				panic("luawasm: GETUPVAL on the main proto (cl=0) is unsupported until U6 — ledger row 53")
			}
			fe.f.LocalGet(1).I32Const(int32(B))
			fe.cellAddr(A)
			fe.f.Call(fe.b.imp("rt_getupval"))
			fe.bumpTop(A + 1)
		case lua.OP_SETUPVAL:
			if fe.isMain {
				panic("luawasm: SETUPVAL on the main proto (cl=0) is unsupported until U6 — ledger row 53")
			}
			fe.f.LocalGet(1).I32Const(int32(B))
			fe.cellAddr(A)
			fe.f.Call(fe.b.imp("rt_setupval"))
		case lua.OP_GETTABUP:
			if fe.isMain {
				// main runs cl=0: its _ENV upvalue IS the globals table
				// (row 53) — lower to the registry read
				fe.cellAddr(A)
				fe.kcell(C)
				fe.f.I32Const(fe.line(pc))
				fe.f.Call(fe.b.imp("rt_getglobal"))
				fe.checkStatus()
			} else {
				fe.f.LocalGet(1).I32Const(int32(B))
				fe.scratchAddr(0)
				fe.f.Call(fe.b.imp("rt_getupval"))
				fe.scratchAddr(0)
				fe.kcell(C)
				fe.cellAddr(A)
				fe.f.I32Const(fe.line(pc))
				// row-71 hints: the object is the upvalue cell B; the
				// __index-not-callable call error names "metamethod 'index'"
				fe.f.I32Const(int32(fe.hintUpv(B)))
				fe.f.I32Const(int32(fe.hintCallSite(pc)))
				fe.f.Call(fe.b.imp("rt_gettable"))
				fe.checkStatus()
			}
			fe.bumpTop(A + 1)
		case lua.OP_SETTABUP:
			if fe.isMain {
				fe.kcell(B)
				fe.vcCell(C, k)
				fe.f.I32Const(fe.line(pc))
				fe.f.Call(fe.b.imp("rt_setglobal"))
				fe.checkStatus()
			} else {
				fe.f.LocalGet(1).I32Const(int32(A))
				fe.scratchAddr(0)
				fe.f.Call(fe.b.imp("rt_getupval"))
				fe.scratchAddr(0)
				fe.kcell(B)
				fe.vcCell(C, k)
				fe.f.I32Const(fe.line(pc))
				fe.f.I32Const(int32(fe.hintUpv(A)))
				fe.f.I32Const(int32(fe.hintCallSite(pc)))
				fe.f.Call(fe.b.imp("rt_settable"))
				fe.checkStatus()
			}
		case lua.OP_GETTABLE:
			fe.cellAddr(B)
			fe.cellAddr(C)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, B)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_GETI:
			fe.mkIntScratch(1, int64(C))
			fe.cellAddr(B)
			fe.scratchAddr(1)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, B)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_GETFIELD:
			fe.cellAddr(B)
			fe.kcell(C)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, B)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SETTABLE:
			fe.cellAddr(A)
			fe.cellAddr(B)
			fe.vcCell(C, k)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, A)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_SETI:
			fe.mkIntScratch(1, int64(B))
			fe.cellAddr(A)
			fe.scratchAddr(1)
			fe.vcCell(C, k)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, A)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_SETFIELD:
			fe.cellAddr(A)
			fe.kcell(B)
			fe.vcCell(C, k)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, A)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_settable"))
			fe.checkStatus()
		case lua.OP_NEWTABLE:
			narr := lua.OpGetArgvC(inst)
			if k != 0 {
				narr += lua.OpGetArgAx(code[pc+1]) * 1024
			}
			nrec := 0
			if vB := lua.OpGetArgvB(inst); vB > 0 {
				nrec = 1 << uint(vB-1)
			}
			fe.cellAddr(A)
			fe.f.I32Const(int32(narr)).I32Const(int32(nrec))
			fe.f.Call(fe.b.imp("rt_newtable"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_SELF:
			// the interp's order (_vm.go OP_SELF): save the object first
			// (A aliases B in the common s:method() shape — writing the
			// method first would clobber it), then R[A] := R[B][K[C]:
			// string] (an __index metamethod observes both R[A] and R[A+1]
			// stale, as on the interp), then R[A+1] := the saved object
			fe.copyCell(fe.scratchAddrCell(0), fe.cell(B))
			fe.cellAddr(B)
			fe.kcell(C)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, B)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_gettable"))
			fe.checkStatus()
			fe.copyCell(fe.cell(A+1), fe.scratchAddrCell(0))
			fe.bumpTop(A + 2)
		case lua.OP_ADDI:
			fe.emitArithI(A, B, C, pc)
		case lua.OP_ADDK, lua.OP_SUBK, lua.OP_MULK, lua.OP_MODK, lua.OP_POWK,
			lua.OP_DIVK, lua.OP_IDIVK, lua.OP_BANDK, lua.OP_BORK, lua.OP_BXORK:
			fe.emitArithK(op, A, B, C, pc)
		case lua.OP_SHLI, lua.OP_SHRI:
			fe.emitShiftI(op, A, B, C, pc)
		case lua.OP_ADD, lua.OP_SUB, lua.OP_MUL, lua.OP_MOD, lua.OP_POW,
			lua.OP_DIV, lua.OP_IDIV, lua.OP_BAND, lua.OP_BOR, lua.OP_BXOR,
			lua.OP_SHL, lua.OP_SHR:
			fe.emitArithReg(op, A, B, C, pc)
		case lua.OP_MMBIN, lua.OP_MMBINI, lua.OP_MMBINK:
			// the arith lowering consumed this slot as its fallback
			panic("luawasm: MMBIN* slot reached as real code (arith pairing bug)")
		case lua.OP_UNM:
			fe.emitUnm(A, B, pc)
		case lua.OP_BNOT:
			fe.emitBnot(A, B, pc)
		case lua.OP_NOT:
			fe.setBoolCellA(A, func() {
				fe.f.I32Const(1)
				fe.truth(B)
				fe.f.I32Sub()
			})
			fe.bumpTop(A + 1)
		case lua.OP_LEN:
			fe.cellAddr(B)
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintSite(pc, B)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_len"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_CONCAT:
			// R[A] := R[A] .. ... .. R[A+B-1] — cells base R(A), count B;
			// the hint run covers every operand (the seam maps the faulty
			// slot back to its index by stack position, row 71)
			fe.cellAddr(A)
			fe.f.I32Const(int32(B))
			fe.cellAddr(A)
			fe.f.I32Const(fe.line(pc))
			fe.f.I32Const(int32(fe.hintRun(pc, A, B)))
			fe.f.I32Const(int32(fe.hintCallSite(pc)))
			fe.f.Call(fe.b.imp("rt_concat"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_TBC:
			// U4f: register the variable's cell in the rt tbc registry
			// (false/nil exempt; a non-closable value raises with the
			// variable's name from the init-registered name table; the
			// line gives the raise its "chunk:line: " position — the
			// interp raises at the OP_TBC's own position)
			fe.cellAddr(A)
			fe.f.I32Const(int32(fe.pi.tbcIdx[pc]))
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_tbc"))
			fe.checkStatus()
		case lua.OP_CLOSE:
			// U4f: the tbc-aware close (upvalues first, then 1-arg
			// __close over the registry entries >= frame+16*A — the
			// interp's OP_CLOSE = closeUpvalues(RA) + closeTBC(RA))
			fe.f.LocalGet(0).I32Const(int32(cellSize * A)).I32Add()
			fe.f.Call(fe.b.imp("rt_close"))
			fe.checkStatus()
		case lua.OP_JMP:
			fe.setBlk(fe.blockOf(fe.jumpTarget(pc)))
			terminal = true
		case lua.OP_EQ, lua.OP_LT, lua.OP_LE, lua.OP_EQK, lua.OP_EQI,
			lua.OP_LTI, lua.OP_LEI, lua.OP_GTI, lua.OP_GEI:
			fe.emitCompare(op, A, B, C, k, pc)
			terminal = true
		case lua.OP_TEST:
			// cond = truthy(R[A]); cond == k → take the partner JMP,
			// else skip it (pc+2)
			fe.truth(A)
			fe.f.LocalSet(fe.lT0)
			fe.f.LocalGet(fe.lT0).I32Const(int32(k)).I32Eq().If(wasm.Void)
			fe.setBlk(fe.branchPartner(pc))
			fe.f.Else()
			fe.setBlk(fe.blockOf(pc + 2))
			fe.f.End()
			terminal = true
		case lua.OP_TESTSET:
			// falsy(R[B]) != k → R[A] := R[B] and take the partner JMP;
			// else skip it (pc+2)
			fe.truth(B)
			fe.f.LocalSet(fe.lT0)
			fe.f.I32Const(1)
			fe.f.LocalGet(fe.lT0)
			fe.f.I32Sub() // falsity
			fe.f.I32Const(int32(k)).I32Ne().If(wasm.Void)
			fe.copyCell(fe.cell(A), fe.cell(B))
			fe.bumpTop(A + 1)
			fe.setBlk(fe.branchPartner(pc))
			fe.f.Else()
			fe.setBlk(fe.blockOf(pc + 2))
			fe.f.End()
			terminal = true
		case lua.OP_CALL:
			fe.emitCall(A, B, C, pc, false)
		case lua.OP_TAILCALL:
			// NOT terminal: the RETURN B=0 sentinel at pc+1 stages the
			// results when the tailcall declines staging (C functions,
			// __call objects — parity with the interpreter's pushed-frame
			// tailcall whose RETURN continuation runs after)
			fe.emitCall(A, B, 0, pc, true)
		case lua.OP_RETURN:
			fe.emitReturn(A, B)
			terminal = true
		case lua.OP_RETURN0:
			fe.emitReturn(0, 1)
			terminal = true
		case lua.OP_RETURN1:
			fe.emitReturn(A, 2)
			terminal = true
		case lua.OP_FORLOOP:
			fe.emitForloop(A, Bx, pc)
			terminal = true
		case lua.OP_FORPREP:
			fe.emitForprep(A, Bx, pc)
			terminal = true
		case lua.OP_TFORPREP:
			// U5e (row 85): swap the control (R[A+3]) and closing
			// (R[A+2]) cells, then register the closing value
			// to-be-closed (the interp's registerTBC(RA+2, A+3) —
			// nil/false exempt inside rt_tbc) — then jump to the TFORCALL
			fe.swapCells(A+2, A+3)
			fe.cellAddr(A + 2)
			fe.f.I32Const(int32(fe.pi.tbcIdx[pc]))
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_tbc"))
			fe.checkStatus()
			fe.setBlk(fe.blockOf(fe.tforprepTarget(pc)))
			terminal = true
		case lua.OP_TFORCALL:
			// fused with the TFORLOOP at pc+1 (they always pair; the
			// interp dispatches them back-to-back the same way)
			fe.emitTForcall(A, C, pc)
			terminal = true
		case lua.OP_TFORLOOP:
			// consumed by the fused TFORCALL lowering — unreachable as a
			// block head (its targets are leaders, so it never starts a
			// block); a lone TFORLOOP means a pairing bug
			panic("luawasm: TFORLOOP reached without its TFORCALL")
		case lua.OP_SETLIST:
			fe.emitSetlist(inst, A, pc)
		case lua.OP_CLOSURE:
			child := fe.proto.FunctionPrototypes[Bx]
			fe.cellAddr(A)
			fe.f.I32Const(int32(fe.b.protoIdx[child]))
			fe.f.LocalGet(1) // cl — this closure is the parent
			fe.f.LocalGet(0) // frame — capture addresses are frame+16*k
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_newclosure"))
			fe.checkStatus()
			fe.bumpTop(A + 1)
		case lua.OP_VARARG:
			fe.emitVararg(A, B, C, k, pc)
		case lua.OP_VARARGPREP:
			// U5e (row 85): table model builds the vararg table into
			// R(np) (stock createvarargtab over the staged extras);
			// hidden model is a no-op — the trampoline already staged
			// the extras at the vararg base and wasm registers are
			// frame-relative, so the interp's param shift has nothing
			// to do
			if fe.proto.IsVarArg&lua.VarArgTable != 0 {
				fe.cellAddr(fe.np)
				fe.varargBase()
				fe.pushNextra()
				fe.f.I32Const(fe.line(pc))
				fe.f.Call(fe.b.imp("rt_varargtab"))
				fe.checkStatus()
				fe.bumpTop(fe.np + 1)
			}
		case lua.OP_EXTRAARG:
			// consumed by its primary (LOADKX/NEWTABLE/SETLIST)
			panic("luawasm: EXTRAARG reached as real code (pairing bug)")
		case lua.OP_GETVARG:
			// U5e (row 85): the hidden model's named `...t[i]` read —
			// never raises (rt_getvarg is plain like rt_getupval):
			// integral key 1..nextra reads the extra, "n" yields nextra,
			// else nil
			fe.cellAddr(A)
			fe.cellAddr(C)
			fe.varargBase()
			fe.pushNextra()
			fe.f.Call(fe.b.imp("rt_getvarg"))
			fe.bumpTop(A + 1)
		case lua.OP_ERRNNIL:
			// U5e (row 85): a global declaration whose register is
			// already non-nil raises (stock luaG_errnnil) — name from
			// K[Bx-1] when the compiler recorded a string constant
			fe.tagIs(A, rtTagNil).I32Eqz().If(wasm.Void)
			fe.cellAddr(A)
			if Bx > 0 && Bx-1 < len(fe.proto.Constants) {
				if _, ok := fe.proto.Constants[Bx-1].(lua.LString); ok {
					fe.kcell(Bx - 1)
				} else {
					fe.f.I32Const(0)
				}
			} else {
				fe.f.I32Const(0)
			}
			fe.f.I32Const(fe.line(pc))
			fe.f.Call(fe.b.imp("rt_errnnil"))
			fe.checkStatus()
			fe.f.End()
		default:
			panic(fmt.Sprintf("luawasm: unhandled opcode %d", op))
		}
		pc++
		// skip-slot instructions (MMBIN* fallbacks, EXTRAARG words) are
		// never real code — the arith lowering already folded the
		// fallback into its miss path; the primaries consumed their
		// EXTRAARG words inline
		for pc < len(fe.code) && fe.pseudo[pc] {
			pc++
		}
		if terminal {
			// the instruction routed all successors itself (jumps,
			// branches, returns) — emitting anything after it would
			// overwrite lBlk at runtime
			break
		}
	}
	if !terminal {
		fe.setBlk(fe.blockOf(end))
	}
}

// vcCell: the C operand of a SET-family op — a K cell when k is set
// (the value was a folded constant), the register otherwise
func (fe *funcEmitter) vcCell(C, k int) *wasm.Function {
	if k != 0 {
		return fe.kcell(C)
	}
	return fe.cellAddr(C)
}

// mkIntScratch: materialize an integer immediate into static scratch k
func (fe *funcEmitter) mkIntScratch(k int, v int64) {
	fe.scratchAddr(k)
	fe.f.I64Const(v)
	fe.f.Call(fe.b.imp("rt_mkinteger"))
}

// mkFloatScratch: materialize a float immediate into static scratch k
func (fe *funcEmitter) mkFloatScratch(k int, v float64) {
	fe.scratchAddr(k)
	fe.f.F64Const(v)
	fe.f.Call(fe.b.imp("rt_mknumber"))
}

// swapCells: exchange two whole TValue cells (one i64 temp per word)
func (fe *funcEmitter) swapCells(x, y int) {
	f := fe.f
	fe.cellAddr(x)
	f.I64Load(0).LocalSet(fe.lVlo)
	fe.cellAddr(x)
	fe.cellAddr(y).I64Load(0)
	f.I64Store(0)
	fe.cellAddr(y)
	f.LocalGet(fe.lVlo).I64Store(0)
	fe.cellAddr(x)
	f.I64Load(8).LocalSet(fe.lVlo)
	fe.cellAddr(x)
	fe.cellAddr(y).I64Load(8)
	f.I64Store(8)
	fe.cellAddr(y)
	f.LocalGet(fe.lVlo).I64Store(8)
}
