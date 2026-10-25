package luawasm

// U12: in-wasm table access arms. Every table op today costs at least
// one wasm->C crossing even on its fast path (the U10/U11 probes):
// these arms move the hit case entirely into wasm — the Table, Node,
// and TString layouts of stock 5.5.1 (wasm32) are walked directly, and
// every condition the matching probe would check is checked in-wasm
// first. An arm is a strict REFINEMENT of its probe's hit set: anything
// the arm declines (non-table, long key, empty slot, metamethod,
// protection, barrier, occupied/colliding insert) falls through to the
// exact probe->protected chain that ran before, so semantics are
// unchanged by construction and the differential gates stay the oracle.
//
// Layout pins (lobject.h/ltable.h, wasm32):
//
//	Table:  next@0 tt@4 marked@5 flags@6 lsizenode@7 asize@8
//	        array@12 node@16 metatable@20 gclist@24      (28 bytes)
//	Node:   value_@0 tt_@8 key_tt@9 next@12 key_val@16    (24 bytes)
//	TString: ... hash@8 ...
//
// The array part is INVERTED (ltable.h's diagram): tags live at
// array+4+k (byte granular, ascending) and values at array-8*(k+1)
// (descending), with the #t lenhint in the unsigned between them. Cell
// tags are full 4-byte words (row 45) but the table-side tag rows and
// NodeKey.tt_ are single bytes — arms read tags with load8_u and write
// them with store8, zero-extending only when staging into a cell.

import "github.com/pschlump/glasm/wasm"

const (
	tblOffMarked    = 5  // Table.marked (CommonHeader: lgc.h)
	tblOffFlags     = 6  // Table.flags: TM-absence cache bits 0..5 + BITDUMMY 6
	tblOffLsizenode = 7  // Table.lsizenode
	tblOffAsize     = 8  // Table.asize (unsigned int)
	tblOffArray     = 12 // Table.array (Value*)
	tblOffNode      = 16 // Table.node
	tblOffMetatable = 20 // Table.metatable

	nodeSz        = 24 // sizeof(Node) on wasm32
	nodeOffTT     = 8  // NodeKey.tt_ (the VALUE's tag byte)
	nodeOffKeyTT  = 9
	nodeOffNext   = 12
	nodeOffKeyVal = 16

	strOffHash = 8 // TString.hash

	arrTagRowOff = 4 // getArrTag(t,k) = array + sizeof(unsigned) + k
	valSz        = 8 // sizeof(Value): getArrVal = array - valSz*(k+1)

	rtTagTable  = 0x45 // ctb(LUA_VTABLE)  — ttistable's exact byte
	rtTagShrstr = 0x44 // ctb(LUA_VSHRSTR) — keyisshrstr's exact byte
)

// LayoutPins (the U12 drift lock): rt_layout_num(which) on the rt side
// must return exactly these values — the arms bake them in as i32
// immediates, so a layout change in the vendored tree has to fail
// loudly somewhere instead of silently reading garbage. Hosted by the
// blob export; checked by a host-package test.
var LayoutPins = []struct {
	Which int32
	Val   int32
	Name  string
}{
	{0, tblOffAsize, "Table.asize"},
	{1, tblOffArray, "Table.array"},
	{2, tblOffNode, "Table.node"},
	{3, tblOffMetatable, "Table.metatable"},
	{4, tblOffMarked, "Table.marked"},
	{5, tblOffLsizenode, "Table.lsizenode"},
	{6, nodeSz, "sizeof(Node)"},
	{7, nodeOffKeyTT, "NodeKey.key_tt"},
	{8, nodeOffNext, "NodeKey.next"},
	{9, nodeOffKeyVal, "NodeKey.key_val"},
	{10, nodeOffTT, "NodeKey.tt_"},
	{11, strOffHash, "TString.hash"},
	{12, cellSize, "sizeof(TValue)"},
	{13, arrTagRowOff, "getArrTag row offset"},
	{14, valSz, "sizeof(Value)"},
}

// Arm shape: Block A { Block B { checks+hit } fallback } — a miss
// branches to B's end (into the caller's fallback closure), a hit
// branches past it to A's end. Labels are RELATIVE depths; every br
// site states its count.

// armGeti lowers an integer-key table read. keyConst >= 0 embeds the
// key (OP_GETI/SETI's sC field — an int by construction, no tag check);
// otherwise the key cell's tag is checked in-wasm. Locals: lT0 = Table*,
// lVlo = u (the C-index), lT1 = tag address.
func (fe *funcEmitter) armGeti(tblCell, keyCell func() *wasm.Function, keyConst int64, dst int, fallback func()) {
	f := fe.f
	f.Block(wasm.Void) // A: hit exit
	f.Block(wasm.Void) // B: miss exit -> fallback
	tblCell().I32Load(0).LocalSet(fe.lT0)
	tblCell().I32Load8U(8).I32Const(rtTagTable).I32Ne().BrIf(0)
	if keyConst >= 0 {
		f.I64Const(keyConst)
	} else {
		keyCell().I32Load8U(8).I32Const(rtTagInt).I32Ne().BrIf(0)
		keyCell().I64Load(0)
	}
	f.I64Const(1).I64Sub().LocalSet(fe.lVlo)
	// u <u (u64)asize — key 0 wraps to 2^64-1 and misses by itself
	f.LocalGet(fe.lVlo)
	f.LocalGet(fe.lT0).I32Load(tblOffAsize).I64ExtendI32U()
	f.I64GeU().BrIf(0)
	f.LocalGet(fe.lT0).I32Load(tblOffArray).LocalSet(fe.lT1)
	f.LocalGet(fe.lT1).I32Const(arrTagRowOff).I32Add()
	f.LocalGet(fe.lVlo).I32WrapI64().I32Add().LocalSet(fe.lT1)
	f.LocalGet(fe.lT1).I32Load8U(0).I32Const(0x0F).I32And().I32Eqz().BrIf(0)
	// hit: farr2val — the 8-byte value, then the tag byte as a full word
	fe.cellAddr(dst)
	f.LocalGet(fe.lT0).I32Load(tblOffArray)
	f.LocalGet(fe.lVlo).I32WrapI64().I32Const(1).I32Add().I32Const(valSz).I32Mul().I32Sub()
	f.I64Load(0).I64Store(0)
	fe.cellAddr(dst)
	f.LocalGet(fe.lT1).I32Load8U(0).I32Store(8)
	f.Br(1) // past the fallback (B=0, A=1)
	f.End()
	fallback()
	f.End()
}

// armSeti lowers an integer-key table write — luaH_fastseti's in-bounds
// arm: an overwrite is always fast; an empty in-bounds slot needs
// checknoTM(metatable, TM_NEWINDEX) (the metatable's flags byte, bit
// 1<<1). Out-of-bounds keys (hash-part ints — key 0, negatives) decline
// to the probe chain, which still writes them. Screens before any
// store: the protection generation (writes to a protected table must
// raise through the seam, never bypass it) and the write barrier's
// conservative superset (iscollectable(v) && isblack(t) — the exact
// barrier also needs iswhite(v); declining more only costs speed).
func (fe *funcEmitter) armSeti(tblCell, keyCell func() *wasm.Function, keyConst int64, valCell func() *wasm.Function, fallback func()) {
	f := fe.f
	f.Block(wasm.Void) // A
	f.Block(wasm.Void) // B
	f.GlobalGet(fe.b.gProtGen).I32Load(0).BrIf(0)
	tblCell().I32Load(0).LocalSet(fe.lT0)
	tblCell().I32Load8U(8).I32Const(rtTagTable).I32Ne().BrIf(0)
	if keyConst >= 0 {
		f.I64Const(keyConst)
	} else {
		keyCell().I32Load8U(8).I32Const(rtTagInt).I32Ne().BrIf(0)
		keyCell().I64Load(0)
	}
	f.I64Const(1).I64Sub().LocalSet(fe.lVlo)
	f.LocalGet(fe.lVlo)
	f.LocalGet(fe.lT0).I32Load(tblOffAsize).I64ExtendI32U()
	f.I64GeU().BrIf(0)
	f.LocalGet(fe.lT0).I32Load(tblOffArray).LocalSet(fe.lT1)
	f.LocalGet(fe.lT1).I32Const(arrTagRowOff).I32Add()
	f.LocalGet(fe.lVlo).I32WrapI64().I32Add().LocalSet(fe.lT1)
	// empty slot? -> the NEWINDEX screen (fastseti's ||, short-circuited)
	f.LocalGet(fe.lT1).I32Load8U(0).I32Const(0x0F).I32And().I32Eqz().If(wasm.Void)
	f.LocalGet(fe.lT0).I32Load(tblOffMetatable).If(wasm.Void) // has one?
	f.LocalGet(fe.lT0).I32Load(tblOffMetatable).I32Load8U(6)  // its flags
	f.I32Const(2).I32And().I32Eqz().BrIf(2)                   // __newindex not cached absent -> decline
	f.End()
	f.End()
	// the barrier screen: collectable value into a black table declines
	valCell().I32Load8U(8).I32Const(0x40).I32And().If(wasm.Void)
	f.LocalGet(fe.lT0).I32Load8U(tblOffMarked).I32Const(0x20).I32And().BrIf(1)
	f.End()
	// fval2arr: tag byte into the row, value below the array base
	f.LocalGet(fe.lT1)
	valCell().I32Load8U(8).I32Store8(0)
	f.LocalGet(fe.lT0).I32Load(tblOffArray)
	f.LocalGet(fe.lVlo).I32WrapI64().I32Const(1).I32Add().I32Const(valSz).I32Mul().I32Sub()
	valCell().I64Load(0).I64Store(0)
	f.Br(1)
	f.End()
	fallback()
	f.End()
}

// armGets lowers a short-string-key table read — luaH_Hgetshortstr's
// chain walk verbatim: bucket = key->hash & (sizenode-1), then
// keyisshrstr(n) && keystrval(n)==key, else gnext(n) in NODE units,
// 0 = chain end. The walk is dummynode-safe without an isdummy check
// (lsizenode 0 -> node[0], whose nil key_tt never matches and whose
// next is 0). Locals: lT0 = Table*, lT1 = TString*, lDd0 = Node*.
func (fe *funcEmitter) armGets(tblCell, keyCell func() *wasm.Function, dst int, fallback func()) {
	f := fe.f
	f.Block(wasm.Void) // A
	f.Block(wasm.Void) // B
	tblCell().I32Load(0).LocalSet(fe.lT0)
	tblCell().I32Load8U(8).I32Const(rtTagTable).I32Ne().BrIf(0)
	keyCell().I32Load(0).LocalSet(fe.lT1)
	keyCell().I32Load8U(8).I32Const(rtTagShrstr).I32Ne().BrIf(0)
	f.LocalGet(fe.lT0).I32Load(tblOffNode)
	f.LocalGet(fe.lT1).I32Load(strOffHash)
	// NOTE: wasm binops pop rhs-then-lhs — the 1 must be pushed FIRST
	// (pushing lsz first computes lsz<<1, a wrong-but-small mask that
	// only traps when lsz==0: (0<<1)-1 == -1 → the raw hash as bucket).
	f.I32Const(1)
	f.LocalGet(fe.lT0).I32Load8U(tblOffLsizenode)
	f.I32Shl().I32Const(1).I32Sub()
	f.I32And().I32Const(nodeSz).I32Mul().I32Add().LocalSet(fe.lDd0)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lDd0).I32Load8U(nodeOffKeyTT).I32Const(rtTagShrstr).I32Eq()
	f.LocalGet(fe.lDd0).I32Load(nodeOffKeyVal).LocalGet(fe.lT1).I32Eq()
	f.I32And()
	f.LocalGet(fe.lDd0).I32Load8U(nodeOffTT).I32Const(0x0F).I32And().I32Const(0).I32Ne()
	f.I32And().If(wasm.Void)
	fe.cellAddr(dst)
	f.LocalGet(fe.lDd0).I64Load(0).I64Store(0)
	fe.cellAddr(dst)
	f.LocalGet(fe.lDd0).I32Load8U(nodeOffTT).I32Store(8)
	f.Br(3)
	f.End()
	f.LocalGet(fe.lDd0).I32Load(nodeOffNext).I32Eqz().BrIf(1)
	f.LocalGet(fe.lDd0)
	f.LocalGet(fe.lDd0)
	f.LocalGet(fe.lDd0).I32Load(nodeOffNext).I32Const(nodeSz).I32Mul().I32Add()
	f.LocalSet(fe.lDd0)
	f.Br(0)
	f.End()
	f.End()
	fallback()
	f.End()
}

// armSets lowers a short-string-key table write in two arms, both
// stock-verbatim: (1) psetshortstr's existing-key arm — setobj: value +
// tag byte, the key untouched; (2) at chain end (key absent),
// insertkey's main-position-free branch: isempty(gval(mp)) -> setnodekey
// (the kcell's value word + tag byte, wholesale) + setobj + the caller's
// invalidateTMcache (flags &= ~0x3F, keeping BITDUMMY). Everything else —
// colliding main position (getfreepos/occupant-move), full tables
// (rehash), float/int normalization, external strings — declines: the
// probe and protected path already own it. Screens before any store:
// protection generation, BITDUMMY (inserting into the shared dummy node
// would write a static), nil values (luaH_newkey's !ttisnil), fasttm
// __newindex absence (finishset consults it BEFORE inserting), and the
// barrier: the key is collectable by construction, so any black table
// declines (luaC_barrierback both for the key and the value). mp->next is
// left untouched, matching insertkey: a deleted mid-chain head keeps its
// chain and is reused in place.
func (fe *funcEmitter) armSets(tblCell, keyCell, valCell func() *wasm.Function, fallback func()) {
	f := fe.f
	f.Block(wasm.Void) // A
	f.Block(wasm.Void) // B
	f.GlobalGet(fe.b.gProtGen).I32Load(0).BrIf(0)
	tblCell().I32Load(0).LocalSet(fe.lT0)
	tblCell().I32Load8U(8).I32Const(rtTagTable).I32Ne().BrIf(0)
	keyCell().I32Load(0).LocalSet(fe.lT1)
	keyCell().I32Load8U(8).I32Const(rtTagShrstr).I32Ne().BrIf(0)
	f.LocalGet(fe.lT0).I32Load(tblOffNode)
	f.LocalGet(fe.lT1).I32Load(strOffHash)
	f.I32Const(1) // rhs-then-lhs: 1 << lsizenode (see armGets)
	f.LocalGet(fe.lT0).I32Load8U(tblOffLsizenode)
	f.I32Shl().I32Const(1).I32Sub()
	f.I32And().I32Const(nodeSz).I32Mul().I32Add().LocalSet(fe.lDd0)
	f.LocalGet(fe.lDd0).LocalSet(fe.lDd1) // mp, saved across the walk
	f.Block(wasm.Void)                    // C: the walk — its end falls into the insert region
	f.Loop(wasm.Void)
	// existing key with a live value? (empty value = a nil'd slot)
	f.LocalGet(fe.lDd0).I32Load8U(nodeOffKeyTT).I32Const(rtTagShrstr).I32Eq()
	f.LocalGet(fe.lDd0).I32Load(nodeOffKeyVal).LocalGet(fe.lT1).I32Eq()
	f.I32And()
	f.LocalGet(fe.lDd0).I32Load8U(nodeOffTT).I32Const(0x0F).I32And().I32Const(0).I32Ne()
	f.I32And().If(wasm.Void)
	// the barrier screen (same as armSeti's)
	valCell().I32Load8U(8).I32Const(0x40).I32And().If(wasm.Void)
	f.LocalGet(fe.lT0).I32Load8U(tblOffMarked).I32Const(0x20).I32And().BrIf(3)
	f.End()
	// overwrite hit: setobj — value word, then the tag byte into tt_
	f.LocalGet(fe.lDd0)
	valCell().I64Load(0).I64Store(0)
	f.LocalGet(fe.lDd0)
	valCell().I32Load8U(8).I32Store8(nodeOffTT)
	f.Br(4)
	f.End()
	f.LocalGet(fe.lDd0).I32Load(nodeOffNext).I32Eqz().BrIf(1) // -> insert region (C's end)
	f.LocalGet(fe.lDd0)
	f.LocalGet(fe.lDd0)
	f.LocalGet(fe.lDd0).I32Load(nodeOffNext).I32Const(nodeSz).I32Mul().I32Add()
	f.LocalSet(fe.lDd0)
	f.Br(0)
	f.End()
	f.End()
	// ---- insert: the key is absent ----

	// dummy node? (flags BITDUMMY 0x40 — insertkey's || isdummy(t))
	f.LocalGet(fe.lT0).I32Load8U(tblOffFlags).I32Const(0x40).I32And().BrIf(0)
	// nil never inserts (luaH_newkey's !ttisnil(value))
	valCell().I32Load8U(8).I32Const(0x0F).I32And().I32Eqz().BrIf(0)
	// fasttm __newindex absent? (a metatable without the bit cached away declines)
	f.LocalGet(fe.lT0).I32Load(tblOffMetatable).If(wasm.Void)
	f.LocalGet(fe.lT0).I32Load(tblOffMetatable).I32Load8U(tblOffFlags)
	f.I32Const(2).I32And().I32Eqz().BrIf(1)
	f.End()
	// barrier: the key is collectable by construction — black tables decline
	f.LocalGet(fe.lT0).I32Load8U(tblOffMarked).I32Const(0x20).I32And().BrIf(0)
	// main position OCCUPIED? (!isempty(gval(mp)): any live variant has a
	// nonzero novariant tag — VNIL/VEMPTY/VABSTKEY are all variant 0)
	f.LocalGet(fe.lDd1).I32Load8U(nodeOffTT).I32Const(0x0F).I32And().BrIf(0)
	// setnodekey: kcell value word + tag byte, wholesale
	f.LocalGet(fe.lDd1)
	keyCell().I64Load(0).I64Store(nodeOffKeyVal)
	f.LocalGet(fe.lDd1)
	keyCell().I32Load8U(8).I32Store8(nodeOffKeyTT)
	// setobj2t: value word + tag byte
	f.LocalGet(fe.lDd1)
	valCell().I64Load(0).I64Store(0)
	f.LocalGet(fe.lDd1)
	valCell().I32Load8U(8).I32Store8(nodeOffTT)
	// invalidateTMcache: clear the TM bits, keep BITDUMMY (flags &= 0xC0)
	f.LocalGet(fe.lT0)
	f.LocalGet(fe.lT0).I32Load8U(tblOffFlags).I32Const(0xC0).I32And().I32Store8(tblOffFlags)
	f.Br(1)
	f.End() // B
	fallback()
	f.End() // A
}
