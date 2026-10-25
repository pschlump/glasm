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
	rtTagLCF    = 0x16 // LUA_VLCF raw — light C funcs are not collectable
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

// stageNilFill stages nil (variant-0) into cells A+3+from .. A+3+C-1 —
// poscall's missing-results adjustment for a fixed want (C results,
// the callee returned fewer).
func (fe *funcEmitter) stageNilFill(A, C, from int) {
	for s := from; s < C; s++ {
		fe.cellAddr(A + 3 + s)
		fe.f.I32Const(rtTagNil).I32Store(8)
	}
}

// armNext lowers luaB_next + luaH_next inline at a TFORCALL. It runs
// inside the caller's A/B wrapper (emitTForcall) and opens its own
// body block on top: BrIf(0)/Br(0) = decline out of THIS arm (fall
// through to armIpairs / the gmatch probe — never br to B, that skips
// them), Br(4) = the hit (A's end — the TFORLOOP test); each nested
// If/Loop adds one.
//
// The callee must be the pinned stock next — a LIGHT C function (zero
// upvalues): the cell's value word IS the f pointer, raw LUA_VLCF tag
// (no closure object to walk). The state must be a table (luaB_next's
// checktype raise belongs to C). Control shapes armed, per findindex:
// nil (first iteration, i=0), an integer key inside [1,asize] (resume
// at k — the slot after the key's own k-1), and a short string (the
// bucket-chain walk; a DELETED key's node still anchors the resume —
// getgeneric's deadok=1 — so the match is on the key alone, value
// liveness never checked). Everything else declines: integer keys in
// the hash part (hashint is C's), any other key type, a key not in
// the table (C raises "invalid key to 'next'"), a custom iterator
// (different f — pairs pushes the STATIC luaB_next, same f as the
// pin, so a shadowed global next still arms; a replaced pairs or a
// hand-rolled iterator declines), a state that is not a table.
//
// The scan is stock luaH_next verbatim: array part first (tag row at
// array+4+i, variant-0 = empty), then the hash part SLOT-SEQUENTIAL
// (chains are irrelevant to order — pairs order is observable) with
// sizenode = 1<<lsizenode even for dummy tables (the shared dummy
// node's VEMPTY gval just never hits). Locals: lT0=t, lT1=hashN,
// lDd0=array, lDd1=asize, lTop=i (array index, then hash index),
// lSt=node cursor, lVlo=i64 temp.
func (fe *funcEmitter) armNext(A, C int) {
	f := fe.f
	f.Block(wasm.Void) // arm body: BrIf(0) = decline -> armIpairs/probe
	fe.cellAddr(A + 3).I32Load8U(8).I32Const(rtTagLCF).I32Ne().BrIf(0)
	fe.cellAddr(A + 3).I32Load(0).GlobalGet(fe.b.gNextF).I32Ne().BrIf(0)
	fe.cellAddr(A + 4).I32Load(0).LocalSet(fe.lT0)
	fe.cellAddr(A + 4).I32Load8U(8).I32Const(rtTagTable).I32Ne().BrIf(0)
	f.LocalGet(fe.lT0).I32Load(tblOffAsize).LocalSet(fe.lDd1)
	f.I32Const(1).LocalGet(fe.lT0).I32Load8U(tblOffLsizenode).I32Shl().LocalSet(fe.lT1)
	f.LocalGet(fe.lT0).I32Load(tblOffArray).LocalSet(fe.lDd0)
	// ---- findindex ----
	fe.cellAddr(A + 5).I32Load8U(8).I32Const(rtTagNil).I32Eq().If(wasm.Void)
	f.I32Const(0).LocalSet(fe.lTop)
	f.Else()
	fe.cellAddr(A + 5).I32Load8U(8).I32Const(rtTagInt).I32Eq().If(wasm.Void)
	// checkrange: (u64)(k-1) <u asize ⟺ 1 <= k <= asize; resume AT k
	fe.cellAddr(A + 5).I64Load(0).LocalSet(fe.lVlo)
	f.LocalGet(fe.lVlo).I64Const(1).I64Sub()
	f.LocalGet(fe.lDd1).I64ExtendI32U()
	f.I64GeU().BrIf(2) // decline: If_int(0) If_nil(1) N(2)
	f.LocalGet(fe.lVlo).I32WrapI64().LocalSet(fe.lTop)
	f.Else()
	fe.cellAddr(A + 5).I32Load8U(8).I32Const(rtTagShrstr).I32Eq().If(wasm.Void)
	// bucket = s->hash & (hashN-1) — NOTE: left operand (hashN-1)
	// first would compute (hashN-1)&hash; the And's rhs-then-lhs pop
	// order needs the HASH pushed first, mask second
	f.LocalGet(fe.lT0).I32Load(tblOffNode)
	fe.cellAddr(A + 5).I32Load(0).I32Load(strOffHash)
	f.LocalGet(fe.lT1).I32Const(1).I32Sub()
	f.I32And().I32Const(nodeSz).I32Mul().I32Add().LocalSet(fe.lSt)
	f.Loop(wasm.Void)
	f.LocalGet(fe.lSt).I32Load8U(nodeOffKeyTT).I32Const(rtTagShrstr).I32Eq()
	f.LocalGet(fe.lSt).I32Load(nodeOffKeyVal)
	fe.cellAddr(A + 5).I32Load(0)
	f.I32Eq().I32And().If(wasm.Void)
	// found (dead or live — deadok): i = asize + nodeIdx + 1
	f.LocalGet(fe.lDd1)
	f.LocalGet(fe.lSt)
	f.LocalGet(fe.lT0).I32Load(tblOffNode)
	f.I32Sub().I32Const(nodeSz).I32DivS()
	f.I32Const(1).I32Add().I32Add().LocalSet(fe.lTop)
	f.Br(4) // to the scans: If_found(0) Loop(1) If_str(2) If_nil(3) N(4)
	f.End()
	f.LocalGet(fe.lSt).I32Load(nodeOffNext).I32Eqz().BrIf(3) // decline: Loop(0) If_str(1) If_nil(2) N(3)
	f.LocalGet(fe.lSt)
	f.LocalGet(fe.lSt).I32Load(nodeOffNext).I32Const(nodeSz).I32Mul().I32Add()
	f.LocalSet(fe.lSt)
	f.Br(0)
	f.End() // walk loop
	f.Else()
	f.Br(2) // any other control key type: If_str(0) If_nil(1) N(2)
	f.End()
	f.End()
	f.End() // close the findindex If/Else chain
	// ---- array scan (stock's first loop) ----
	f.Block(wasm.Void) // SARR: natural exit = fall into the hash phase
	f.Loop(wasm.Void)
	f.LocalGet(fe.lTop).LocalGet(fe.lDd1).I32GeU().BrIf(1)
	f.LocalGet(fe.lDd0).I32Const(arrTagRowOff).I32Add()
	f.LocalGet(fe.lTop).I32Add().I32Load8U(0).I32Const(0x0F).I32And().I32Eqz().If(wasm.Void)
	f.LocalGet(fe.lTop).I32Const(1).I32Add().LocalSet(fe.lTop)
	f.Br(1) // If(0) -> Loop(1): backedge, NOT the If exit
	f.End()
	// HIT (array slot i): key = i+1, value = farr2val
	fe.cellAddr(A + 3)
	f.LocalGet(fe.lTop).I32Const(1).I32Add().I64ExtendI32U().I64Store(0)
	fe.cellAddr(A + 3)
	f.I32Const(rtTagInt).I32Store(8)
	fe.cellAddr(A + 4)
	f.LocalGet(fe.lDd0)
	f.LocalGet(fe.lTop).I32Const(1).I32Add().I32Const(valSz).I32Mul().I32Sub()
	f.I64Load(0).I64Store(0)
	fe.cellAddr(A + 4)
	f.LocalGet(fe.lDd0).I32Const(arrTagRowOff).I32Add()
	f.LocalGet(fe.lTop).I32Add().I32Load8U(0).I32Store(8)
	fe.stageNilFill(A, C, 2)
	f.Br(4) // Loop(0) SARR(1) N(2) B(3) A(4) — hit exit
	f.End()
	f.End() // SARR
	// ---- hash scan: j = i - asize ----
	f.LocalGet(fe.lTop).LocalGet(fe.lDd1).I32Sub().LocalSet(fe.lTop)
	f.Block(wasm.Void) // SHASH: natural exit = exhausted
	f.Loop(wasm.Void)
	f.LocalGet(fe.lTop).LocalGet(fe.lT1).I32GeU().BrIf(1)
	f.LocalGet(fe.lT0).I32Load(tblOffNode)
	f.LocalGet(fe.lTop).I32Const(nodeSz).I32Mul().I32Add().LocalSet(fe.lSt)
	f.LocalGet(fe.lSt).I32Load8U(nodeOffTT).I32Const(0x0F).I32And().I32Eqz().If(wasm.Void)
	f.LocalGet(fe.lTop).I32Const(1).I32Add().LocalSet(fe.lTop)
	f.Br(1) // If(0) -> Loop(1): backedge, NOT the If exit
	f.End()
	// HIT (node j): key = getnodekey (word + tt byte wholesale), value = gval
	fe.cellAddr(A + 3)
	f.LocalGet(fe.lSt).I64Load(nodeOffKeyVal).I64Store(0)
	fe.cellAddr(A + 3)
	f.LocalGet(fe.lSt).I32Load8U(nodeOffKeyTT).I32Store(8)
	fe.cellAddr(A + 4)
	f.LocalGet(fe.lSt).I64Load(0).I64Store(0)
	fe.cellAddr(A + 4)
	f.LocalGet(fe.lSt).I32Load8U(nodeOffTT).I32Store(8)
	fe.stageNilFill(A, C, 2)
	f.Br(4) // Loop(0) SHASH(1) N(2) B(3) A(4) — hit exit
	f.End()
	f.End() // SHASH
	// exhausted: all C results nil (luaB_next's 1-value nil, adjusted)
	fe.stageNilFill(A, C, 0)
	f.Br(2) // N(0) B(1) A(2)
	f.End() // arm body
}

// armIpairs lowers ipairsaux inline at a TFORCALL — same A/B wrapper
// context as armNext, with its own body block: BrIf(0) = decline (fall
// through to the gmatch probe), Br(3) = the hit (A's end). The callee
// must be the pinned stock ipairsaux
// (light C), the state a table (a non-table state may have __index —
// lua_geti's metamethod arm belongs to C), the control an integer.
// Stock: j = k+1; in-bounds (checkrange on j ⟺ (u64)k <u asize) and
// live → (j, t[j]); in-bounds and empty → one nil result (geti's NIL,
// poscall-adjusted — the loop exits); beyond asize → decline (hash
// integers and metamethods are C's).
func (fe *funcEmitter) armIpairs(A, C int) {
	f := fe.f
	f.Block(wasm.Void) // arm body: BrIf(0) = decline -> the probe
	fe.cellAddr(A + 3).I32Load8U(8).I32Const(rtTagLCF).I32Ne().BrIf(0)
	fe.cellAddr(A + 3).I32Load(0).GlobalGet(fe.b.gIpairF).I32Ne().BrIf(0)
	fe.cellAddr(A + 4).I32Load(0).LocalSet(fe.lT0)
	fe.cellAddr(A + 4).I32Load8U(8).I32Const(rtTagTable).I32Ne().BrIf(0)
	// __index: for an EMPTY in-bounds slot stock lua_geti consults the
	// metamethod (it may return non-nil and the loop continues) — the
	// arm's nil-staging would diverge. flags cannot screen this
	// (invalidateTMcache zeroes them on any insert), so any metatable
	// declines, conservative
	f.LocalGet(fe.lT0).I32Load(tblOffMetatable).BrIf(0)
	fe.cellAddr(A + 5).I32Load8U(8).I32Const(rtTagInt).I32Ne().BrIf(0)
	fe.cellAddr(A + 5).I64Load(0).LocalSet(fe.lVlo) // k
	f.LocalGet(fe.lVlo)
	f.LocalGet(fe.lT0).I32Load(tblOffAsize).I64ExtendI32U()
	f.I64GeU().BrIf(0)
	f.LocalGet(fe.lT0).I32Load(tblOffArray).LocalSet(fe.lDd0)
	f.LocalGet(fe.lVlo).I32WrapI64().LocalSet(fe.lTop) // idx = k (slot of t[k+1]... geti(j), j-1 = k)
	f.LocalGet(fe.lDd0).I32Const(arrTagRowOff).I32Add()
	f.LocalGet(fe.lTop).I32Add().I32Load8U(0).I32Const(0x0F).I32And().I32Eqz().If(wasm.Void)
	// exhausted: t[j] nil — all C results nil
	fe.stageNilFill(A, C, 0)
	f.Br(3) // If(0) N(1) B(2) A(3)
	f.Else()
	// hit: (j, t[j]) with j = k+1
	fe.cellAddr(A + 3)
	f.LocalGet(fe.lVlo).I64Const(1).I64Add().I64Store(0)
	fe.cellAddr(A + 3)
	f.I32Const(rtTagInt).I32Store(8)
	fe.cellAddr(A + 4)
	f.LocalGet(fe.lDd0)
	f.LocalGet(fe.lVlo).I32WrapI64().I32Const(1).I32Add().I32Const(valSz).I32Mul().I32Sub()
	f.I64Load(0).I64Store(0)
	fe.cellAddr(A + 4)
	f.LocalGet(fe.lDd0).I32Const(arrTagRowOff).I32Add()
	f.LocalGet(fe.lTop).I32Add().I32Load8U(0).I32Store(8)
	fe.stageNilFill(A, C, 2)
	f.Br(3) // If(0) N(1) B(2) A(3) — hit exit
	f.End()
	f.End() // arm body
}
