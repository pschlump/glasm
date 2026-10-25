package lua

import (
	"fmt"
	"os"
	"runtime"
	"weak"
)

// Weak tables (U5f; row 79's durable fix, pulled in from U7+ because the
// official coroutine.lua's collected-coroutines battery is a
// required-green file and needs value-weak clearing). Stock clears a
// weak table's entries when the GC collects their key or value; the fork
// has no incremental GC to hook, so collectgarbage()'s full-collection
// arm runs runtime.GC() for the host heap and then this walk — mark
// every value reachable from the Lua roots, then drop weak entries whose
// weak component is unmarked. Full-collection semantics only: code that
// waits for an AUTOMATIC cycle (closure.lua's repeat-until-GC loop)
// still never observes a clearing — that file keeps its row-cited skip.
//
// Roots: the globals and registry tables, the basic-type metatables, and
// the thread graph — the main, current, and running threads plus any
// thread reachable as a value (a live coroutine keeps its registers,
// frame functions, and hook alive). Marking traverses table cells (a
// weak table's weak side marks nothing — that is the point; its strong
// side and metatable still do), function environments and upvalue cells,
// userdata metatables/environments, and full thread state. Strings,
// numbers, and booleans are eternal; light userdata is never collected
// (stock: not a GC object), so it never clears an entry.

// clearWeakTables runs the mark phase from the Lua roots and the clear
// phase over the weak tables discovered on the way (baseCollectGarbage's
// "collect" arm).
func (ls *LState) clearWeakTables() {
	w := &weakSweep{marked: map[LValue]bool{}}
	w.markTable(ls.G.Global)
	if ls.G.Registry != nil {
		w.markTable(ls.G.Registry)
	}
	for _, mt := range ls.G.builtinMts {
		w.mark(mt)
	}
	for _, th := range []*LState{ls.G.MainThread, ls.G.CurrentThread, ls} {
		if th != nil {
			w.markThread(th)
		}
	}
	w.clear()
}

// weakMode parses a table's __mode (stock luaH_modevia... reads the
// metatable field as a string and tests the 'k'/'v' characters).
func weakMode(tb *LTable) (kmode, vmode bool) {
	if tb.Metatable == LNil {
		return
	}
	mt, ok := tb.Metatable.(*LTable)
	if !ok {
		return
	}
	mode, ok := mt.RawGetString("__mode").(LString)
	if !ok {
		return
	}
	for _, c := range string(mode) {
		switch c {
		case 'k':
			kmode = true
		case 'v':
			vmode = true
		}
	}
	return
}

type weakSweep struct {
	marked map[LValue]bool
	weak   []*LTable
}

func (w *weakSweep) mark(v LValue) {
	switch o := v.(type) {
	case *LTable:
		w.markTable(o)
	case *LFunction:
		if w.marked[v] {
			return
		}
		w.marked[v] = true
		if o.Env != nil {
			w.markTable(o.Env)
		}
		for _, uv := range o.Upvalues {
			if uv != nil {
				w.mark(uv.Value())
			}
		}
	case *LState:
		w.markThread(o)
	case *LUserData:
		if w.marked[v] {
			return
		}
		w.marked[v] = true
		if o.Metatable != LNil {
			w.mark(o.Metatable)
		}
		if o.Env != nil {
			w.markTable(o.Env)
		}
	}
}

func (w *weakSweep) markTable(tb *LTable) {
	if w.marked[tb] {
		return
	}
	w.marked[tb] = true
	if tb.Metatable != LNil {
		w.mark(tb.Metatable)
	}
	kmode, vmode := weakMode(tb)
	if kmode || vmode {
		w.weak = append(w.weak, tb)
		// only the STRONG side marks: 'k'-weak keeps the values (array
		// keys are integers — eternal — so a 'k'-only table keeps its
		// array values like any other), 'v'-weak keeps the dict keys
		switch {
		case kmode && !vmode:
			for _, v := range tb.array {
				w.mark(v)
			}
			for _, v := range tb.dict {
				w.mark(v)
			}
			for _, v := range tb.strdict {
				w.mark(v)
			}
		case vmode && !kmode:
			for k := range tb.dict {
				w.mark(k)
			}
		}
		return
	}
	for _, v := range tb.array {
		w.mark(v)
	}
	for k, v := range tb.dict {
		w.mark(k)
		w.mark(v)
	}
	for _, v := range tb.strdict {
		w.mark(v)
	}
}

func (w *weakSweep) markThread(th *LState) {
	if w.marked[th] {
		return
	}
	w.marked[th] = true
	if th.Env != nil {
		w.markTable(th.Env)
	}
	if th.Parent != nil {
		w.markThread(th.Parent)
	}
	if th.hookFn != nil {
		w.mark(th.hookFn)
	}
	// live registers only — cells beyond top are dead by construction
	for i := 0; i < th.reg.top && i < len(th.reg.array); i++ {
		if v := th.reg.array[i]; v != nil {
			w.mark(v)
		}
	}
	if th.stack != nil {
		for i := 0; i < th.stack.Sp(); i++ {
			if fr := th.stack.At(i); fr != nil {
				if fr.Fn != nil {
					w.mark(fr.Fn)
				}
				if fr.PcallHandler != nil {
					w.mark(fr.PcallHandler)
				}
			}
		}
	}
}

// weakCollectable is whether a value can be collected at all — only the
// reference types participate (light userdata among them is a value in
// stock, but as a *LUserData it clears only if unmarked, which a live
// light userdata pinned by its creator is not; the suite does not pin
// this corner).
func weakCollectable(v LValue) bool {
	switch v.(type) {
	case *LTable, *LFunction, *LState, *LUserData:
		return true
	}
	return false
}

// clear drops the dead entries: array cells (integer keys — only
// value-weakness applies) and the dict parts (either component's
// weakness). Removal goes through the RawSet paths so the keys/k2i
// iteration bookkeeping stays exactly what a nil assignment leaves.
func (w *weakSweep) clear() {
	for _, tb := range w.weak {
		kmode, vmode := weakMode(tb)
		if vmode {
			for i, v := range tb.array {
				if v != LNil && weakCollectable(v) && !w.marked[v] {
					tb.array[i] = LNil
				}
			}
		}
		if kmode || vmode {
			for k, v := range tb.dict {
				if (vmode && v != LNil && weakCollectable(v) && !w.marked[v]) ||
					(kmode && weakCollectable(k) && !w.marked[k]) {
					tb.RawSetH(k, LNil)
				}
			}
			for s, v := range tb.strdict {
				if vmode && v != LNil && weakCollectable(v) && !w.marked[v] {
					tb.RawSetString(s, LNil)
				}
			}
		}
	}
}

// __gc finalizers (U5f; db.lua:915's debug-info battery). Stock couples
// __gc to its collector: luaC_checkfinalizer separates the object at
// setmetatable time, the GC cycle moves it to g->tmudata, GCTM (from
// luaC_checkGC at OP_NEWTABLE/OP_CLOSURE/OP_CONCAT, a forced full
// collection, or lua_close) calls the metamethod. The fork's collector
// is Go's GC, so an object's death is observable only through
// runtime.SetFinalizer: registration mirrors checkfinalizer at
// SetMetatable time, the callback (on Go's finalizer goroutine — it must
// not touch any LState) only enqueues on the Global queue, and
// drainFinalizers is GCTM's driver at the stock drain points.
//
// Approximations (divergence ledger row): a metatable mutated to gain or
// lose __gc AFTER setmetatable never re-registers (stock checks at
// separation); an object finalizing because its queue slot holds the
// last reference resurrects it until the drain runs (stock's tmudata
// link does the same); a self-referential object+metatable cycle may
// never finalizer-fire (Go refuses finalizers on self-reachable
// objects) — the corpus's finalizer metatables never reference the
// finalized object.
func (ls *LState) syncFinalizer(obj, mt LValue) {
	has := false
	if tb, ok := mt.(*LTable); ok {
		if tb.RawGetString("__gc") != LNil {
			has = true
		}
	}
	g := ls.G
	switch v := obj.(type) {
	case *LTable:
		if has {
			g.trackFinalizableTable(v)
			runtime.SetFinalizer(v, func(t *LTable) { g.enqueueFinalizer(t) })
		} else {
			runtime.SetFinalizer(v, nil)
		}
	case *LUserData:
		if v.Light {
			return // stock finalizes tables and full userdata only
		}
		if has {
			g.trackFinalizableUserData(v)
			runtime.SetFinalizer(v, func(u *LUserData) { g.enqueueFinalizer(u) })
		} else {
			runtime.SetFinalizer(v, nil)
		}
	}
}

// trackFinalizableTable/UserData record a registration for Close's
// clearRegisteredFinalizers sweep (value.go's finReg comment: the weak
// lists must not pin the objects — that would defeat mid-life firing).
func (g *Global) trackFinalizableTable(v *LTable) {
	g.finMu.Lock()
	g.finRegT = append(g.finRegT, weak.Make(v))
	g.finMu.Unlock()
}

func (g *Global) trackFinalizableUserData(v *LUserData) {
	g.finMu.Lock()
	g.finRegU = append(g.finRegU, weak.Make(v))
	g.finMu.Unlock()
}

// clearRegisteredFinalizers unregisters every finalizer this state
// registered — Close's last GC-facing step, after drainFinalizers. The
// entries that still hold live objects (the io standard streams;
// anything a script left under a global) are exactly the ones whose
// pending finalizer would otherwise pin the closed state's whole graph
// via the closure's captured Global. Objects already collected (or
// already finalized) skip — weak.Value reports nil, and SetFinalizer on
// a dead-pending object is the runtime's race to win; a straggler
// enqueue lands on the orphaned finQ and dies with the state's cycle.
func (g *Global) clearRegisteredFinalizers() {
	g.finMu.Lock()
	for _, p := range g.finRegT {
		if v := p.Value(); v != nil {
			runtime.SetFinalizer(v, nil)
		}
	}
	for _, p := range g.finRegU {
		if v := p.Value(); v != nil {
			runtime.SetFinalizer(v, nil)
		}
	}
	g.finRegT, g.finRegU = nil, nil
	g.finMu.Unlock()
}

// enqueueFinalizer is the SetFinalizer callback body — the ONLY code that
// runs on Go's finalizer goroutine. The flag store happens under the
// queue lock so it can never be lost against drainFinalizers' empty-queue
// clear (the two serialize; a callback appending before the drain's
// check is seen, one appending after re-raises the flag).
func (g *Global) enqueueFinalizer(lv LValue) {
	g.finMu.Lock()
	g.finQ = append(g.finQ, lv)
	g.finPending.Store(true)
	g.finMu.Unlock()
}

// drainFinalizers runs the queued __gc metamethods on the running thread
// (GCTM over g->tmudata). The atomic gate keeps the per-instruction cost
// one load; finDepth suppresses nested drains — a finalizer's own
// allocation sites leave THEIR objects queued for a later drain (stock's
// GCSTPGC around GCTM).
func (ls *LState) drainFinalizers() {
	g := ls.G
	if g == nil || !g.finPending.Load() || g.finDepth > 0 {
		return
	}
	g.finDepth++
	defer func() { g.finDepth-- }()
	for {
		g.finMu.Lock()
		if len(g.finQ) == 0 {
			g.finPending.Store(false)
			g.finMu.Unlock()
			return
		}
		q := g.finQ
		g.finQ = nil
		g.finMu.Unlock()
		for _, obj := range q {
			ls.runFinalizer(obj)
		}
	}
}

// runFinalizer is stock GCTM for one object: __gc re-fetched at RUN time
// (removed/absent skips — the queue's copy may be stale), hooks blocked
// (allowhook=0), the caller frame marked CIST_FIN for the call so
// getinfo(1) inside the metamethod resolves ("__gc", "metamethod"), a
// protected 0-result call (luaD_pcall — noyield, so a yield inside __gc
// raises the boundary error and is warned like any other failure), and
// an error warned to stderr then dropped (luaE_warnerror + the object
// popped).
func (ls *LState) runFinalizer(obj LValue) {
	tm := ls.metaOp1(obj, "__gc")
	if tm == LNil {
		return
	}
	saveBusy := ls.hookBusy
	ls.hookBusy = true
	cf := ls.currentFrame
	if cf != nil {
		cf.Fin = true
	}
	top := ls.reg.Top()
	ls.reg.Push(tm)
	ls.reg.Push(obj)
	err := ls.PCall(1, 0, nil)
	if cf != nil {
		cf.Fin = false
	}
	ls.hookBusy = saveBusy
	ls.reg.SetTop(top)
	if err != nil {
		msg := "error object is not a string"
		if ae, ok := err.(*ApiError); ok {
			if s, ok := ae.Object.(LString); ok {
				msg = string(s)
			}
		} else {
			msg = err.Error()
		}
		// stock luaE_warnerror over the standalone driver's warnf
		fmt.Fprintf(os.Stderr, "Lua warning: error in __gc (%s)\n", msg)
	}
}
