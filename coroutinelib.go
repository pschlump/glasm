package lua

func OpenCoroutine(L *LState) int {
	// TODO: Tie module name to contents of linit.go?
	mod := L.RegisterModule(CoroutineLibName, coFuncs)
	L.Push(mod)
	return 1
}

var coFuncs = map[string]LGFunction{
	"create":      coCreate,
	"yield":       coYield,
	"resume":      coResume,
	"running":     coRunning,
	"status":      coStatus,
	"wrap":        coWrap,
	"close":       coClose,
	"isyieldable": coIsYieldable,
}

func coCreate(L *LState) int {
	fn := L.CheckFunction(1)
	newthread, _ := L.NewThread()
	base := 0
	newthread.stack.Push(callFrame{
		Fn:         fn,
		Pc:         0,
		Base:       base,
		LocalBase:  base + 1,
		ReturnBase: base,
		NArgs:      0,
		NRet:       MultRet,
		Parent:     nil,
		TailCall:   0,
	})
	L.Push(newthread)
	return 1
}

func coYield(L *LState) int {
	// U5d (row 22, stock lua_yieldk's yieldable guard): a yield inside a
	// Go-driven call region — anything under ls.Call (sort comparators,
	// gsub replacements, the tostring/concat metamethod dispatch, __close
	// under the error paths) — raises the CATCHABLE boundary error; from
	// the main thread, the outside-coroutine error. Both are
	// luaG_runerror at the yield C frame: no position, bare text (a
	// pcall marker in the frame stack catches them; coroutine.lua:341).
	if L == L.G.MainThread {
		L.raiseErrorAt(0, "attempt to yield from outside a coroutine")
	}
	if L.noyieldDepth > 0 {
		L.raiseErrorAt(0, "attempt to yield across a C-call boundary")
	}
	return -1
}

// isResumeAncestor reports whether th is an ancestor of cur in the resume
// chain (U4d; stock's L->ci != &L->base_ci structural check): a thread
// blocked in threadRun waiting on a child it resumed is NORMAL, not
// suspended — the chain from CurrentThread up through Parent links is
// exactly that set (the main thread mid-run included).
func isResumeAncestor(cur, th *LState) bool {
	for p := cur.Parent; p != nil; p = p.Parent {
		if p == th {
			return true
		}
	}
	return false
}

// resumeError reports a failed resume (U4d; stock resume_error): the
// message is returned as false + msg through coroutine.resume, or raised
// (with the caller's position, stock auxwrap's luaL_where(L,1)) through
// a wrap.
func resumeError(L *LState, th *LState, msg string) int {
	if th.wrapped {
		L.RaiseError("%s", msg)
		return 0
	}
	L.Push(LFalse)
	L.Push(LString(msg))
	return 2
}

// resumeEntryMarker restructures a not-yet-started coroutine whose entry
// function IS the pcall/xpcall builtin (U5d, row 22 —
// coroutine.create(pcall), official coroutine.lua:277): the base frame
// becomes the protected marker and the payload — already staged at
// register 1 by the resume — becomes its body frame, so a yield inside
// the payload suspends the thread instead of unwinding the Go stack.
// Degenerate arities and non-function handlers stay plain so the
// builtin's own checks raise. Returns false on the plain path.
func resumeEntryMarker(th *LState, cf *callFrame, nargs int) bool {
	fn := cf.Fn
	kind := uint8(0)
	if fn != nil && fn.IsG {
		if fn == th.G.builtinPcall && nargs >= 1 {
			kind = 1
		} else if fn == th.G.builtinXPCall && nargs >= 2 && th.reg.Get(2).Type() == LTFunction {
			kind = 2
		}
	}
	if kind == 0 {
		return false
	}
	handler := LNil
	markerNArgs := nargs
	if kind == 2 {
		handler = th.reg.Get(2)
		th.reg.CopyRange(2, 3, -1, nargs-2)
		markerNArgs = nargs - 1
	}
	cf.Pcall = kind
	cf.PcallHandler = handler
	cf.Base = 0
	cf.LocalBase = 1
	cf.ReturnBase = 0
	cf.NArgs = markerNArgs
	lv := th.reg.Get(1)
	// a payload that is ITSELF a marker builtin (resume(co, pcall, f)) is
	// a nested marker — the builtin's Go body PCalls noyield and loses the
	// continuation (same rule as pushPcallMarker's body resolution)
	bodyArgs := nargs - 1
	if kind == 2 {
		bodyArgs = nargs - 2
	}
	if kind2 := markerKind(th, lv, 1, bodyArgs); kind2 != 0 {
		return pushPcallMarker(th, cf, lv, 1, bodyArgs, MultRet, kind2, false)
	}
	bfn, callee, ccmt := th.resolveCall(lv, 2)
	bodyNArgs := nargs - 1 + ccmt
	if kind == 2 {
		bodyNArgs = nargs - 2 + ccmt
	}
	th.pushCallFrame(callFrame{
		Fn:         bfn,
		Pc:         0,
		Base:       1,
		LocalBase:  2,
		ReturnBase: 1,
		NArgs:      bodyNArgs,
		NRet:       MultRet,
		Parent:     cf,
		TailCall:   0,
		CCmt:       ccmt,
	}, callee, false)
	return true
}

func coResume(L *LState) int {
	th := L.CheckThread(1)
	// U4d stock pin (lua_resume's guards, stock order): only a fresh or
	// suspended coroutine resumes. A running one (CurrentThread) or a
	// normal one (resume-chain ancestor) reports non-suspended; a
	// finished/errored one reports dead; then the nesting ceiling. The
	// old fork texts were gopherlua's ("can not resume a ... thread") —
	// nothing live pins them (_glua-tests/coroutine.lua is row-22
	// skipped), and without the normal guard, resuming a normal thread
	// re-entered threadRun and recursed to a fatal Go stack overflow.
	if L.G.CurrentThread == th || isResumeAncestor(L.G.CurrentThread, th) {
		return resumeError(L, th, "cannot resume non-suspended coroutine")
	}
	if th.Dead {
		return resumeError(L, th, "cannot resume dead coroutine")
	}
	if L.G.resumeDepth >= luaMaxCCalls {
		return resumeError(L, th, "C stack overflow")
	}
	L.G.resumeDepth++
	defer func() { L.G.resumeDepth-- }()
	th.Parent = L
	L.G.CurrentThread = th
	if !th.isStarted() {
		cf := th.stack.Last()
		th.currentFrame = cf
		th.SetTop(0)
		nargs := L.GetTop() - 1
		L.XMoveTo(th, nargs)
		if !resumeEntryMarker(th, cf, nargs) {
			cf.NArgs = nargs
			th.initCallFrame(cf)
		}
		th.Panic = panicWithoutTraceback
		// U5f: stock lua_resume calls the entry function (luaD_call) — the
		// call hook fires for the entry frame (a vararg body defers to its
		// OP_VARARGPREP; the marker arm's payload is the frame on top)
		th.hookCallEvent(th.currentFrame)
	} else {
		nargs := L.GetTop() - 1
		L.XMoveTo(th, nargs)
	}
	top := L.GetTop()
	threadRun(th)
	return L.GetTop() - top
}

// coRunning ports 5.3+ coroutine.running: the running thread (the main
// thread included — 5.1 returned nil for it) plus an is-main flag.
func coRunning(L *LState) int {
	th := L.G.CurrentThread
	L.Push(th)
	L.Push(LBool(th == L.G.MainThread))
	return 2
}

func coStatus(L *LState) int {
	L.Push(LString(L.Status(L.CheckThread(1))))
	return 1
}

func wrapaux(L *LState) int {
	L.Insert(L.ToThread(UpvalueIndex(1)), 1)
	return coResume(L)
}

func coWrap(L *LState) int {
	coCreate(L)
	L.CheckThread(L.GetTop()).wrapped = true
	v := L.Get(L.GetTop())
	L.Pop(1)
	L.Push(L.NewClosure(wrapaux, v))
	return 1
}

// coOptThread: the optional 5.5 coroutine argument — absent means the
// RUNNING coroutine (close/isyieldable gained the no-arg form).
func coOptThread(L *LState) *LState {
	if v := L.Get(1); v != LNil && v.Type() != LTNil {
		return L.CheckThread(1)
	}
	return L
}

// coClose ports luaB_close (5.4 coroutine.close, 5.5 no-arg extension):
// dead or suspended coroutines reset — true, or false + the error object
// the coroutine died with (dead arm) / the failing __close raised
// (suspended arm); a normal coroutine errors; the main thread errors.
// U4d: both closing arms follow stock luaE_resetthread's order — resetCI
// (kill) BEFORE the closes, so a nested coroutine.close(co) inside its own
// __close sees the dead arm and returns true — and close with the 1-arg
// __close(obj) shape (YIELD maps to LUA_OK; err == nil).
func coClose(L *LState) int {
	th := coOptThread(L)
	switch L.Status(th) {
	case "dead":
		err := th.deadErr
		th.deadErr = nil // the status is consumed by the first close
		th.kill()
		if err != nil {
			L.Push(LFalse)
			L.Push(err)
			return 2
		}
		L.Push(LTrue)
		return 1
	case "suspended":
		th.kill() // resetCI first: nested closes see the dead arm
		if obj, failed := th.closeTBCProtected(0, LNil); failed {
			L.Push(LFalse)
			L.Push(obj)
			return 2
		}
		L.Push(LTrue)
		return 1
	case "normal":
		L.RaiseError("cannot close a normal coroutine")
	default: // running
		if th == L.G.MainThread {
			L.RaiseError("cannot close main thread")
		}
		// U4d (stock lua_closethread's self-close): reset (kill) first,
		// run the thread's whole tbc list 1-arg, then unwind EVERY pcall
		// level via the base-level panic — threadRun's closeSelfUnwind
		// arm terminates the thread with the close outcome
		th.kill()
		obj, failed := th.closeTBCProtected(0, LNil)
		panic(&closeSelfUnwind{errobj: obj, ok: !failed})
	}
	return 0
}

// coIsYieldable: a coroutine is yieldable iff it is not the main
// thread AND not inside a Go-driven call region (U5d, stock
// yieldable(L) = L->nny == 0 && !isMain; 5.5's no-arg form asks about
// the running one).
func coIsYieldable(L *LState) int {
	th := coOptThread(L)
	L.Push(LBool(th != L.G.MainThread && th.noyieldDepth == 0))
	return 1
}

//
