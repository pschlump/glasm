// Package luawasm is the Lua→wasm backend (design doc §4): FunctionProto
// trees from the golua55 frontend become wasm modules that share linear
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

	lua "github.com/pschlump/golua55"
	"github.com/pschlump/golua55/wasm"
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
}

// Compile emits a wasm module for the FunctionProto tree rooted at main.
func Compile(main *lua.FunctionProto, chunkName string) ([]byte, error) {
	protos := collectProtos(main)

	// the C runtime renders the chunk name verbatim in error positions
	// (ldebug reads the proto source raw), so strip the '@' sigil the Go
	// callers carry (U1 ChunkID convention) before registering it
	b := &backend{m: wasm.NewModule(), main: main, chunkName: strings.TrimPrefix(chunkName, "@")}
	b.layoutProtos(protos)
	b.declareImports()
	b.emitInit()
	for _, pi := range b.protos {
		fe := b.newFuncEmitter(pi.proto)
		fe.emitBody()
		b.protoFuncs = append(b.protoFuncs, fe.f)
	}
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
	m          *wasm.Module
	main       *lua.FunctionProto
	chunkName  string
	imports    map[string]uint32
	gKCells    uint32 // mutable global: constants cells base
	protos     []protoInfo
	protoIdx   map[*lua.FunctionProto]int // proto → dispatch index
	protoFuncs []*wasm.Function           // dispatch idx → wasm function
	dispatchFn uint32                     // lua_dispatch's function index
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
	i32f64 := []wasm.ValueType{wasm.I32, wasm.F64}
	i32i64 := []wasm.ValueType{wasm.I32, wasm.I64}

	// ABI v4 (runtime/rt_abi55.h). Dropped vs v3: rt_compat_arg (a
	// 5.1-ism), the dialect layer and the deadline block (U6), and
	// rt_mknil (inline tag store). New: rt_mkinteger (lossless int64
	// cells — the reason the 5.1 blob could not serve U2).
	b.imports = map[string]uint32{
		"rt_newtable":      m.ImportFunc("rt", "rt_newtable", iii, i32v),
		"rt_gettable":      m.ImportFunc("rt", "rt_gettable", iiii, i32v),
		"rt_settable":      m.ImportFunc("rt", "rt_settable", iiii, i32v),
		"rt_arith":         m.ImportFunc("rt", "rt_arith", iiiii, i32v),
		"rt_len":           m.ImportFunc("rt", "rt_len", iii, i32v),
		"rt_eq":            m.ImportFunc("rt", "rt_eq", iiii, i32v),
		"rt_lt":            m.ImportFunc("rt", "rt_lt", iiii, i32v),
		"rt_le":            m.ImportFunc("rt", "rt_le", iiii, i32v),
		"rt_concat":        m.ImportFunc("rt", "rt_concat", iiii, i32v),
		"rt_call":          m.ImportFunc("rt", "rt_call", iiiii, i32v),
		"rt_call_count":    m.ImportFunc("rt", "rt_call_count", i32v, i32v),
		"rt_forprep":       m.ImportFunc("rt", "rt_forprep", ii, i32v),
		"rt_frame_alloc":   m.ImportFunc("rt", "rt_frame_alloc", i32v, i32v),
		"rt_getglobal":     m.ImportFunc("rt", "rt_getglobal", iii, i32v),
		"rt_setglobal":     m.ImportFunc("rt", "rt_setglobal", iii, i32v),
		"rt_intern":        m.ImportFunc("rt", "rt_intern", iii, i32v),
		"rt_mknumber":      m.ImportFunc("rt", "rt_mknumber", i32f64, nil),
		"rt_mkinteger":     m.ImportFunc("rt", "rt_mkinteger", i32i64, nil),
		"rt_set_chunkname": m.ImportFunc("rt", "rt_set_chunkname", ii, nil),
		"rt_wasm_proto":    m.ImportFunc("rt", "rt_wasm_proto", iiiii, i32v),
		"rt_wasm_upval":    m.ImportFunc("rt", "rt_wasm_upval", iiii, nil),
		"rt_newclosure":    m.ImportFunc("rt", "rt_newclosure", iiiii, i32v),
		"rt_getupval":      m.ImportFunc("rt", "rt_getupval", iii, nil),
		"rt_setupval":      m.ImportFunc("rt", "rt_setupval", iii, nil),
		"rt_close_upvals":  m.ImportFunc("rt", "rt_close_upvals", i32v, nil),
		"rt_tail_stage":    m.ImportFunc("rt", "rt_tail_stage", iiii, i32v),
		"rt_tail_clidx":    m.ImportFunc("rt", "rt_tail_clidx", nil, i32v),
		"rt_tail_nargs":    m.ImportFunc("rt", "rt_tail_nargs", nil, i32v),
		"rt_tail_funcell":  m.ImportFunc("rt", "rt_tail_funcell", nil, i32v),
		"rt_tail_restage":  m.ImportFunc("rt", "rt_tail_restage", nil, i32v),
	}
	b.gKCells = m.GlobalI32(0, true)
	m.ExportGlobal("gKCells", b.gKCells)
	m.ImportMemory("rt", "memory", 1, 0)
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

	f.I32Const(int32(strBytes)).Call(b.imp("rt_frame_alloc")).LocalSet(lS)
	f.I32Const(int32(cellSize * total)).Call(b.imp("rt_frame_alloc")).LocalSet(lK)
	f.LocalGet(lK).GlobalSet(b.gKCells)
	f.I32Const(0).LocalSet(lC)

	// staged by the step parameter (0 = allocs only, 1 = +chunkname,
	// 2 = +interns +proto registration) — a compile-time debug aid kept
	// for triage
	f.LocalGet(lStep).I32Const(2).I32GeS().If(wasm.Void)

	// chunk name (the FIRST string written — the cursor still sits at
	// 0, so the bytes start exactly at lS)
	writeStrBytes(f, lS, lC, b.chunkName)
	f.LocalGet(lS).I32Const(int32(len(b.chunkName))).Call(b.imp("rt_set_chunkname"))

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
	m := b.m
	f := m.NewFunction([]wasm.ValueType{wasm.I32, wasm.I32, wasm.I32, wasm.I32, wasm.I32}, []wasm.ValueType{wasm.I32})
	b.dispatchFn = f.Idx()
	const lNret uint32 = 5
	f.Local(wasm.I32)
	n := len(b.protos)

	f.Loop(wasm.Void) // $again — the M5c restage target
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
func (b *backend) emitMain() {
	f := b.m.NewFunction([]wasm.ValueType{wasm.I32}, []wasm.ValueType{wasm.I32})
	f.I32Const(0).LocalGet(0).I32Const(0).I32Const(0).I32Const(-1).
		Call(b.dispatchFn)
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
