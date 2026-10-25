// Package luawasm is the Lua→wasm backend (design doc §4): FunctionProto
// trees from the glasm frontend become wasm modules that share linear
// memory with the C runtime and call down through the rt_* ABI.
//
// U2e rewrite: the backend speaks the stock 5.5.1 instruction set
// (decoded via the exported lua.OpGet* accessors — never re-implemented
// here) and ABI v4 on runtime/lua55_rt.wasm (the zero-patch 5.5 tree's
// rt_abi55). Every proto compiles to a wasm function with the common
// signature (frame, cl, nargs, want) -> nret — registers as TValue
// cells in a shared-memory frame the CClosure trampoline allocates and
// arg-fills (rt_wasm_push_frame) — flattened control flow, every opcode
// lowered via the ABI with inline fast paths for integer/float
// arithmetic, bitwise ops and both for-loop flavors. lua_dispatch
// br_table-dispatches proto idx to its function; the C runtime reaches
// it through the host trampoline (host.wasm_dispatch) — the A2-proven
// reentrancy seam. Calls (OP_CALL/OP_TAILCALL) go through rt_call; a
// wasm-closure callee re-enters compiled code through the CClosure
// seam (rt_wasm_trampoline).
//
// nret convention (ABI v4): >= 0 results at frame+0.., -1 error (the
// exact TValue staged in the runtime; the trampoline re-raises it and
// clears the staging), -2 tailcall sentinel (M5c), -3 host-refused.
package luawasm

import (
	"fmt"
	"strings"

	lua "github.com/pschlump/glasm"
	"github.com/pschlump/glasm/wasm"
)

const (
	cellSize = 16 // sizeof(TValue) on wasm32 (static-asserted in rt_abi55.c)

	// stock 5.5 TValue.tt_ variant tags (rt_abi55.h RT_V*, pinned against
	// lobject.h by static asserts). Tags are ALWAYS written/read as full
	// 4-byte words (row 45: a 1-byte store leaves recycled cell memory
	// with garbage upper tag bytes).
	rtTagNil   = 0  // LUA_VNIL
	rtTagFalse = 1  // LUA_VFALSE (value_.b = 0)
	rtTagInt   = 3  // LUA_VNUMINT (value_.i at +0)
	rtTagTrue  = 17 // LUA_VTRUE (value_.b = 1)
	rtTagFloat = 19 // LUA_VNUMFLT (value_.n at +0)
)

// rtOpOfTM: an MMBIN* slot's C operand is a tag-method number; rt_arith
// takes the LUA_OP* (= RT_OP_*) code (tmAdd = 6, ltm.h order — the same
// mapping _vm.go's tmToArithOp applies before opcode dispatch).
func rtOpOfTM(tm int) int32 { return int32(tm - 6) }

// protoInfo: one proto's slot in the module (dispatch index = order).
type protoInfo struct {
	proto *lua.FunctionProto
	koff  int // global constant-pool index of Constants[0]
	nc    int // len(Constants)
	// tbcIdx maps an OP_TBC's pc to its name-table index (U4f; nil when
	// the proto has no to-be-closed sites)
	tbcIdx map[int]int
}

// Compile emits a wasm module for the FunctionProto tree rooted at main.
func Compile(main *lua.FunctionProto, chunkName string) ([]byte, error) {
	protos := collectProtos(main)

	// the C runtime renders the chunk name verbatim in error positions
	// (ldebug reads the proto source raw), so strip the '@' sigil the Go
	// callers carry (U1 ChunkID convention) before registering it
	b := &backend{m: wasm.NewModule(), main: main, chunkName: strings.TrimPrefix(chunkName, "@")}
	b.layoutProtos(protos)
	b.collectTBCNames()
	b.declareImports()
	// U9c (row 104): reserve lua_dispatch's function index BEFORE the
	// bodies — the direct-dispatch arm inside emitCall/emitTForcall calls
	// it, and the index must already be fixed while the protos are being
	// lowered (the body itself is written later, in emitDispatch below —
	// NewFunction reserves the slot; the builder writes into the pointer)
	b.dispatchF = b.m.NewFunction([]wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32}, []wasm.ValueType{wasm.I32})
	b.dispatchFn = b.dispatchF.Idx()
	for _, pi := range b.protos {
		fe := b.newFuncEmitter(pi.proto)
		fe.emitBody()
		b.protoFuncs = append(b.protoFuncs, fe.f)
	}
	// U5e: emitInit runs AFTER the bodies — the varinfo hint registry is
	// collected while lowering (the same GetObjName walk the interp
	// resolves lazily at raise time, precomputed per raise site here)
	b.emitInit()
	b.emitDispatch()
	b.emitMain()
	return b.m.Encode(), nil
}

func collectProtos(p *lua.FunctionProto) []*lua.FunctionProto {
	out := []*lua.FunctionProto{p}
	for _, c := range p.FunctionPrototypes {
		out = append(out, collectProtos(c)...)
	}
	return out
}

type backend struct {
	m                *wasm.Module
	main             *lua.FunctionProto
	chunkName        string
	imports          map[string]uint32
	gKCells          uint32 // mutable global: constants cells base
	gDeadline        uint32 // mutable global: the deadline flag's address (U6b)
	gDeadlinePollCtr uint32 // mutable global: the row-100 backstop's back-edge counter (U7a)
	gDispFrame       uint32 // mutable global: lua_dispatch's EXIT frame (U9c, row 104)
	gProtGen         uint32 // mutable global: &g_prot_gen (U12 — the arms' protection screen)
	gNextF           uint32 // mutable global: &pin_next (U14 — the pairs/next arm)
	gIpairF          uint32 // mutable global: &pin_ipairsaux (U14 — the ipairs arm)
	protos           []protoInfo
	protoIdx         map[*lua.FunctionProto]int // proto → dispatch index
	protoFuncs       []*wasm.Function           // dispatch idx → wasm function
	dispatchF        *wasm.Function             // lua_dispatch (shell reserved before the bodies — U9c)
	dispatchFn       uint32                     // lua_dispatch's function index
	// tbcNames (U4f): the OP_TBC variable names, init-registered via
	// rt_name_put in this order — the OP_TBC lowering embeds the index
	tbcNames []string
	// hints (U5e, row 71): the formatted varinfo hint strings the raising
	// rt_* entries reference by index — the interp's own findsetreg/
	// getobjname answers (varinfo.go), precomputed per raise site and
	// registered via rt_hint_put in this order. NO dedup: concat runs
	// need hbase+idx to address contiguous entries, so every site appends
	hints []string
}

// hint appends s to the hint registry and returns its index (the emitted
// code embeds it; -1 means "no hint" and never enters the table).
func (b *backend) hint(s string) int32 {
	b.hints = append(b.hints, s)
	return int32(len(b.hints) - 1)
}

// hintSite: the interp's protoVarinfo answer for register reg at lastpc
// (the raise site's cf.Pc-1) — -1 when the operand carries no name.
func (b *backend) hintSite(p *lua.FunctionProto, lastpc, reg int) int32 {
	kind, name := lua.GetObjName(p, lastpc, reg)
	if kind == "" {
		return -1
	}
	return b.hint(lua.FormatVarInfo(kind, name))
}

// hintUpv: the GETTABUP/SETTABUP object hint — the table operand is an
// upvalue cell (varinfo's getupvalname arm; upvalHint on the interp).
func (b *backend) hintUpv(p *lua.FunctionProto, uv int) int32 {
	return b.hint(lua.FormatVarInfo("upvalue", lua.UpvalName(p, uv)))
}

// hintCallSite: the call-error hint — funcnamefromcode's answer for the
// instruction at pc (the interp's raiseCallError naming; ""-kind — an
// unattributable call site the interp's register VALUE scan would have
// to resolve — is not precomputable, so -1: the wasm raise carries no
// hint, the one known row-71 residual).
func (b *backend) hintCallSite(p *lua.FunctionProto, pc int) int32 {
	kind, name := lua.FuncNameFromCode(p, pc)
	if kind == "" {
		return -1
	}
	return b.hint(lua.FormatVarInfo(kind, name))
}

// hintRun: OP_CONCAT's operand run — registers hints for regs
// [base, base+count) CONTIGUOUSLY (unnamed operands take the empty
// string, so the seam's hbase+idx mapping stays dense) and returns the
// base index.
func (b *backend) hintRun(p *lua.FunctionProto, pc, base, count int) int32 {
	h := int32(-1)
	for r := base; r < base+count; r++ {
		kind, name := lua.GetObjName(p, pc, r)
		if idx := b.hint(lua.FormatVarInfo(kind, name)); h < 0 {
			h = idx
		}
	}
	return h
}

func (b *backend) imp(name string) uint32 { return b.imports[name] }

// layoutProtos assigns dispatch indices (preorder) and one global
// constant pool: each proto's Constants (the 5.5 single-pool encoding —
// ints, floats and strings alike; U2c retired the separate
// StringConstants channel the fork's globals lowering used).
func (b *backend) layoutProtos(protos []*lua.FunctionProto) {
	off := 0
	b.protoIdx = make(map[*lua.FunctionProto]int, len(protos))
	for i, p := range protos {
		pi := protoInfo{proto: p, koff: off, nc: len(p.Constants)}
		b.protos = append(b.protos, pi)
		b.protoIdx[p] = i
		off += pi.nc
	}
}

// collectTBCNames (U4f) walks every proto's code for OP_TBC sites and
// builds the name table the runtime's non-closable raise reads. The interp
// answers the name from the proto's debug locals at run time
// (registerTBC → localNameAt(proto, A+1, pc)); the backend precomputes the
// same answer at compile time and registers the bytes at init, keeping the
// rt side debug-info-free. "?" is the interp's fallback for a missing
// debug name.
func (b *backend) collectTBCNames() {
	for i := range b.protos {
		pi := &b.protos[i]
		for pc, inst := range pi.proto.Code {
			// OP_TFORPREP registers its closing value too (U5e, row 85):
			// after the swap, the closing variable sits at A+2 and its
			// debug name at A+3 — the interp's registerTBC(RA+2, A+3).
			dreg := 0
			switch lua.OpGetOpCode(inst) {
			case lua.OP_TBC:
				dreg = int(lua.OpGetArgA(inst)) + 1
			case lua.OP_TFORPREP:
				dreg = int(lua.OpGetArgA(inst)) + 3
			default:
				continue
			}
			name := "?"
			if nm, ok := lua.LocalNameAt(pi.proto, dreg, pc); ok {
				name = nm
			}
			if pi.tbcIdx == nil {
				pi.tbcIdx = make(map[int]int)
			}
			pi.tbcIdx[pc] = len(b.tbcNames)
			b.tbcNames = append(b.tbcNames, name)
		}
	}
}

// frameCells: the registered per-proto frame window. nregs registers +
// 2 static scratch cells (nregs+2, nregs+3 — per-instruction lowering
// temporaries: compare results, materialized immediates, env cells; the
// new encoding's TFORCALL call window is already inside nregs via
// patchCode's operand extents, but the margin costs nothing) + one
// spare, with the vararg staging area (and the M5b dynamic callee-save
// scratch above it) allocated by rt_wasm_push_frame at frame+16*
// framecells.. — need = 16*(framecells+1+nv) always covers one dynamic
// spare cell.
func frameCells(nregs int) int { return nregs + 5 }

func (b *backend) declareImports() {
	m := b.m
	i32v := []wasm.ValueType{wasm.I32}
	ii := []wasm.ValueType{wasm.I32, wasm.I32}
	iii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32}
	iiii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32}
	iiiii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32}
	iiiiii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32}
	iiiiiiii := []wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32}
	i32f64 := []wasm.ValueType{wasm.I32, wasm.F64}
	i32i64 := []wasm.ValueType{wasm.I32, wasm.I64}
	i64v := []wasm.ValueType{wasm.I64}
	dd := []wasm.ValueType{wasm.F64, wasm.F64}
	f64v := []wasm.ValueType{wasm.F64}

	// ABI v4 (runtime/rt_abi55.h). Dropped vs v3: rt_compat_arg (a
	// 5.1-ism), the dialect layer (U3), and rt_mknil (inline tag store).
	// New: rt_mkinteger (lossless int64 cells — the reason the 5.1 blob
	// could not serve U2) and the deadline block's return at U6b.
	// deadline_poll is the ONE import outside the rt namespace: the host
	// module every engine already defines for the blob serves it too
	// (row 100's STW-proof backstop — see emitDeadlinePoll).
	b.imports = map[string]uint32{
		"rt_newtable": m.ImportFunc("rt", "rt_newtable", iii, i32v),
		"rt_gettable": m.ImportFunc("rt", "rt_gettable", iiiiii, i32v),
		"rt_settable": m.ImportFunc("rt", "rt_settable", iiiiii, i32v),
		// U10 probes (iii, i32v): the unprotected luaV_fastgeti/
		// fastseti arms — a hit skips the try_table enter entirely
		"rt_geti_probe": m.ImportFunc("rt", "rt_geti_probe", iii, i32v),
		"rt_seti_probe": m.ImportFunc("rt", "rt_seti_probe", iii, i32v),
		// U11 probes: short-string keys (same iii shape) and length
		"rt_gets_probe": m.ImportFunc("rt", "rt_gets_probe", iii, i32v),
		"rt_sets_probe": m.ImportFunc("rt", "rt_sets_probe", iii, i32v),
		"rt_len_probe":  m.ImportFunc("rt", "rt_len_probe", ii, i32v),
		// U11b: pure stdlib callee probe (string.byte / string.find's
		// plain search) — (funcaddr, argcells, nargs, want)
		"rt_stdcall_probe": m.ImportFunc("rt", "rt_stdcall_probe", iiii, i32v),
		"rt_arith":         m.ImportFunc("rt", "rt_arith", iiiiiiii, i32v),
		"rt_len":           m.ImportFunc("rt", "rt_len", iiiii, i32v),
		"rt_eq":            m.ImportFunc("rt", "rt_eq", iiii, i32v),
		"rt_lt":            m.ImportFunc("rt", "rt_lt", iiii, i32v),
		"rt_le":            m.ImportFunc("rt", "rt_le", iiii, i32v),
		"rt_concat":        m.ImportFunc("rt", "rt_concat", iiiiii, i32v),
		// U11c: concat fold probe — one-pass, memlimit pre-flighted
		"rt_concat_probe": m.ImportFunc("rt", "rt_concat_probe", iii, i32v),
		// U11e: float MOD/POW as plain calls (luai_nummod/luai_numpow
		// verbatim — cannot raise, so no enter)
		"rt_fmod":          m.ImportFunc("rt", "rt_fmod", dd, f64v),
		"rt_pow":           m.ImportFunc("rt", "rt_pow", dd, f64v),
		"rt_gmatch_probe":  m.ImportFunc("rt", "rt_gmatch_probe", ii, i32v),
		"rt_call":          m.ImportFunc("rt", "rt_call", iiiiii, i32v),
		"rt_call_addr":     m.ImportFunc("rt", "rt_call_addr", iiiiii, i32v),
		"rt_call_count":    m.ImportFunc("rt", "rt_call_count", i32v, i32v),
		"rt_forprep":       m.ImportFunc("rt", "rt_forprep", ii, i32v),
		"rt_frame_alloc":   m.ImportFunc("rt", "rt_frame_alloc", i32v, i32v),
		"rt_getglobal":     m.ImportFunc("rt", "rt_getglobal", iii, i32v),
		"rt_setglobal":     m.ImportFunc("rt", "rt_setglobal", iii, i32v),
		"rt_globals_cell":  m.ImportFunc("rt", "rt_globals_cell", i32v, nil),
		"rt_intern":        m.ImportFunc("rt", "rt_intern", iii, i32v),
		"rt_mknumber":      m.ImportFunc("rt", "rt_mknumber", i32f64, nil),
		"rt_mkinteger":     m.ImportFunc("rt", "rt_mkinteger", i32i64, nil),
		"rt_set_chunkname": m.ImportFunc("rt", "rt_set_chunkname", ii, nil),
		"rt_wasm_proto":    m.ImportFunc("rt", "rt_wasm_proto", iiiii, i32v),
		"rt_wasm_upval":    m.ImportFunc("rt", "rt_wasm_upval", iiii, nil),
		"rt_newclosure":    m.ImportFunc("rt", "rt_newclosure", iiiii, i32v),
		"rt_getupval":      m.ImportFunc("rt", "rt_getupval", iii, nil),
		"rt_setupval":      m.ImportFunc("rt", "rt_setupval", iii, nil),
		"rt_varargtab":     m.ImportFunc("rt", "rt_varargtab", iiii, i32v),
		"rt_getvarg":       m.ImportFunc("rt", "rt_getvarg", iiii, nil),
		"rt_errnnil":       m.ImportFunc("rt", "rt_errnnil", iii, i32v),
		"rt_vararg_read":   m.ImportFunc("rt", "rt_vararg_read", iiiii, i32v),
		"rt_sea_setline":   m.ImportFunc("rt", "rt_sea_setline", i32v, nil),
		"rt_sea_main":      m.ImportFunc("rt", "rt_sea_main", i32v, nil),
		"rt_sea_pop":       m.ImportFunc("rt", "rt_sea_pop", nil, nil),
		"rt_close_upvals":  m.ImportFunc("rt", "rt_close_upvals", i32v, nil),
		"rt_name_put":      m.ImportFunc("rt", "rt_name_put", ii, i32v),
		"rt_hint_put":      m.ImportFunc("rt", "rt_hint_put", ii, i32v),
		"rt_tbc":           m.ImportFunc("rt", "rt_tbc", iii, i32v),
		"rt_close":         m.ImportFunc("rt", "rt_close", i32v, i32v),
		"rt_close_err":     m.ImportFunc("rt", "rt_close_err", i32v, nil),
		"rt_tail_stage":    m.ImportFunc("rt", "rt_tail_stage", iiii, i32v),
		"rt_tail_clidx":    m.ImportFunc("rt", "rt_tail_clidx", nil, i32v),
		"rt_tail_nargs":    m.ImportFunc("rt", "rt_tail_nargs", nil, i32v),
		"rt_tail_funcell":  m.ImportFunc("rt", "rt_tail_funcell", nil, i32v),
		"rt_tail_restage":  m.ImportFunc("rt", "rt_tail_restage", nil, i32v),
		// U9c: the direct arm saves the frame region itself — the first
		// backend consumer of the cursor (imp() is a silent map lookup; a
		// missing name would emit call 0)
		"rt_frame_cursor":   m.ImportFunc("rt", "rt_frame_cursor", nil, i32v),
		"rt_ctrl_addr":      m.ImportFunc("rt", "rt_ctrl_addr", nil, i32v),
		"rt_protgen_addr":   m.ImportFunc("rt", "rt_protgen_addr", nil, i32v),
		"rt_next_pin_addr":  m.ImportFunc("rt", "rt_next_pin_addr", nil, i32v),
		"rt_ipaux_pin_addr": m.ImportFunc("rt", "rt_ipaux_pin_addr", nil, i32v),
		"rt_deadline":       m.ImportFunc("rt", "rt_deadline", nil, i32v),
		// U9c (row 104): the direct-dispatch helpers — a compiled-callee
		// OP_CALL/TFORCALL runs lua_dispatch in-module (probe -> push frame
		// -> wasm call -> retire); everything else declines to rt_call_addr.
		"rt_dd_callee":  m.ImportFunc("rt", "rt_dd_callee", i32v, i64v),
		"rt_dd_enter":   m.ImportFunc("rt", "rt_dd_enter", iiii, i32v),
		"rt_dd_ret":     m.ImportFunc("rt", "rt_dd_ret", iiiiii, i32v),
		"rt_dd_err":     m.ImportFunc("rt", "rt_dd_err", ii, i32v),
		"rt_dd_reset":   m.ImportFunc("rt", "rt_dd_reset", nil, nil),
		"deadline_poll": m.ImportFunc("host", "deadline_poll", nil, i32v),
	}
	b.gKCells = m.GlobalI32(0, true)
	m.ExportGlobal("gKCells", b.gKCells)
	b.gDeadline = m.GlobalI32(0, true)
	b.gDeadlinePollCtr = m.GlobalI32(deadlinePollInterval, true)
	b.gDispFrame = m.GlobalI32(0, true)
	b.gProtGen = m.GlobalI32(0, true)
	b.gNextF = m.GlobalI32(0, true)
	b.gIpairF = m.GlobalI32(0, true)
	m.ImportMemory("rt", "memory", 1, 0)
}

// deadlinePollInterval is the row-100 backstop cadence: one
// host.deadline_poll call per this many basic-block transitions. At the
// dispatch rate of a fully-spinning guest (~350M transitions/s measured)
// the host call lands every ~3ms — negligible call overhead, and a
// wedge-case kill is bounded by one interval past the deadline.
const deadlinePollInterval int32 = 1 << 20

// emitDeadlinePoll (U6b, row 37 — the M6d D4 design, design A5; U7a,
// row 100): load the host-owned deadline flag and, when the host has
// expired the run, raise through rt_deadline — rt_deadline stages the
// error and always returns RT_ERR, so this returns -1 (the ABI v4 error
// convention), unwinding the frame chain exactly like any rt_* error
// (pcall can catch it; the flag stays sticky so the next back edge
// re-raises). Emitted at (i) every proto's flattened dispatch-loop
// header — once per basic-block transition, the v3 granularity: calls do
// NOT split blocks, so a pcall-catch followed by straight-line
// statements inside one block completes without re-raising (the m6d
// catch leg), while any loop's back edge re-polls (the reraise leg) —
// and (ii) lua_dispatch's tail-restage loop (tail-call chains never
// touch a basic block; the rt's own restage loop polls the flag directly
// for the ABI-level chains; every restage hop also re-enters a compiled
// body through this poll, so the counter covers tail chains).
//
// The U7a backstop (row 100, closing row 99's wedge): under a pending GC
// mark-STW the spinning guest is the ONLY goroutine still executing —
// the timer that writes the flag is frozen with the scheduler. So every
// deadlinePollInterval-th transition the guest asks the host directly
// (host.deadline_poll: "has this run expired?"), and an affirmative
// answer takes the same rt_deadline raise as the flag path — identical
// text, staging, and pcall semantics, but delivery that no longer
// depends on the scheduler.
func (b *backend) emitDeadlinePoll(f *wasm.Function) {
	f.GlobalGet(b.gDeadline)
	f.I32Load(0)
	f.If(wasm.Void)
	f.Call(b.imp("rt_deadline")).Drop()
	f.I32Const(-1).Return()
	f.End()
	f.GlobalGet(b.gDeadlinePollCtr)
	f.I32Eqz()
	f.If(wasm.Void)
	f.I32Const(deadlinePollInterval)
	f.GlobalSet(b.gDeadlinePollCtr)
	f.Call(b.imp("deadline_poll"))
	f.If(wasm.Void)
	f.Call(b.imp("rt_deadline")).Drop()
	f.I32Const(-1).Return()
	f.End()
	f.Else()
	f.GlobalGet(b.gDeadlinePollCtr)
	f.I32Const(-1)
	f.I32Add()
	f.GlobalSet(b.gDeadlinePollCtr)
	f.End()
}

// emitInit writes luawasm_init: interns the global constant pool into a
// cells area (base stored in the gKCells global), installs the chunk
// name, and registers every proto with the runtime (rt_wasm_proto — the
// dispatch registry the CClosure trampoline reads). All bytes are
// written with i64 stores into one rt_frame_alloc buffer, so the module
// needs no data segments and cannot collide with the runtime.
func (b *backend) emitInit() {
	m := b.m
	f := m.NewFunction([]wasm.ValueType{wasm.I32}, nil)
	// local 0 is the step PARAM; declared locals follow it
	const lStep uint32 = 0
	const lS, lK, lC uint32 = 1, 2, 3
	f.Local(wasm.I32).Local(wasm.I32).Local(wasm.I32)

	strBytes := len(b.chunkName)
	total := 0
	for _, pi := range b.protos {
		total += pi.nc
		for _, cv := range pi.proto.Constants {
			if s, ok := cv.(lua.LString); ok {
				strBytes += len(s)
			}
		}
	}
	for _, nm := range b.tbcNames { // U4f: the OP_TBC name table
		strBytes += len(nm)
	}
	for _, h := range b.hints { // U5e: the varinfo hint table
		strBytes += len(h)
	}
	f.I32Const(int32(strBytes)).Call(b.imp("rt_frame_alloc")).LocalSet(lS)
	f.I32Const(int32(cellSize * total)).Call(b.imp("rt_frame_alloc")).LocalSet(lK)
	f.LocalGet(lK).GlobalSet(b.gKCells)
	// U6b: cache the deadline control-block address BEFORE the step gate —
	// the poll sites read it on every basic-block transition, and a step<2
	// debug init must not leave gDeadline at 0 (a poll of linear-memory
	// base+0 could read nonzero data and raise spuriously).
	f.Call(b.imp("rt_ctrl_addr")).GlobalSet(b.gDeadline)
	// U12: the protection-generation address — the table-write arms poll
	// this word before every in-wasm store (sticky: once any table has
	// been protected the arms decline forever, and the probe chain's
	// per-write rt_prot_isprotected screen takes over)
	f.Call(b.imp("rt_protgen_addr")).GlobalSet(b.gProtGen)
	// U14: the pairs/next + ipairs iterator pins (light-C function
	// pointers — zero when the base library was absent at init, and the
	// arms then decline forever). The getters return the ADDRESS of the
	// rt statics; load the pointed-to f value (unlike gProtGen above,
	// which deliberately keeps the address for per-store polling)
	f.Call(b.imp("rt_next_pin_addr")).I32Load(0).GlobalSet(b.gNextF)
	f.Call(b.imp("rt_ipaux_pin_addr")).I32Load(0).GlobalSet(b.gIpairF)
	f.I32Const(0).LocalSet(lC)

	// staged by the step parameter (0 = allocs only, 1 = +chunkname,
	// 2 = +interns +proto registration) — a compile-time debug aid kept
	// for triage
	f.LocalGet(lStep).I32Const(2).I32GeS().If(wasm.Void)

	// chunk name (the FIRST string written — the cursor still sits at
	// 0, so the bytes start exactly at lS)
	writeStrBytes(f, lS, lC, b.chunkName)
	f.LocalGet(lS).I32Const(int32(len(b.chunkName))).Call(b.imp("rt_set_chunkname"))

	// the OP_TBC name table (U4f): names written and registered in order —
	// rt_name_put's return IS the position, which the OP_TBC lowering
	// embeds as an immediate
	for _, nm := range b.tbcNames {
		writeStrBytes(f, lS, lC, nm)
		f.LocalGet(lS).LocalGet(lC).I32Const(int32(len(nm))).I32Sub().I32Add()
		f.I32Const(int32(len(nm))).Call(b.imp("rt_name_put")).Drop()
	}

	// the varinfo hint table (U5e, row 71): same pattern — the raising
	// entries embed rt_hint_put's position for the precomputed
	// " (local 'x')"-class strings (no dedup: concat runs index densely)
	for _, h := range b.hints {
		writeStrBytes(f, lS, lC, h)
		f.LocalGet(lS).LocalGet(lC).I32Const(int32(len(h))).I32Sub().I32Add()
		f.I32Const(int32(len(h))).Call(b.imp("rt_hint_put")).Drop()
	}

	for _, pi := range b.protos {
		for i, cv := range pi.proto.Constants {
			switch v := cv.(type) {
			case lua.LNumber:
				f.LocalGet(lK).I32Const(int32(cellSize * (pi.koff + i))).I32Add().F64Const(float64(v)).
					Call(b.imp("rt_mknumber"))
			case lua.LInteger:
				// ABI v4: integers stage losslessly (variant-tag cells)
				f.LocalGet(lK).I32Const(int32(cellSize * (pi.koff + i))).I32Add().I64Const(int64(v)).
					Call(b.imp("rt_mkinteger"))
			case lua.LString:
				writeStrBytes(f, lS, lC, string(v))
				f.LocalGet(lK).I32Const(int32(cellSize * (pi.koff + i))).I32Add()
				f.LocalGet(lS).LocalGet(lC).I32Const(int32(len(v))).I32Sub().I32Add()
				f.I32Const(int32(len(v))).Call(b.imp("rt_intern")).Drop()
			default:
				panic(fmt.Sprintf("luawasm: unsupported constant type %T", cv))
			}
		}
	}

	// register every proto FIRST: (idx, numparams, isvararg, nupvalues,
	// framecells). THEN the upvalue capture descriptors — FunctionProto.
	// Upvalues[] (U2c moved captures out of the post-OP_CLOSURE
	// pseudo-instructions; the fork convention is gone). Order matters:
	// rt_wasm_upval drops rows for unregistered protos, and a parent's
	// registration must precede its children's descriptor rows.
	for _, pi := range b.protos {
		p := pi.proto
		f.I32Const(int32(b.protoIdx[p])).
			I32Const(int32(p.NumParameters)).
			I32Const(int32(p.IsVarArg)).
			I32Const(int32(p.NumUpvalues)).
			I32Const(int32(frameCells(int(p.NumUsedRegisters)))).
			Call(b.imp("rt_wasm_proto")).Drop()
	}
	for _, pi := range b.protos {
		p := pi.proto
		for u, uv := range p.Upvalues {
			instack := int32(0)
			if uv.InStack {
				instack = 1
			}
			f.I32Const(int32(b.protoIdx[p])).
				I32Const(int32(u)).
				I32Const(instack).
				I32Const(int32(uv.Idx)).
				Call(b.imp("rt_wasm_upval"))
		}
	}

	f.End()
	f.End() // close the step gate
	f.Export("luawasm_init")
}

// emitDispatch writes lua_dispatch(idx, frame, cl, nargs, want) -> nret:
// a br_table of direct calls over the proto index, inside the M5c
// tail-restage loop scaffold. Arm k tail-calls protoFuncs[k] then breaks
// OUT to the -2 check (never to the loop label — that would re-dispatch
// the same proto forever); default: unknown idx → host-refused (-3).
func (b *backend) emitDispatch() {
	f := b.dispatchF // the shell Compile reserved before the bodies (U9c)
	const lNret uint32 = 5
	// U9a (row 103): 6 = the entry frame — the M5c restage loop below
	// overwrites the frame param (local 1) with each restaged successor
	// frame, but lua_main's caller reads results at the frame IT passed
	// (host/vm.go reads entry+16i); 8 = the entry cl, saved because the
	// restage loop overwrites local 2 as well. 7 = the copy-back loop
	// index.
	const lEntry uint32 = 6
	const lI uint32 = 7
	const lEntryCl uint32 = 8
	f.Local(wasm.I32)
	f.Local(wasm.I32)
	f.Local(wasm.I32)
	f.Local(wasm.I32)
	n := len(b.protos)
	f.LocalGet(1).LocalSet(lEntry)
	f.LocalGet(2).LocalSet(lEntryCl)

	f.Loop(wasm.Void) // $again — the M5c restage target
	// U6b (row 37): the restage loop is an unbounded back edge with no
	// basic blocks of its own (tail-call chains) — poll it directly.
	b.emitDeadlinePoll(f)
	for i := 0; i <= n; i++ {
		f.Block(wasm.Void)
	}
	// default: unknown idx → host-refused marker
	f.I32Const(-3).LocalSet(lNret)
	depths := make([]uint32, n)
	for i := range depths {
		depths[i] = uint32(i)
	}
	f.LocalGet(0).BrTable(depths, uint32(n))
	for k := 0; k < n; k++ {
		f.End() // close arm k's block (k=0: the innermost)
		f.LocalGet(1).LocalGet(2).LocalGet(3).LocalGet(4).
			Call(b.protoFuncs[k].Idx()).LocalSet(lNret)
		// → past the remaining arm blocks and OUT of the default block,
		// landing at the -2 check (NOT the loop label)
		f.Br(uint32(n - k - 1))
	}
	f.End() // close the default block — the convergence point
	// M5c: the tailcall restage — a staged descriptor becomes a fresh
	// dispatch at the SAME adapter level (same want), so `return f(x)`
	// recursion is O(1) wasm stack and O(1) frames.
	f.LocalGet(lNret).I32Const(-2).I32Eq().If(wasm.Void)
	f.Call(b.imp("rt_tail_clidx")).LocalSet(0)
	f.Call(b.imp("rt_tail_nargs")).LocalSet(3)
	f.Call(b.imp("rt_tail_funcell")).LocalSet(2)
	f.Call(b.imp("rt_tail_restage")).LocalSet(1)
	f.Br(1) // → the dispatch loop again
	f.End()
	f.End() // close loop
	// U9a (row 103): the restage loop exits with results staged at the
	// FINAL successor frame (local 1), while lua_main's caller reads
	// entry+16i — copy the nret cells back when a restage actually moved
	// the frame. Gated on entry cl==0 (row 53: main runs cl=0 — every
	// trampoline-driven dispatch carries its nonzero rt_wcl record): in
	// a NESTED dispatch the copy would be a write through dead cells that
	// can OVERLAP the successor's live result window (vararg blocks pack
	// tightly), clobbering the very cells rtw_dispatch_body is about to
	// copy to the stack — tco08/var13/var07 caught exactly that. Equal
	// frames (a non-vararg successor lands on the entry block) and
	// nret==0 need no copy.
	f.LocalGet(lEntryCl).I32Eqz().If(wasm.Void)
	f.LocalGet(lNret).I32Const(0).I32GeS().If(wasm.Void)
	f.LocalGet(1).LocalGet(lEntry).I32Ne().If(wasm.Void)
	f.I32Const(0).LocalSet(lI)
	f.Block(wasm.Void)
	f.Loop(wasm.Void)
	f.LocalGet(lI).LocalGet(lNret).I32GeS().BrIf(1)
	f.LocalGet(lEntry).LocalGet(lI).I32Const(int32(cellSize)).I32Mul().I32Add()
	f.LocalGet(1).LocalGet(lI).I32Const(int32(cellSize)).I32Mul().I32Add().I64Load(0)
	f.I64Store(0)
	f.LocalGet(lEntry).LocalGet(lI).I32Const(int32(cellSize)).I32Mul().I32Add()
	f.LocalGet(1).LocalGet(lI).I32Const(int32(cellSize)).I32Mul().I32Add().I64Load(8)
	f.I64Store(8)
	f.LocalGet(lI).I32Const(1).I32Add().LocalSet(lI)
	f.Br(0)
	f.End()
	f.End()
	f.End()
	f.End()
	f.End()
	// U9c (row 104): publish the EXIT frame for direct callers. The M5c
	// restage loop above may have moved local 1 off the block the direct
	// arm's rt_dd_enter allocated, so the arm reads results through this
	// global — immediately after its $lua_dispatch returns, before any
	// other dispatch can publish over it (a nested lua_dispatch publishes
	// at ITS exit, which precedes this callee's own exit publish; the
	// read that matters always follows the immediate callee's final
	// publish). Trampoline-driven dispatches publish harmlessly — no
	// direct arm is mid-flight across them.
	f.LocalGet(1).GlobalSet(b.gDispFrame)
	f.LocalGet(lNret)
	f.End()
	f.Export("lua_dispatch")
}

// emitMain writes the thin engine entry: lua_main(frame) runs proto 0
// through the dispatcher with want=-1 (multret) and returns the dispatch
// status verbatim — nret ≥ 0 (nret results staged at frame+0..16n) or a
// negative error code with the error TValue staged in the runtime (the
// host reads script results off the frame cells, so the count must
// survive the entry). Main runs cl=0 (row 53): its _ENV is the globals
// table, and GETTABUP/SETTABUP on the main proto lower to
// rt_getglobal/rt_setglobal for exactly that reason.
//
// U4g: lua_main calls the dispatcher DIRECTLY — the main chunk has no C
// trampoline, so rt_wasm_trampoline's error arms (the rt_close_err
// boundary close, row 75) never run for its dying frames. On a staged
// error run the protected close at the main frame level here: main-chunk
// tbc cells are ≥ frame, the 2-arg __close fold runs under rt_close_err's
// per-entry protection (a failing close REPLACES the staged object), and
// a no-entry call re-stages the original untouched (its `had` path).
func (b *backend) emitMain() {
	f := b.m.NewFunction([]wasm.ValueType{wasm.I32}, []wasm.ValueType{wasm.I32})
	f.Local(wasm.I32) // 1: the dispatch status
	// U9c (row 104): heal the direct-dispatch depth counter at the run
	// boundary — a trap that surfaced HERE (a main-level direct chain that
	// exhausted the engine stack) unwound lua_main past every wasm-side
	// rt_dd_ret/rt_dd_err decrement, and a pooled VM's next Run must start
	// from the full budget.
	f.Call(b.imp("rt_dd_reset"))
	// U5e (row 60): plant the main chunk's bottom shadow entry — keyed
	// &base_ci (lua_main runs cl=0 with no CallInfo, so stock
	// lua_getstack cannot resolve main level) and bound to main's line
	// cell at frame+16*(nregs_main+4). The dispatch's first wasm
	// instruction rebinding the same cell is harmless; the pop at the
	// single exit keeps the stack balanced for the next run.
	f.LocalGet(0).I32Const(int32(cellSize * (int(b.main.NumUsedRegisters) + 4))).
		I32Add().Call(b.imp("rt_sea_main"))
	f.I32Const(0).LocalGet(0).I32Const(0).I32Const(0).I32Const(-1).
		Call(b.dispatchFn)
	f.LocalSet(1)
	f.LocalGet(1).I32Const(-1).I32Eq().If(wasm.Void)
	f.LocalGet(0).Call(b.imp("rt_close_err"))
	f.End()
	f.Call(b.imp("rt_sea_pop"))
	f.LocalGet(1)
	f.End()
	f.Export("lua_main")
}

// writeStrBytes appends s to the sbuf (base local lS, cursor local lC),
// advancing the cursor: full 8-byte chunks via i64.const stores, the
// tail via single-byte stores.
func writeStrBytes(f *wasm.Function, lS, lC uint32, s string) {
	addr := func(off int) {
		f.LocalGet(lS).LocalGet(lC).I32Add()
		if off > 0 {
			f.I32Const(int32(off)).I32Add()
		}
	}
	i := 0
	for ; i+8 <= len(s); i += 8 {
		var v uint64
		for j := 7; j >= 0; j-- {
			v = v<<8 | uint64(s[i+j])
		}
		addr(i)
		f.I64Const(int64(v)).I64Store(0)
	}
	for ; i < len(s); i++ {
		addr(i)
		f.I32Const(int32(s[i])).I32Store8(0)
	}
	f.LocalGet(lC).I32Const(int32(len(s))).I32Add().LocalSet(lC)
}
