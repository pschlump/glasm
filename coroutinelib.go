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
	return -1
}

func coResume(L *LState) int {
	th := L.CheckThread(1)
	if L.G.CurrentThread == th {
		msg := "can not resume a running thread"
		if th.wrapped {
			L.RaiseError("%s", msg)
			return 0
		}
		L.Push(LFalse)
		L.Push(LString(msg))
		return 2
	}
	if th.Dead {
		msg := "can not resume a dead thread"
		if th.wrapped {
			L.RaiseError("%s", msg)
			return 0
		}
		L.Push(LFalse)
		L.Push(LString(msg))
		return 2
	}
	th.Parent = L
	L.G.CurrentThread = th
	if !th.isStarted() {
		cf := th.stack.Last()
		th.currentFrame = cf
		th.SetTop(0)
		nargs := L.GetTop() - 1
		L.XMoveTo(th, nargs)
		cf.NArgs = nargs
		th.initCallFrame(cf)
		th.Panic = panicWithoutTraceback
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
// dead or suspended coroutines reset — true, or false + the error
// object the coroutine died with; a normal coroutine errors; the main
// thread errors. (TBC variables in suspended coroutines get their
// __close calls at U4; today no TBC exists.)
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
		th.kill()
		L.Push(LTrue)
		return 1
	case "normal":
		L.RaiseError("cannot close a normal coroutine")
	default: // running
		if th == L.G.MainThread {
			L.RaiseError("cannot close main thread")
		}
		// stock terminates the running coroutine itself via
		// luaD_throwbaselevel (it never returns); the closest fork shape
		// until the U4 error-path work is the explicit error
		L.RaiseError("cannot close a running coroutine")
	}
	return 0
}

// coIsYieldable: a coroutine is yieldable iff it is not the main
// thread (5.5's no-arg form asks about the running one).
func coIsYieldable(L *LState) int {
	th := coOptThread(L)
	L.Push(LBool(th != L.G.MainThread))
	return 1
}

//
