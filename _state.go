package lua

import (
	"context"
	"fmt"
	"io"
	"math"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pschlump/glasm/parse"
)

const MultRet = -1
const RegistryIndex = -10000
const EnvironIndex = -10001
const GlobalsIndex = -10002

// errorFrameReserve is the fork's ERRORSTACKSIZE analog (stock ldo.c
// luaD_growstack reserves extra frames before raising "stack overflow" so
// the error message AND the message handler have room; a second overflow
// inside that reserve is luaD_errerr's "error in error handling"). The
// fork's fixed frame array cannot grow — PCall's errfunc arm unwinds this
// many dying frames instead (U5a; divergence ledger row).
const errorFrameReserve = 16

/* ApiError {{{ */

type ApiError struct {
	Type       ApiErrorType
	Object     LValue
	StackTrace string
	// Underlying error. This attribute is set only if the Type is ApiErrorFile or ApiErrorSyntax
	Cause error
}

func newApiError(code ApiErrorType, object LValue) *ApiError {
	return &ApiError{code, object, "", nil}
}

func newApiErrorS(code ApiErrorType, message string) *ApiError {
	return newApiError(code, LString(message))
}

func newApiErrorE(code ApiErrorType, err error) *ApiError {
	return &ApiError{code, LString(err.Error()), "", err}
}

func (e *ApiError) Error() string {
	if len(e.StackTrace) > 0 {
		return fmt.Sprintf("%s\n%s", e.Object.String(), e.StackTrace)
	}
	return e.Object.String()
}

type ApiErrorType int

const (
	ApiErrorSyntax ApiErrorType = iota
	ApiErrorFile
	ApiErrorRun
	ApiErrorError
	ApiErrorPanic
)

/* }}} */

/* ResumeState {{{ */

type ResumeState int

const (
	ResumeOK ResumeState = iota
	ResumeYield
	ResumeError
)

/* }}} */

/* P {{{ */

type P struct {
	Fn      LValue
	NRet    int
	Protect bool
	Handler *LFunction
}

/* }}} */

/* Options {{{ */

// Options is a configuration that is used to create a new LState.
type Options struct {
	// Call stack size. This defaults to `lua.CallStackSize`.
	CallStackSize int
	// Data stack size. This defaults to `lua.RegistrySize`.
	RegistrySize int
	// Allow the registry to grow from the registry size specified up to a value of RegistryMaxSize. A value of 0
	// indicates no growth is permitted. The registry will not shrink again after any growth.
	RegistryMaxSize int
	// If growth is enabled, step up by an additional `RegistryGrowStep` each time to avoid having to resize too often.
	// This defaults to `lua.RegistryGrowStep`
	RegistryGrowStep int
	// Controls whether or not libraries are opened by default
	SkipOpenLibs bool
	// Tells whether a Go stacktrace should be included in a Lua stacktrace when panics occur.
	IncludeGoStackTrace bool
	// If `MinimizeStackMemory` is set, the call stack will be automatically grown or shrank up to a limit of
	// `CallStackSize` in order to minimize memory usage. This does incur a slight performance penalty.
	MinimizeStackMemory bool
}

/* }}} */

/* Debug {{{ */

type Debug struct {
	frame           *callFrame
	Name            string
	NameWhat        string
	What            string
	Source          string
	CurrentLine     int
	NUpvalues       int
	LineDefined     int
	LastLineDefined int
	// U5c (stock lua_Debug): 'u' adds nparams/isvararg, 't' adds
	// istailcall/extraargs — extraargs is the __call-chain count (the
	// ci's MAX_CCMT bits, ldebug.c:359), NOT the vararg count: a plain
	// vararg call answers 0, a chain-fed call answers its hop count.
	NParams    int
	IsVarArg   bool
	IsTailCall bool
	ExtraArgs  int
	// 'r' ftransfer/ntransfer (no hooks in the fork: always 0) and 'L'
	// activelines (nil for C functions, collectvalidlines' table).
	FTransfer   int
	NTransfer   int
	ActiveLines *LTable
}

/* }}} */

/* callFrame {{{ */

type callFrame struct {
	Idx        int
	Fn         *LFunction
	Parent     *callFrame
	Pc         int
	Base       int
	LocalBase  int
	ReturnBase int
	NArgs      int
	NRet       int
	TailCall   int
	// CCmt is the __call-chain replacement count that reached this
	// frame (stock's ci callstatus MAX_CCMT bits): getinfo 't' reports
	// it as extraargs. Reset per call site (stock resets it per precall).
	CCmt int
	// Pcall marks a continuation/protected marker frame (U5d, row 22 —
	// stock models these as C frames with lua_callk/lua_pcallk
	// continuations; the fork carries the semantics as DATA on the
	// frame so a yield suspends through them instead of unwinding the
	// Go stack):
	//   0    plain frame
	//   1    pcall  — body results prefix true; errors stage [false, obj]
	//   2    xpcall — as pcall, msgh in PcallHandler run AT raise
	//   3    pairs  — __pairs call, body results pad to NRet (pairscont)
	//   4/5  close continuation: a VM-called __close (4 = from OP_CLOSE,
	//        5 = from a RETURN; ContRA/ContB resume the interrupted
	//        instruction's tail)
	//   7    error-path close continuation: a __close(obj, errobj) from a
	//        marker catch's yieldable sweep (stock finishpcallk); the
	//        in-flight object rides the marker's LocalBase slot
	//   6    metamethod continuation: a VM-dispatched Lua metamethod
	//        (MMBIN/getfield/order/concat/...); vmFinishMM completes the
	//        owner's interrupted instruction when it returns
	// A marker never executes code; the return/error/resume paths
	// complete it (completePcallMarker, the close continuations, and
	// vmFinishMM).
	Pcall        uint8
	PcallHandler LValue
	ContRA       int
	ContB        int
	// Hooked is stock's CIST_HOOKED (U5f, luaD_hook): the frame an event
	// fired ON is marked while the hook function runs, so name resolution
	// reports ("?", "hook") for the hook's own frame (funcnamefromcall's
	// first arm) and getinfo 'r' reports the thread's transfer window.
	// Frame slots are stable (segment arrays never move), so the flag is
	// safe to hold across the hook's nested frames.
	Hooked bool
	// Fin is stock's CIST_FIN (U5f, GCTM): the frame a __gc finalizer is
	// being called on behalf of is marked while the metamethod runs, so
	// the finalizer's own frame resolves ("__gc", "metamethod") in name
	// queries (funcnamefromcall's FIN arm — checked before any call-site
	// walk, and whatever the caller's current instruction is). Transient:
	// set/cleared around the call, like the stock bit.
	Fin bool
	// PendRet/PendBase (U5f, stock resume → unroll → finishCfunc →
	// luaD_poscall): a plain G call that suspended mid-body
	// (coroutine.yield) left this Lua frame mid-instruction; the resume
	// stages its arguments at the interrupted call's result window, and
	// mainLoop's entry replays the poscall — adjusting the staged values
	// to the call's wanted count. PendRet holds want+1 (0 = none,
	// MultRet records nothing — all staged values are kept), PendBase the
	// window's absolute register (the suspended G frame's ReturnBase).
	PendRet  int
	PendBase int
}

type callFrameStack interface {
	Push(v callFrame)
	Pop() *callFrame
	Last() *callFrame

	SetSp(sp int)
	Sp() int
	At(sp int) *callFrame

	IsFull() bool
	IsEmpty() bool

	FreeAll()
}

type fixedCallFrameStack struct {
	array []callFrame
	sp    int
}

func newFixedCallFrameStack(size int) callFrameStack {
	return &fixedCallFrameStack{
		array: make([]callFrame, size),
		sp:    0,
	}
}

func (cs *fixedCallFrameStack) IsEmpty() bool { return cs.sp == 0 }

func (cs *fixedCallFrameStack) IsFull() bool { return cs.sp == len(cs.array) }

func (cs *fixedCallFrameStack) Clear() {
	cs.sp = 0
}

func (cs *fixedCallFrameStack) Push(v callFrame) {
	cs.array[cs.sp] = v
	cs.array[cs.sp].Idx = cs.sp
	cs.sp++
}

func (cs *fixedCallFrameStack) Sp() int {
	return cs.sp
}

func (cs *fixedCallFrameStack) SetSp(sp int) {
	cs.sp = sp
}

func (cs *fixedCallFrameStack) Last() *callFrame {
	if cs.sp == 0 {
		return nil
	}
	return &cs.array[cs.sp-1]
}

func (cs *fixedCallFrameStack) At(sp int) *callFrame {
	return &cs.array[sp]
}

func (cs *fixedCallFrameStack) Pop() *callFrame {
	cs.sp--
	return &cs.array[cs.sp]
}

func (cs *fixedCallFrameStack) FreeAll() {
	// nothing to do for fixed callframestack
}

// FramesPerSegment should be a power of 2 constant for performance reasons. It will allow the go compiler to change
// the divs and mods into bitshifts. Max is 256 due to current use of uint8 to count how many frames in a segment are
// used.
const FramesPerSegment = 8

type callFrameStackSegment struct {
	array [FramesPerSegment]callFrame
}
type segIdx uint16
type autoGrowingCallFrameStack struct {
	segments []*callFrameStackSegment
	segIdx   segIdx
	// segSp is the number of frames in the current segment which are used. Full 'sp' value is segIdx * FramesPerSegment + segSp.
	// It points to the next stack slot to use, so 0 means to use the 0th element in the segment, and a value of
	// FramesPerSegment indicates that the segment is full and cannot accommodate another frame.
	segSp uint8
}

var segmentPool sync.Pool

func newCallFrameStackSegment() *callFrameStackSegment {
	seg := segmentPool.Get()
	if seg == nil {
		return &callFrameStackSegment{}
	}
	return seg.(*callFrameStackSegment)
}

func freeCallFrameStackSegment(seg *callFrameStackSegment) {
	segmentPool.Put(seg)
}

// newCallFrameStack allocates a new stack for a lua state, which will auto grow up to a max size of at least maxSize.
// it will actually grow up to the next segment size multiple after maxSize, where the segment size is dictated by
// FramesPerSegment.
func newAutoGrowingCallFrameStack(maxSize int) callFrameStack {
	cs := &autoGrowingCallFrameStack{
		segments: make([]*callFrameStackSegment, (maxSize+(FramesPerSegment-1))/FramesPerSegment),
		segIdx:   0,
	}
	cs.segments[0] = newCallFrameStackSegment()
	return cs
}

func (cs *autoGrowingCallFrameStack) IsEmpty() bool {
	return cs.segIdx == 0 && cs.segSp == 0
}

// IsFull returns true if the stack cannot receive any more stack pushes without overflowing
func (cs *autoGrowingCallFrameStack) IsFull() bool {
	return int(cs.segIdx) == len(cs.segments) && cs.segSp >= FramesPerSegment
}

func (cs *autoGrowingCallFrameStack) Clear() {
	for i := segIdx(1); i <= cs.segIdx; i++ {
		freeCallFrameStackSegment(cs.segments[i])
		cs.segments[i] = nil
	}
	cs.segIdx = 0
	cs.segSp = 0
}

func (cs *autoGrowingCallFrameStack) FreeAll() {
	for i := segIdx(0); i <= cs.segIdx; i++ {
		freeCallFrameStackSegment(cs.segments[i])
		cs.segments[i] = nil
	}
}

// Push pushes the passed callFrame onto the stack. it panics if the stack is full, caller should call IsFull() before
// invoking this to avoid this.
func (cs *autoGrowingCallFrameStack) Push(v callFrame) {
	curSeg := cs.segments[cs.segIdx]
	if cs.segSp >= FramesPerSegment {
		// segment full, push new segment if allowed
		if cs.segIdx < segIdx(len(cs.segments)-1) {
			curSeg = newCallFrameStackSegment()
			cs.segIdx++
			cs.segments[cs.segIdx] = curSeg
			cs.segSp = 0
		} else {
			panic("lua callstack overflow")
		}
	}
	curSeg.array[cs.segSp] = v
	curSeg.array[cs.segSp].Idx = int(cs.segSp) + FramesPerSegment*int(cs.segIdx)
	cs.segSp++
}

// Sp retrieves the current stack depth, which is the number of frames currently pushed on the stack.
func (cs *autoGrowingCallFrameStack) Sp() int {
	return int(cs.segSp) + int(cs.segIdx)*FramesPerSegment
}

// SetSp can be used to rapidly unwind the stack, freeing all stack frames on the way. It should not be used to
// allocate new stack space, use Push() for that.
func (cs *autoGrowingCallFrameStack) SetSp(sp int) {
	desiredSegIdx := segIdx(sp / FramesPerSegment)
	desiredFramesInLastSeg := uint8(sp % FramesPerSegment)
	for {
		if cs.segIdx <= desiredSegIdx {
			break
		}
		freeCallFrameStackSegment(cs.segments[cs.segIdx])
		cs.segments[cs.segIdx] = nil
		cs.segIdx--
	}
	cs.segSp = desiredFramesInLastSeg
}

func (cs *autoGrowingCallFrameStack) Last() *callFrame {
	curSeg := cs.segments[cs.segIdx]
	segSp := cs.segSp
	if segSp == 0 {
		if cs.segIdx == 0 {
			return nil
		}
		curSeg = cs.segments[cs.segIdx-1]
		segSp = FramesPerSegment
	}
	return &curSeg.array[segSp-1]
}

func (cs *autoGrowingCallFrameStack) At(sp int) *callFrame {
	segIdx := segIdx(sp / FramesPerSegment)
	frameIdx := uint8(sp % FramesPerSegment)
	return &cs.segments[segIdx].array[frameIdx]
}

// Pop pops off the most recent stack frame and returns it
func (cs *autoGrowingCallFrameStack) Pop() *callFrame {
	curSeg := cs.segments[cs.segIdx]
	if cs.segSp == 0 {
		if cs.segIdx == 0 {
			// stack empty
			return nil
		}
		freeCallFrameStackSegment(curSeg)
		cs.segments[cs.segIdx] = nil
		cs.segIdx--
		cs.segSp = FramesPerSegment
		curSeg = cs.segments[cs.segIdx]
	}
	cs.segSp--
	return &curSeg.array[cs.segSp]
}

/* }}} */

/* registry {{{ */

type registryHandler interface {
	registryOverflow()
}
type registry struct {
	array   []LValue
	top     int
	growBy  int
	maxSize int
	alloc   *allocator
	handler registryHandler
}

func newRegistry(handler registryHandler, initialSize int, growBy int, maxSize int, alloc *allocator) *registry {
	return &registry{make([]LValue, initialSize), 0, growBy, maxSize, alloc, handler}
}

func (rg *registry) checkSize(requiredSize int) { // +inline-start
	if requiredSize > cap(rg.array) {
		rg.resize(requiredSize)
	}
} // +inline-end

func (rg *registry) resize(requiredSize int) { // +inline-start
	newSize := requiredSize + rg.growBy // give some padding
	if newSize > rg.maxSize {
		newSize = rg.maxSize
	}
	if newSize < requiredSize {
		rg.handler.registryOverflow()
		return
	}
	rg.forceResize(newSize)
} // +inline-end

func (rg *registry) forceResize(newSize int) {
	newSlice := make([]LValue, newSize)
	copy(newSlice, rg.array[:rg.top]) // should we copy the area beyond top? there shouldn't be any valid values there so it shouldn't be necessary.
	rg.array = newSlice
}

func (rg *registry) SetTop(topi int) { // +inline-start
	// +inline-call rg.checkSize topi
	oldtopi := rg.top
	rg.top = topi
	for i := oldtopi; i < rg.top; i++ {
		rg.array[i] = LNil
	}
	// values beyond top don't need to be valid LValues, so setting them to nil is fine
	// setting them to nil rather than LNil lets us invoke the golang memclr opto
	if rg.top < oldtopi {
		nilRange := rg.array[rg.top:oldtopi]
		for i := range nilRange {
			nilRange[i] = nil
		}
	}
	//for i := rg.top; i < oldtop; i++ {
	//	rg.array[i] = LNil
	//}
} // +inline-end

func (rg *registry) Top() int {
	return rg.top
}

func (rg *registry) Push(v LValue) {
	newSize := rg.top + 1
	// +inline-call rg.checkSize newSize
	rg.array[rg.top] = v
	rg.top++
}

func (rg *registry) Pop() LValue {
	v := rg.array[rg.top-1]
	rg.array[rg.top-1] = LNil
	rg.top--
	return v
}

func (rg *registry) Get(reg int) LValue {
	return rg.array[reg]
}

// CopyRange will move a section of values from index `start` to index `regv`
// It will move `n` values.
// `limit` specifies the maximum end range that can be copied from. If it's set to -1, then it defaults to stopping at
// the top of the registry (values beyond the top are not initialized, so if specifying an alternative `limit` you should
// pass a value <= rg.top.
// If start+n is beyond the limit, then nil values will be copied to the destination slots.
// After the copy, the registry is truncated to be at the end of the copied range, ie the original of the copied values
// are nilled out. (So top will be regv+n)
// CopyRange should ideally be renamed to MoveRange.
func (rg *registry) CopyRange(regv, start, limit, n int) { // +inline-start
	newSize := regv + n
	// +inline-call rg.checkSize newSize
	if limit == -1 || limit > rg.top {
		limit = rg.top
	}
	for i := 0; i < n; i++ {
		srcIdx := start + i
		if srcIdx >= limit || srcIdx < 0 {
			rg.array[regv+i] = LNil
		} else {
			rg.array[regv+i] = rg.array[srcIdx]
		}
	}

	// values beyond top don't need to be valid LValues, so setting them to nil is fine
	// setting them to nil rather than LNil lets us invoke the golang memclr opto
	oldtop := rg.top
	rg.top = regv + n
	if rg.top < oldtop {
		nilRange := rg.array[rg.top:oldtop]
		for i := range nilRange {
			nilRange[i] = nil
		}
	}
} // +inline-end

// FillNil fills the registry with nil values from regm to regm+n and then sets the registry top to regm+n
func (rg *registry) FillNil(regm, n int) { // +inline-start
	newSize := regm + n
	// +inline-call rg.checkSize newSize
	for i := 0; i < n; i++ {
		rg.array[regm+i] = LNil
	}
	// values beyond top don't need to be valid LValues, so setting them to nil is fine
	// setting them to nil rather than LNil lets us invoke the golang memclr opto
	oldtop := rg.top
	rg.top = regm + n
	if rg.top < oldtop {
		nilRange := rg.array[rg.top:oldtop]
		for i := range nilRange {
			nilRange[i] = nil
		}
	}
} // +inline-end

func (rg *registry) Insert(value LValue, reg int) {
	top := rg.Top()
	if reg >= top {
		// +inline-call rg.Set reg value
		return
	}
	top--
	for ; top >= reg; top-- {
		// FIXME consider using copy() here if Insert() is called enough
		// +inline-call rg.Set top+1 rg.Get(top)
	}
	// +inline-call rg.Set reg value
}

func (rg *registry) Set(regi int, vali LValue) { // +inline-start
	newSize := regi + 1
	// +inline-call rg.checkSize newSize
	rg.array[regi] = vali
	if regi >= rg.top {
		rg.top = regi + 1
	}
} // +inline-end

func (rg *registry) SetNumber(regi int, vali LNumber) { // +inline-start
	newSize := regi + 1
	// +inline-call rg.checkSize newSize
	rg.array[regi] = rg.alloc.LNumber2I(vali)
	if regi >= rg.top {
		rg.top = regi + 1
	}
} // +inline-end

func (rg *registry) SetInteger(regi int, vali LInteger) { // +inline-start
	newSize := regi + 1
	// +inline-call rg.checkSize newSize
	rg.array[regi] = rg.alloc.LInteger2I(vali)
	if regi >= rg.top {
		rg.top = regi + 1
	}
} // +inline-end

func (rg *registry) IsFull() bool {
	return rg.top >= cap(rg.array)
}

/* }}} */

/* Global {{{ */

func newGlobal() *Global {
	return &Global{
		MainThread: nil,
		Registry:   newLTable(0, 32),
		Global:     newLTable(0, 64),
		gcparams:   defaultGCParams(),
		builtinMts: make(map[int]LValue),
		tempFiles:  make([]*os.File, 0, 10),
	}
}

/* }}} */

/* package local methods {{{ */

func panicWithTraceback(L *LState) {
	err := newApiError(ApiErrorRun, L.Get(-1))
	err.StackTrace = L.stackTrace(nil, 0)
	panic(err)
}

func panicWithoutTraceback(L *LState) {
	err := newApiError(ApiErrorRun, L.Get(-1))
	panic(err)
}

func newLState(options Options) *LState {
	al := newAllocator(32)
	ls := &LState{
		G:       newGlobal(),
		Parent:  nil,
		Panic:   panicWithTraceback,
		Dead:    false,
		Options: options,

		stop:         0,
		alloc:        al,
		currentFrame: nil,
		wrapped:      false,
		uvcache:      nil,
		hasErrorFunc: false,
		mainLoop:     mainLoop,
		ctx:          nil,
	}
	if options.MinimizeStackMemory {
		ls.stack = newAutoGrowingCallFrameStack(options.CallStackSize)
	} else {
		ls.stack = newFixedCallFrameStack(options.CallStackSize)
	}
	ls.reg = newRegistry(ls, options.RegistrySize, options.RegistryGrowStep, options.RegistryMaxSize, al)
	ls.Env = ls.G.Global
	return ls
}

func (ls *LState) printReg() {
	println("-------------------------")
	println("thread:", ls)
	println("top:", ls.reg.Top())
	if ls.currentFrame != nil {
		println("function base:", ls.currentFrame.Base)
		println("return base:", ls.currentFrame.ReturnBase)
	} else {
		println("(vm not started)")
	}
	println("local base:", ls.currentLocalBase())
	for i := 0; i < ls.reg.Top(); i++ {
		println(i, ls.reg.Get(i).String())
	}
	println("-------------------------")
}

func (ls *LState) printCallStack() {
	println("-------------------------")
	for i := 0; i < ls.stack.Sp(); i++ {
		print(i)
		print(" ")
		frame := ls.stack.At(i)
		if frame == nil {
			break
		}
		if frame.Fn.IsG {
			println("IsG:", true, "Frame:", frame, "Fn:", frame.Fn)
		} else {
			println("IsG:", false, "Frame:", frame, "Fn:", frame.Fn, "pc:", frame.Pc)
		}
	}
	println("-------------------------")
}

func (ls *LState) closeAllUpvalues() { // +inline-start
	for cf := ls.currentFrame; cf != nil; cf = cf.Parent {
		if !cf.Fn.IsG {
			ls.closeUpvalues(cf.LocalBase)
		}
	}
} // +inline-end

func (ls *LState) raiseError(level int, format string, args ...interface{}) {
	// NOTE: upvalues are NOT closed here. The old blanket
	// closeAllUpvalues() also closed the upvalues of frames that
	// SURVIVE the unwind (C Lua closes only above the pcall boundary,
	// luaD_pcall's luaF_close(L, old_base)) — so after a caught error,
	// a surviving closure's SETUPVAL wrote the closed copy while the
	// owning frame's register kept the stale value (fuzzer find, row
	// 44: pcall-then-upvalue-write lost the write on interp, wasm
	// matched stock C). PCall's recovery closes exactly the dying
	// frames' upvalues (closeUpvalues(base)); upvalues are index-based,
	// so nothing dangles between raise and recover.
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	if level > 0 {
		if prefix := ls.stockWhere(level); prefix != "" {
			message = fmt.Sprintf("%v %v", prefix, message)
		}
	}
	if ls.reg.IsFull() {
		// if the registry is full then it won't be possible to push a value, in this case, force a larger size
		ls.reg.forceResize(ls.reg.Top() + 1)
	}
	ls.reg.Push(LString(message))
	ls.Panic(ls)
}

func (ls *LState) findLocal(frame *callFrame, no int) string {
	fn := frame.Fn
	if !fn.IsG {
		if name, ok := fn.LocalName(no, frame.Pc-1); ok {
			return name
		}
	}
	var top int
	if ls.currentFrame == frame {
		top = ls.reg.Top()
	} else if frame.Idx+1 < ls.stack.Sp() {
		top = ls.stack.At(frame.Idx + 1).Base
	} else {
		return ""
	}
	// U5f: stock's temporary arm requires n > 0 — index 0 is not a
	// local (luaF_getlocalname counts from 1 and the generic-slot test
	// guards n > 0), so getlocal(l, 0) answers no name (db.lua:413)
	if no > 0 && top-frame.LocalBase >= no {
		// U5a (stock 5.5.1 luaG_findlocal): a valid slot past the named
		// locals is "(temporary)" — 5.5 dropped 5.4's asterisk — and a
		// C frame's is "(C temporary)"
		if fn.IsG {
			return "(C temporary)"
		}
		return "(temporary)"
	}
	return ""
}

func (ls *LState) where(level int, skipg bool) string {
	dbg, ok := ls.GetStack(level)
	if !ok {
		return ""
	}
	cf := dbg.frame
	proto := cf.Fn.Proto
	sourcename := "[G]"
	if proto != nil {
		// U1: source names follow stock's sigil convention (@file,
		// =literal, or [string "..."]) — ChunkID is the shared renderer;
		// U5f: a stripped chunk's NULL source renders "?" (stock
		// luaG_addinfo's no-source chunkid). An explicit empty chunkname
		// (load(s, "")) is NOT NULL — it renders `[string ""]`
		if proto.SourceName == "" && proto.SourceIsNull {
			sourcename = "?"
		} else {
			sourcename = parse.ChunkID(proto.SourceName)
		}
	} else if skipg {
		return ls.where(level+1, skipg)
	}
	line := ""
	if proto != nil {
		// U5f: no lineinfo (a stripped chunk) → luaG_getfuncline's -1
		if len(proto.DbgSourcePositions) == 0 {
			line = "-1:"
		} else {
			line = fmt.Sprintf("%v:", proto.DbgSourcePositions[cf.Pc-1])
		}
	}
	return fmt.Sprintf("%v:%v", sourcename, line)
}

// stockWhere is luaL_where's semantics (U2f re-pin, D7 direction): the
// position of the frame at 'level' counting the running (G) function as
// level 0 — and EMPTY for non-Lua frames, never skipping past them.
// The old skipg walk landed on the nearest Lua frame, so pcall(error,
// 'x') gained pcall's caller's position where stock has none, and
// error('m', 2) took the wrong frame's line.
func (ls *LState) stockWhere(level int) string {
	dbg, ok := ls.GetStack(level)
	if !ok {
		return ""
	}
	cf := dbg.frame
	if cf.Fn == nil || cf.Fn.IsG || cf.Fn.Proto == nil {
		return ""
	}
	p := cf.Fn.Proto
	// U5f: a stripped chunk's NULL source is luaG_addinfo's fixed literal
	// "?:?:" (the line does not print at all); a source without lineinfo
	// renders luaG_getfuncline's answer, -1. An explicit empty chunkname
	// (load(s, "")) is a real source — it takes the ChunkID path
	// (`[string ""]:N:`), db.lua:73's battery
	if p.SourceName == "" && p.SourceIsNull {
		return "?:?:"
	}
	if len(p.DbgSourcePositions) == 0 {
		return fmt.Sprintf("%v:-1:", parse.ChunkID(p.SourceName))
	}
	return fmt.Sprintf("%v:%v:", parse.ChunkID(p.SourceName), p.DbgSourcePositions[cf.Pc-1])
}

// tbLevels1/tbLevels2 are lauxlib's LEVELS1/LEVELS2: the traceback shows
// the first 10 and the last 11 levels when the stack is deeper than 21.
const (
	tbLevels1 = 10
	tbLevels2 = 11
)

// lastLevel is lauxlib's lastlevel: the number of levels the thread's
// stack answers (doubling then binary search over GetStack).
func (ls *LState) lastLevel() int {
	li, le := 1, 1
	for {
		if _, ok := ls.GetStack(le); !ok {
			break
		}
		li = le
		le *= 2
	}
	for li < le {
		m := (li + le) / 2
		if _, ok := ls.GetStack(m); ok {
			li = m + 1
		} else {
			le = m
		}
	}
	return le - 1
}

// stackTrace is luaL_traceback (U5f): "stack traceback:" then one line
// per level — "\t<short_src>[:<line>]: in <funcname>" — with a
// "\t...\t(skipping N levels)" seam between the first 10 and last 11
// levels of a deep stack, and a "\t(...tail calls...)" marker after a
// tail-called frame. msg nil = C's NULL (no message — an empty-string
// message still prefixes "\n"); the db.traceback wrapper owns the
// message handling.
func (ls *LState) stackTrace(msg *string, level int) string {
	last := ls.lastLevel()
	limit2show := -1
	if last-level > tbLevels1+tbLevels2 {
		limit2show = tbLevels1
	}
	var buf strings.Builder
	if msg != nil {
		buf.WriteString(*msg)
		buf.WriteByte('\n')
	}
	buf.WriteString("stack traceback:")
	for {
		dbg, ok := ls.GetStack(level)
		if !ok {
			break
		}
		level++
		if limit2show >= 0 {
			if limit2show == 0 {
				n := last - level - tbLevels2 + 1
				fmt.Fprintf(&buf, "\n\t...\t(skipping %d levels)", n)
				level += n
				limit2show = -1
				continue
			}
			limit2show--
		}
		fn, _ := ls.GetInfo("Slntf", dbg, LNil)
		shortSrc := parse.ChunkID(dbg.Source)
		if dbg.CurrentLine > 0 {
			fmt.Fprintf(&buf, "\n\t%s:%d: in ", shortSrc, dbg.CurrentLine)
		} else {
			fmt.Fprintf(&buf, "\n\t%s: in ", shortSrc)
		}
		buf.WriteString(ls.tbFuncName(dbg, fn, shortSrc))
		if dbg.IsTailCall {
			buf.WriteString("\n\t(...tail calls...)")
		}
	}
	return buf.String()
}

// tbFuncName is lauxlib's pushfuncname: the call-site name
// ("global 'x'", "method 'x'", "hook '?'", ...) if any; else "main
// chunk"; else the function's name in package.loaded ("function
// 'io.write'" — "_G." stripped); else "function <src:line>" for a Lua
// function or "?" for a C one.
func (ls *LState) tbFuncName(dbg *Debug, fn LValue, shortSrc string) string {
	if dbg.NameWhat != "" {
		return fmt.Sprintf("%s '%s'", dbg.NameWhat, dbg.Name)
	}
	if dbg.What == "main" {
		return "main chunk"
	}
	if f, ok := fn.(*LFunction); ok {
		if name, found := ls.findLoadedFuncName(f); found {
			return fmt.Sprintf("function '%s'", name)
		}
	}
	if dbg.What != "C" {
		return fmt.Sprintf("function <%s:%d>", shortSrc, dbg.LineDefined)
	}
	return "?"
}

// findLoadedFuncName is lauxlib's pushglobalfuncname/findfield: the
// function's path in the registry's _LOADED table, one module level
// deep ("io.write"); a "_G." prefix is stripped. Iteration follows table
// order (Next), as stock's lua_next does.
func (ls *LState) findLoadedFuncName(fn *LFunction) (string, bool) {
	loaded, ok := ls.G.Registry.RawGetString("_LOADED").(*LTable)
	if !ok {
		return "", false
	}
	for k1, v1 := loaded.Next(LNil); k1 != LNil; k1, v1 = loaded.Next(k1) {
		if k1.Type() != LTString {
			continue
		}
		if v1 == LValue(fn) {
			return string(k1.(LString)), true
		}
		if mod, ok := v1.(*LTable); ok {
			for k2, v2 := mod.Next(LNil); k2 != LNil; k2, v2 = mod.Next(k2) {
				if k2.Type() == LTString && v2 == LValue(fn) {
					name := string(k1.(LString)) + "." + string(k2.(LString))
					// pushglobalfuncname strips a leading "_G." (register-
					// module "_G" holds the globals table itself)
					if strings.HasPrefix(name, "_G.") {
						name = name[3:]
					}
					return name, true
				}
			}
		}
	}
	return "", false
}

func (ls *LState) formattedFrameFuncName(fr *callFrame) string {
	name, ischunk := ls.frameFuncName(fr)
	if ischunk {
		return name
	}
	// U5a: VM-dispatched frames (metamethods, for iterators) render
	// stock's namewhat form ("in metamethod 'close'"), not "function '...'";
	// U5f: so does the hook frame's "?" ("in hook '?'", db.lua:313)
	if strings.HasPrefix(name, "metamethod '") || name == "for iterator" || name == "hook '?'" {
		return name
	}
	if name[0] != '(' && name[0] != '<' {
		return fmt.Sprintf("function '%s'", name)
	}
	return fmt.Sprintf("function %s", name)
}

func (ls *LState) rawFrameFuncName(fr *callFrame) string {
	name, _ := ls.frameFuncName(fr)
	// U5a: debug.getinfo 'n' names the event itself (stock
	// funcnamefromcall: name "close", namewhat "metamethod") — the
	// decorated form is the traceback's rendering only
	if strings.HasPrefix(name, "metamethod '") {
		return name[len("metamethod '") : len(name)-1]
	}
	return name
}

func (ls *LState) frameFuncName(fr *callFrame) (string, bool) {
	name, _, ischunk := ls.frameFuncNameFull(fr)
	return name, ischunk
}

// frameFuncNameFull is frameFuncName plus the colon-syntax flag of the
// resolved call site (U3b: stock's argerror namewhat=="method" arm).
func (ls *LState) frameFuncNameFull(fr *callFrame) (string, bool, bool) {
	frame := fr.Parent
	if frame == nil {
		if ls.Parent == nil {
			return "main chunk", false, true
		} else {
			return "corountine", false, true
		}
	}
	// U5f (stock funcnamefromcall's first arm): a frame called while its
	// caller ran a hook — the hook function itself — is named "?" with
	// namewhat "hook" (checked before any call-site/metamethod walk, and
	// whatever the traced caller is). db.lua:312-314's assert + traceback
	// pin the rendering "in hook '?'".
	if frame.Hooked {
		return "hook '?'", false, false
	}
	// U5f (funcnamefromcall's CIST_FIN arm): the caller is running this
	// frame as a __gc finalizer — the frame is the metamethod, named
	// before any call-site walk and whatever the caller's instruction
	if frame.Fin {
		return "metamethod '__gc'", false, false
	}
	if !frame.Fn.IsG {
		pc := frame.Pc - 1
		for _, call := range frame.Fn.Proto.DbgCalls {
			if call.Pc == pc {
				name := call.Name
				if (name == "?" || fr.TailCall > 0) && !fr.Fn.IsG {
					name = fmt.Sprintf("<%v:%v>", parse.ChunkID(fr.Fn.Proto.SourceName), fr.Fn.Proto.LineDefined)
				}
				return name, call.Method, false
			}
		}
	}
	if !fr.Fn.IsG {
		// U5a (stock funcnamefromcall's funcnamefromcode arm): no call-site
		// entry means the frame's function was dispatched BY the VM — a
		// metamethod (__close at a CLOSE/RETURN instruction, __index,
		// __newindex, an MMBIN slow path, ...) or a for-loop iterator
		// (TFORCALL) — and stock names it from the CALLER's current
		// instruction (locals.lua:544's `in metamethod 'close'`). Call-site
		// pcs and metamethod-dispatch pcs are disjoint, so reaching here
		// with a CALL instruction is not possible.
		if !frame.Fn.IsG {
			if kind, name := funcNameFromCode(frame.Fn.Proto, frame.Pc-1); kind == "metamethod" {
				return fmt.Sprintf("metamethod '%s'", name), false, false
			} else if kind == "for iterator" {
				return "for iterator", false, false
			}
		}
		return fmt.Sprintf("<%v:%v>", parse.ChunkID(fr.Fn.Proto.SourceName), fr.Fn.Proto.LineDefined), false, false
	}
	return "(anonymous)", false, false
}

func (ls *LState) isStarted() bool {
	return ls.currentFrame != nil
}

func (ls *LState) kill() {
	ls.Dead = true
	if ls.ctxCancelFn != nil {
		ls.ctxCancelFn()
	}
}

func (ls *LState) indexToReg(idx int) int {
	base := ls.currentLocalBase()
	if idx > 0 {
		return base + idx - 1
	} else if idx == 0 {
		return -1
	} else {
		tidx := ls.reg.Top() + idx
		if tidx < base {
			return -1
		}
		return tidx
	}
}

func (ls *LState) currentLocalBase() int {
	base := 0
	if ls.currentFrame != nil {
		base = ls.currentFrame.LocalBase
	}
	return base
}

func (ls *LState) currentEnv() *LTable {
	return ls.Env
	/*
		if ls.currentFrame == nil {
			return ls.Env
		}
		return ls.currentFrame.Fn.Env
	*/
}


func (ls *LState) closeUpvalues(idx int) { // +inline-start
	if ls.uvcache != nil {
		var prev *Upvalue
		for uv := ls.uvcache; uv != nil; uv = uv.next {
			if uv.index >= idx {
				if prev != nil {
					prev.next = nil
				} else {
					ls.uvcache = nil
				}
				uv.Close()
			}
			prev = uv
		}
	}
} // +inline-end

// registerTBC is the OP_TBC body (U4; stock luaF_newtbcupval): false/nil
// values are exempt — no entry, no metamethod check; anything else must
// have a __close metamethod AT REGISTRATION TIME, else the
// variable-name error (luaG_findlocal over the frame's debug info;
// regnum is the 1-based register offset, pc the running instruction).
func (ls *LState) registerTBC(idx int, regnum int) {
	lv := ls.reg.Get(idx)
	if LVIsFalse(lv) {
		return
	}
	if op := ls.metaOp1(lv, "__close"); op.Type() != LTFunction {
		name := "?"
		if cf := ls.currentFrame; cf != nil && cf.Fn != nil && !cf.Fn.IsG && cf.Fn.Proto != nil {
			if pc := cf.Pc - 1; pc >= 0 && pc < len(cf.Fn.Proto.Code) {
				if n, ok := localNameAt(cf.Fn.Proto, regnum, pc); ok {
					name = n
				}
			}
		}
		ls.RaiseError("variable '%s' got a non-closable value", name)
	}
	ls.tbclist = &tbcEntry{idx: idx, prev: ls.tbclist}
}

// closeTBC closes the to-be-closed variables at or above the register
// level (U4; stock luaF_close's tbclist loop): innermost first — the
// list is stack-ordered — with __close(obj) as the single argument (the
// 2-argument error-path form is closeTBCProtected, U4d). A __close
// error propagates as a normal error. The entry is unlinked before the
// metamethod call (stock poptbclist ordering), so a __close that runs
// more Lua never re-closes it; reentrant registrations land above the
// dying frame and are closed by their own returns.
func (ls *LState) closeTBC(level int) {
	for ls.tbclist != nil && ls.tbclist.idx >= level {
		e := ls.tbclist
		ls.tbclist = e.prev
		lv := ls.reg.Get(e.idx)
		if LVIsFalse(lv) {
			continue // defensive: false/nil never register
		}
		// U5a (stock callclosemethod, via prepcallclosemth): the __close
		// slot is CALLED unconditionally — no notm check at close time (the
		// registration-time check lives in registerTBC). A metamethod
		// removed or invalidated after registration surfaces as the CALL
		// error "attempt to call a X value (metamethod 'close')", named by
		// the closing instruction (locals.lua:555-562).
		op := ls.metaOp1(lv, "__close")
		ls.reg.Push(op)
		ls.reg.Push(lv)
		ls.Call(1, 0)
	}
}

// closeSelfUnwind (U4d; stock luaD_throwbaselevel from lua_closethread's
// running arm) is the panic a thread raises to close ITSELF: every pcall
// level of that thread re-panics it (no recovery point may catch it) until
// threadRun's recover terminates the thread — ok carries whether the
// closes succeeded (false ⇒ errobj is the reported error object).
type closeSelfUnwind struct {
	errobj LValue
	ok     bool
}

// closeErrorPath is the pcall-boundary tail (U4d): the stock nil-object
// fix (luaG_errormsg replaces a nil crossing the boundary with the literal
// "<no error object>" — AFTER any message handler ran, before the closes)
// then the error-path tbc closes (luaD_pcall's luaD_closeprotected before
// luaD_seterrorobj: the closes see the handler's result as errobj, and a
// failing close replaces the reported object — last error wins).
func (ls *LState) closeErrorPath(base int, errp *error) {
	e, ok := (*errp).(*ApiError)
	if !ok {
		return
	}
	if e.Object == LNil {
		e.Object = LString("<no error object>")
	}
	if obj, failed := ls.closeTBCProtected(base, e.Object); failed {
		e.Object = obj
	}
}

// closeTBCProtected is the error-path form of closeTBC (U4d; stock
// luaD_closeprotected driving luaF_close's tbc loop): every entry >= level
// closes with __close(obj, errobj) — the in-flight error object — under
// per-close protection, and a failing close only REPLACES the in-flight
// object; closing continues until the list is clean, so the LAST raised
// error wins (stock's status replacement — there is no special "error in
// __close" text). A nil errobj means no in-flight error: the calls take
// the 1-argument normal-exit shape (coroutine.close / state close, where
// stock maps YIELD to LUA_OK). Returns the final object and whether any
// close raised.
func (ls *LState) closeTBCProtected(level int, errobj LValue) (LValue, bool) {
	failed := false
	for ls.tbclist != nil && ls.tbclist.idx >= level {
		e := ls.tbclist
		ls.tbclist = e.prev
		lv := ls.reg.Get(e.idx)
		if LVIsFalse(lv) {
			continue // defensive: false/nil never register
		}
		// U5a: unconditional call as in closeTBC — an invalidated __close
		// here becomes the in-flight error object ("attempt to call a
		// number value (metamethod 'close')", locals.lua:564-577)
		op := ls.metaOp1(lv, "__close")
		ls.reg.Push(op)
		ls.reg.Push(lv)
		if errobj != LNil {
			ls.reg.Push(errobj)
		}
		if perr := ls.PCall(1+boolInt(errobj != LNil), 0, nil); perr != nil {
			failed = true
			if ae, ok := perr.(*ApiError); ok {
				errobj = ae.Object
			} else {
				errobj = LString(perr.Error())
			}
		}
	}
	return errobj, failed
}

func (ls *LState) findUpvalue(idx int) *Upvalue {
	var prev *Upvalue
	var next *Upvalue
	if ls.uvcache != nil {
		for uv := ls.uvcache; uv != nil; uv = uv.next {
			if uv.index == idx {
				return uv
			}
			if uv.index > idx {
				next = uv
				break
			}
			prev = uv
		}
	}
	uv := &Upvalue{reg: ls.reg, index: idx, closed: false}
	if prev != nil {
		prev.next = uv
	} else {
		ls.uvcache = uv
	}
	if next != nil {
		uv.next = next
	}
	return uv
}

func (ls *LState) metatable(lvalue LValue, rawget bool) LValue {
	var metatable LValue = LNil
	switch obj := lvalue.(type) {
	case *LTable:
		metatable = obj.Metatable
	case *LUserData:
		if obj.Light {
			// U5c: light userdata has no per-object metatable — the
			// basic-type table (debug.setmetatable territory; base
			// setmetatable refuses light userdata outright)
			if table, ok := ls.G.builtinMts[basicTypeIndex(obj)]; ok {
				metatable = table
			}
		} else {
			metatable = obj.Metatable
		}
	default:
		if table, ok := ls.G.builtinMts[basicTypeIndex(obj)]; ok {
			metatable = table
		}
	}

	if !rawget && metatable != LNil {
		oldmt := metatable
		if tb, ok := metatable.(*LTable); ok {
			metatable = tb.RawGetString("__metatable")
			if metatable == LNil {
				metatable = oldmt
			}
		}
	}

	return metatable
}

func (ls *LState) metaOp1(lvalue LValue, event string) LValue {
	if mt := ls.metatable(lvalue, true); mt != LNil {
		if tb, ok := mt.(*LTable); ok {
			return tb.RawGetString(event)
		}
	}
	return LNil
}

func (ls *LState) metaOp2(value1, value2 LValue, event string) LValue {
	if mt := ls.metatable(value1, true); mt != LNil {
		if tb, ok := mt.(*LTable); ok {
			if ret := tb.RawGetString(event); ret != LNil {
				return ret
			}
		}
	}
	if mt := ls.metatable(value2, true); mt != LNil {
		if tb, ok := mt.(*LTable); ok {
			return tb.RawGetString(event)
		}
	}
	return LNil
}

// maxCallChain is stock MAX_CCMT (lstate.h): the __call replacement
// budget per call — 15 replacements allowed, the 16th raises.
const maxCallChain = 15

// resolveCall walks a call site's callee through the '__call' chain
// (stock luaD_precall's retry loop + tryfuncTM, ldo.c:510): each hop
// fetches the current value's __call field, inserts that value as the
// new first argument at base (the callee cell's successor, shifting the
// previous args up), and retries on the metamethod value itself — which
// may be another non-function with its own __call. The 16th hop raises
// "'__call' chain too long" AFTER its insert (tryfuncTM's
// shift-then-check order). Returns the resolved function (nil when the
// final value has no __call — callee names that VALUE for the caller's
// type error: a __call=5 chain reports "number", probed), and the chain
// count for the frame (getinfo 't' extraargs).
func (ls *LState) resolveCall(callee LValue, base int) (*LFunction, LValue, int) {
	if fn, ok := callee.(*LFunction); ok {
		return fn, callee, 0
	}
	for ccmt := 0; ; ccmt++ {
		tm := ls.metaOp1(callee, "__call")
		if tm == LNil {
			return nil, callee, ccmt
		}
		// tryfuncTM's insert (registry.Insert's shift): the link becomes
		// the new first argument, the previous args move up
		if top := ls.reg.Top(); top > base {
			for ; top > base; top-- {
				ls.reg.Set(top, ls.reg.Get(top-1))
			}
		}
		ls.reg.Set(base, callee)
		if ccmt == maxCallChain {
			// luaG_runerror's position rule: the current frame — a C
			// caller (pcall(x)) answers bare, a Lua caller carries its line
			ls.raiseErrorAt(0, "'__call' chain too long")
		}
		if fn, ok := tm.(*LFunction); ok {
			return fn, callee, ccmt + 1
		}
		callee = tm
	}
}

func (ls *LState) initCallFrame(cf *callFrame) { // +inline-start
	if cf.Fn.IsG {
		ls.reg.SetTop(cf.LocalBase + cf.NArgs)
	} else {
		proto := cf.Fn.Proto
		nargs := cf.NArgs
		np := int(proto.NumParameters)
		if nargs < np {
			// default any missing arguments to nil
			newSize := cf.LocalBase + np
			// +inline-call ls.reg.checkSize newSize
			for i := nargs; i < np; i++ {
				ls.reg.array[cf.LocalBase+i] = LNil
			}
			nargs = np
			ls.reg.top = newSize
		}
		// U5b: no call-time vararg shuffle — the extra arguments stay in
		// the window at registers np..nargs-1 (stock's layout: a callee's
		// cells above its params hold the caller's extras until its own
		// code overwrites them), and the frame is built by OP_VARARGPREP,
		// every vararg proto's first instruction: hidden model copies the
		// fixed params above the extras, table model builds {1..n, n=n}
		// into register np. The nil window fill starts above ALL arguments
		// so the extras survive until the prep runs.
		lo := nargs
		if nr := int(proto.NumUsedRegisters); nr > lo {
			lo = nr
		}
		newSize := cf.LocalBase + lo
		// +inline-call ls.reg.checkSize newSize
		for i := nargs; i < lo; i++ {
			ls.reg.array[cf.LocalBase+i] = LNil
		}
		ls.reg.top = newSize
	}
} // +inline-end

func (ls *LState) pushCallFrame(cf callFrame, fn LValue, meta bool) { // +inline-start
	if meta {
		cf.NArgs++
		ls.reg.Insert(fn, cf.LocalBase)
	}
	if cf.Fn == nil {
		// U2f re-pin: luaG_typeerror names the callee's type
		ls.raiseCallError(fn)
	}
	if ls.stack.IsFull() {
		ls.RaiseError("stack overflow")
	}
	ls.stack.Push(cf)
	newcf := ls.stack.Last()
	// +inline-call ls.initCallFrame newcf
	ls.currentFrame = newcf
} // +inline-end

// findPcallMarker walks the current frame chain innermost-out for the
// nearest pcall/xpcall marker (U5d, row 22): the PROTECTOR that catches
// an in-flight error. Pcall 3/4/5 are continuations, not protectors —
// they never catch. minIdx scopes the walk to a recovery region: only
// markers the wrapped loop itself pushed (Idx >= minIdx) complete at
// that depth; outer markers belong to outer recovery points.
func (ls *LState) findPcallMarker(minIdx int) *callFrame {
	for cf := ls.currentFrame; cf != nil; cf = cf.Parent {
		if cf.Idx < minIdx {
			return nil
		}
		if cf.Pcall == 1 || cf.Pcall == 2 {
			return cf
		}
	}
	return nil
}

// runMsgh runs an xpcall message handler AT RAISE (U5d; stock
// luaG_errormsg invokes the errfunc before any unwinding, so a traceback
// handler sees the full dying stack). U5f: stock re-invokes the handler
// for EVERY error, including one raised inside the running handler, with
// each invocation costing one luaD_callnoyield C-call unit: at
// LUAI_MAXCCALLS the pending object is REPLACED by luaG_runerror's
// "C stack overflow" (which still passes through the handler once, in
// "handling" mode), and at LUAI_MAXCCALLS/10*11 luaD_errerr fires
// WITHOUT the handler (errors.lua:636's counting handler terminates
// with "END" at 170, "C stack overflow" at 300). A handler dying of
// REGISTER-stack exhaustion is the other dialect (ldo.c:369 — a stack
// error while already handling one collapses immediately; row 78e).
func (ls *LState) runMsgh(handler LValue, obj LValue) LValue {
	fn, ok := handler.(*LFunction)
	if !ok {
		return obj
	}
	top := ls.reg.Top()
	for count := 1; ; count++ {
		if count == luaMaxCCalls {
			obj = LString("C stack overflow")
		} else if count >= luaMaxCCalls/10*11 {
			ls.reg.SetTop(top)
			return LString("error in error handling")
		}
		ls.reg.SetTop(top)
		ls.reg.Push(fn)
		ls.reg.Push(obj)
		if perr := ls.PCall(1, 1, nil); perr != nil {
			// the handler raised: a plain object re-enters the handler
			// (stock luaG_errormsg); a register-stack overflow is
			// luaD_errerr — collapse without re-invoking
			pobj := LValue(LString(perr.Error()))
			if ae, ok := perr.(*ApiError); ok {
				pobj = ae.Object
			}
			if msghObjIsStackOverflow(pobj) {
				ls.reg.SetTop(top)
				return LString("error in error handling")
			}
			obj = pobj
			continue
		}
		v := ls.reg.Get(top)
		ls.reg.SetTop(top)
		return v
	}
}

// msghObjIsStackOverflow classifies a message-handler failure as the
// register-stack overflow — RaiseError's "stack overflow" with its
// optional position prefix ("src:line: "). A user error() whose text
// happens to end the same way would take the errerr path too; nothing
// in the oracles pins that pathological shape.
func msghObjIsStackOverflow(obj LValue) bool {
	s, ok := obj.(LString)
	if !ok {
		return false
	}
	t := string(s)
	return t == "stack overflow" || strings.HasSuffix(t, ": stack overflow")
}

// completePcallMarker completes the pcall/xpcall/pairs marker that is
// the current frame — its body just finished, results staged at
// marker.LocalBase..top (ok), or [false, obj] staged here (!ok with
// errobj). The completion stages prefix+results at the marker's return
// window, pops it, and cascades while the revealed frame is itself a
// marker (pcall(pcall(...)) with a tailcall seam, the resume-entry
// marker). Returns 1 when the driver loop must exit (thread suspended
// or finished, or a Go-caller region takes over), 0 to continue.
func (ls *LState) completePcallMarker(ok bool, errobj LValue) int {
	if !ok {
		// stage just the object: the completion prefixes the false, so
		// the marker returns exactly [false, obj]
		m := ls.currentFrame
		ls.reg.SetTop(m.LocalBase)
		ls.reg.Push(errobj)
	}
	for {
		m := ls.currentFrame
		nres := ls.reg.Top() - m.LocalBase
		n := m.NRet
		var prefix LValue
		if m.Pcall == 3 {
			// pairscont: "__pairs did all the work" — the call is padded
			// to 4 (lua_callk's nresults) and the marker returns them
			// AS-IS (the true/false prefix is pcall's protocol, not
			// pairs')
			if nres < 4 {
				copyReturnValues(ls, m.LocalBase, m.LocalBase, 4, nres+1)
				nres = 4
			}
			if n == MultRet {
				n = nres
			}
		} else {
			if n == MultRet {
				n = nres + 1
			}
			prefix = LTrue
			if !ok {
				prefix = LFalse
			}
		}
		if ls.stack.Sp() == 1 && ls.Parent != nil {
			// the marker is the thread's base frame (create(pcall)) —
			// stage above it and let switchToParentThread deliver and
			// pop, exactly like doReturnTail's base-frame arm
			top := ls.reg.Top()
			if prefix != nil {
				copyReturnValues(ls, top+1, m.LocalBase, n-1, nres+1)
				ls.reg.Set(top, prefix)
			} else {
				copyReturnValues(ls, top, m.LocalBase, n, nres+1)
			}
			switchToParentThread(ls, n, false, true)
			return 1
		}
		if n == 0 {
			ls.reg.SetTop(m.Base)
		} else if prefix != nil {
			copyReturnValues(ls, m.Base+1, m.LocalBase, n-1, nres+1)
			ls.reg.Set(m.Base, prefix)
		} else {
			copyReturnValues(ls, m.Base, m.LocalBase, n, nres+1)
		}
		ls.stack.Pop()
		ls.currentFrame = ls.stack.Last()
		if ls.currentFrame == nil || ls.stack.IsEmpty() {
			if ls != ls.G.MainThread && !ls.Dead && ls.Parent == nil {
				// mirror doReturn's U4g arm: nothing left to deliver
				ls.kill()
			}
			return 1
		}
		if p := ls.currentFrame.Pcall; p >= 1 && p <= 3 {
			// cascade: this marker's call site just returned into an
			// enclosing marker (a tailcall seam or the resume entry)
			ok = true
			continue
		}
		if ls.currentFrame.Fn.IsG {
			// a Go-caller region (callR): its code takes the results
			return 1
		}
		return 0
	}
}

// errObjectOf extracts a recovered panic's error object (the shared
// shape of completeErrorMarker's head and its close-error re-entry).
func errObjectOf(rcv interface{}) LValue {
	if ae, ok := rcv.(*ApiError); ok {
		return ae.Object
	}
	return LString(fmt.Sprint(rcv))
}

// completeErrorMarker is the error arm of the marker catch (U5d): the
// innermost in-scope pcall/xpcall marker swallows rcv — msgh AT raise
// while the dying frames are still on the stack (xpcall), then the
// unwind to the marker, the dying frames' upvalue/tbc closes with the
// object in flight (a failing __close replaces it; the nil-object fix
// after the handler, stock luaD_pcall's order) — and the marker
// completes [false, obj]. U5f: the remaining tbc closes run YIELDABLE
// as kind-7 VM frames (stock precover -> finishCcall -> finishpcallk
// closes through luaF_close(func, status, 1) — official locals.lua's
// after-error battery yields inside them), with the in-flight object
// riding the marker's completion slot (m.LocalBase). A Go __close that
// raises synchronously here is caught per-close and the close loop
// restarts with the replaced object (stock precover's re-entry at the
// same marker: last error wins, closing continues, the msgh re-runs per
// raise); a Lua close's raise is caught by the driving loop's
// loopAttempt, which re-enters here for the SAME marker.
func (ls *LState) completeErrorMarker(rcv interface{}, m *callFrame) int {
	obj := errObjectOf(rcv)
	if m.Pcall == 2 {
		// U5a's frame reserve, carried into the marker path (row 84): a
		// stack overflow leaves the frame stack FULL, so the handler's
		// call would re-raise before running (stock luaD_growstack
		// reserves ERRORSTACKSIZE before raising; PCall's errfunc arm is
		// the old path's analog). Unwind the topmost DYING frames — they
		// sit above the marker and the SetSp below discards them anyway;
		// a handler needing more than the reserve re-raises where stock
		// reports luaD_errerr (row 78e dialect).
		if ls.stack.IsFull() {
			drop := ls.stack.Sp() - errorFrameReserve
			if drop < m.Idx+1 {
				drop = m.Idx + 1
			}
			ls.stack.SetSp(drop)
			ls.currentFrame = ls.stack.Last()
		}
		obj = ls.runMsgh(m.PcallHandler, obj)
	}
	if obj == LNil {
		obj = LString("<no error object>")
	}
	// unwind to (and including above) the marker: SetSp keeps 0..m.Idx
	ls.stack.SetSp(m.Idx + 1)
	ls.currentFrame = ls.stack.Last()
	ls.closeUpvalues(m.Base)
	// stage the in-flight object at the marker's completion slot WITHOUT
	// SetTop's nil-fill: the dying frames' cells above top still hold the
	// tbc values the sweep reads by absolute index (cells beyond top are
	// "may be nil", never "must be")
	ls.reg.top = m.LocalBase
	ls.reg.Push(obj)
	for {
		st, retry := 0, false
		func() {
			defer func() {
				if r := recover(); r != nil {
					if _, isunwind := r.(*closeSelfUnwind); isunwind {
						panic(r)
					}
					// a Go __close raised mid-close (stock: the error
					// escapes luaF_close and precover re-enters recovery at
					// the same marker): re-derive the object (msgh at raise),
					// unwind the close frames above the marker, and restart
					// the sweep for the remaining entries
					retry = true
					obj = errObjectOf(r)
					if m.Pcall == 2 {
						obj = ls.runMsgh(m.PcallHandler, obj)
					}
					if obj == LNil {
						obj = LString("<no error object>")
					}
					ls.stack.SetSp(m.Idx + 1)
					ls.currentFrame = ls.stack.Last()
					ls.reg.top = m.LocalBase
					ls.reg.Push(obj)
				}
			}()
			st = ls.closeTBCVM(m.Base, 7, 0, 0, obj)
		}()
		if retry {
			continue
		}
		switch st {
		case 1:
			return 0 // a close frame was pushed — the loop enters it
		case 2:
			return 1 // suspended inside a Go __close
		}
		return ls.completePcallMarker(false, obj)
	}
}

// markerKind classifies a call-site callee for the U5d marker
// intercept: the pcall/xpcall/pairs BUILTIN OBJECTS (any alias — the
// dispatch compares the resolved callee cell, so a replaced global is
// an ordinary function and never intercepts). Returns the marker kind
// (0 = no intercept). Degenerate arities and non-function handlers stay
// on the plain path so the builtin's own argument checks raise exactly
// as stock's do.
func markerKind(L *LState, lv LValue, RA, nargs int) uint8 {
	fn, ok := lv.(*LFunction)
	if !ok || !fn.IsG || nargs < 1 {
		return 0
	}
	if fn == L.G.builtinPcall {
		return 1
	}
	if fn == L.G.builtinXPCall {
		if nargs >= 2 && L.reg.Get(RA+2).Type() == LTFunction {
			return 2
		}
		return 0
	}
	if fn == L.G.builtinPairs {
		// only the __pairs metamethod arm; plain pairs stays the builtin
		if L.metaOp1(L.reg.Get(RA+1), "__pairs").Type() == LTFunction {
			return 3
		}
	}
	return 0
}

// pushPcallMarker performs the U5d marker intercept (row 22): pcall/
// xpcall become protected marker frames and pairs' __pairs call a
// continuation marker (stock's lua_pcallk/lua_callk continuations), so
// a yield crossing them suspends instead of unwinding the Go stack. The
// marker window IS the call site's window (fn@RA, body@RA+1, args@
// RA+2+); the body is an ordinary frame the loop enters next — pushed
// AFTER the marker, so a non-callable payload's raise is bare over the
// IsG marker (pcall(42), row 83's position rule). tailcall replaces the
// running frame wholesale (stock: a tailcall to C returns to OUR
// caller), keeping ReturnBase/NRet/Parent/TailCall. Returns whether the
// driver loop must exit (a Go body finished through the marker).
func pushPcallMarker(L *LState, cf *callFrame, lv LValue, RA, nargs, nret int, kind uint8, tailcall bool) (exit bool) {
	reg := L.reg
	bifn, _ := lv.(*LFunction) // markerKind validated it is the builtin
	handler := LNil
	bodyArgs := nargs - 1
	markerNArgs := nargs
	switch kind {
	case 1: // pcall: payload@RA+1, args@RA+2+
	case 2: // xpcall: f@RA+1, msgh@RA+2, extras@RA+3+ — extras shift down
		handler = reg.Get(RA + 2)
		reg.CopyRange(RA+2, RA+3, -1, nargs-2)
		bodyArgs = nargs - 2
		markerNArgs = nargs - 1
	case 3: // pairs: __pairs(t) — window mm@RA+1, t@RA+2
		reg.CopyRange(RA+2, RA+1, -1, 1)
		reg.Set(RA+1, L.metaOp1(reg.Get(RA+2), "__pairs"))
		bodyArgs = 1
		markerNArgs = nargs + 1
	}
	marker := callFrame{
		Fn:         bifn, // the builtin; debug-shaped as its C frame
		Pc:         0,
		Base:       RA,
		LocalBase:  RA + 1,
		ReturnBase: RA,
		NArgs:      markerNArgs,
		NRet:       nret,
		Parent:     cf,
		TailCall:   0,
		Pcall:      kind,
		PcallHandler: handler,
	}
	if tailcall {
		idx, rb, nr, parent, tc := cf.Idx, cf.ReturnBase, cf.NRet, cf.Parent, cf.TailCall
		base := cf.Base
		*cf = marker
		cf.Idx, cf.ReturnBase, cf.NRet, cf.Parent, cf.TailCall = idx, rb, nr, parent, tc
		L.initCallFrame(cf)
		// the plain tailcall slides the reused frame's window back down to
		// the frame's ORIGINAL base (the caller reads results at its own
		// call window, not at RA inside the dying registers) — the marker
		// window must make the same slide. initCallFrame's G arm already
		// set top just past the marker window, so the live span is exactly
		// top-RA cells.
		reg.CopyRange(base, RA, -1, reg.Top()-RA)
		cf.Base = base
		cf.LocalBase = base + 1
		RA = base
	} else {
		L.pushCallFrame(marker, LNil, false)
	}
	// the body: resolve the payload (or the metamethod) in the marker's
	// window — resolveCall may shift the window's args for a __call chain.
	// A payload that is ITSELF a marker builtin (xpcall(pcall, msgh, f),
	// pcall(pairs, t)) becomes a NESTED marker, not a live G frame: the
	// builtin's Go body PCalls (noyield, no continuation), losing exactly
	// what the markers exist to preserve. The body window is the payload's
	// call window (callee@RA+1, args@RA+2+), so the recursion is a plain
	// pushPcallMarker over it with the outer marker as the caller.
	bodyLv := reg.Get(RA + 1)
	if kind2 := markerKind(L, bodyLv, RA+1, bodyArgs); kind2 != 0 {
		return pushPcallMarker(L, L.currentFrame, bodyLv, RA+1, bodyArgs, MultRet, kind2, false)
	}
	bfn, callee, ccmt := L.resolveCall(bodyLv, RA+2)
	L.pushCallFrame(callFrame{
		Fn:         bfn,
		Pc:         0,
		Base:       RA + 1,
		LocalBase:  RA + 2,
		ReturnBase: RA + 1,
		NArgs:      bodyArgs + ccmt,
		NRet:       MultRet,
		Parent:     L.currentFrame,
		TailCall:   0,
		CCmt:       ccmt,
	}, callee, false)
	if bfn != nil && bfn.IsG && callGFunction(L, false) {
		return true
	}
	return false
}

// closeTBCVM drives the VM-yieldable to-be-closed list (U5d): each
// __close call runs as a frame TAGGED with the continuation kind — 4 =
// from an OP_CLOSE (ContB the close level), 5 = from a RETURN (ContRA/
// ContB the interrupted return's staging params, level the owner's
// LocalBase) — so a yield inside a Lua __close suspends through it
// (stock luaF_close from Lua code calls metamethods yieldable) and the
// owner's close/return resumes via doReturn's continuation arm. A C
// __close dispatched from Go context runs noyield (stock callclosemethod
// -> luaD_callnoyield): its Go frame cannot survive a suspend. U5f: from
// the VM it runs yieldable like stock's luaD_call arm — coroutine.yield
// AS __close suspends with the tagged frame kept, and the resume
// finishes it (stock resume's poscall of a NULL-continuation C frame)
// before continuing the close list. Kind 7 (U5f) is the error-path form:
// errval != LNil stages the 2-argument __close(obj, errobj) call of
// stock's finishpcallk (luaF_close(func, status, 1) — yieldable, running
// from the resume's clean C stack), with the in-flight object riding the
// catching marker's completion slot; when the list drains doReturn's
// kind-7 arm completes the marker. Returns 0 when the whole list ran
// synchronously, 1 when a close frame was pushed (the loop enters it),
// 2 when the thread suspended inside a Go __close (the loop must stop).
func (ls *LState) closeTBCVM(level int, kind uint8, contRA, contB int, errval LValue) int {
	for ls.tbclist != nil && ls.tbclist.idx >= level {
		e := ls.tbclist
		ls.tbclist = e.prev
		lv := ls.reg.Get(e.idx)
		if LVIsFalse(lv) {
			continue // defensive: false/nil never register
		}
		op := ls.metaOp1(lv, "__close")
		base := ls.reg.Top()
		// U5f: the close window must sit ABOVE every remaining entry's
		// cell. A kind-7 sweep reuses the dead owner's register region —
		// the plain reg.Top() lands inside it, and the close body's own
		// registers would trash a not-yet-dispatched entry's cell (its
		// later dispatch — or doReturn's plain closeTBC(lbase) sweep of the
		// body's own entries — reads garbage). Kinds 4/5 keep the owner's
		// cells below the window already; the max is a no-op there.
		for rem := ls.tbclist; rem != nil; rem = rem.prev {
			if rem.idx >= base {
				base = rem.idx + 1
			}
		}
		// the pushes below land at reg.top, so the window actually sits at
		// the raised base only if top moves there first — a direct write,
		// NOT SetTop: its nil-fill would wipe the very cells this guard
		// exists to protect
		ls.reg.top = base
		ls.reg.Push(op)
		ls.reg.Push(lv)
		nargs := 1
		if errval != LNil {
			ls.reg.Push(errval)
			nargs = 2
		}
		bfn, callee, ccmt := ls.resolveCall(op, base+1)
		cfk := callFrame{
			Fn:         bfn,
			Pc:         0,
			Base:       base,
			LocalBase:  base + 1,
			ReturnBase: base,
			NArgs:      nargs + ccmt,
			NRet:       0,
			Parent:     ls.currentFrame,
			TailCall:   0,
			CCmt:       ccmt,
			Pcall:      kind,
			ContRA:     contRA,
			ContB:      contB,
		}
		if bfn != nil && bfn.IsG {
			if ls.mmYieldable() || kind == 7 {
				// U5f: stock callclosemethod's yieldable arm — a C __close
				// called from Lua code is luaD_call'd (yy=1), so a Go close
				// body may suspend; the kind tag carries the close
				// continuation across the suspend (mainLoop's resume arm).
				// Kind 7 runs yieldable unconditionally: its closes run from
				// the resume's clean C stack (finishpcallk), where stock's
				// non-yieldable count is zero whatever sits below.
				ls.pushCallFrame(cfk, callee, false)
				ls.hookCallEvent(ls.currentFrame)
				if callGFunction(ls, false) {
					if ls.G.CurrentThread != ls {
						return 2 // suspended inside the Go __close
					}
					return 1 // the body ended the thread / a Go region took over
				}
				continue
			}
			// noyield (Go-context dispatch — stock luaD_callnoyield): the
			// depth bump must survive a raise mid-close: scope it so
			// the deferred decrement runs on the unwind too
			ls.noyieldDepth++
			done := func() (done bool) {
				defer func() { ls.noyieldDepth-- }()
				ls.pushCallFrame(cfk, callee, false)
				ls.hookCallEvent(ls.currentFrame)
				return callGFunction(ls, false)
			}()
			if done {
				return 1
			}
			continue
		}
		ls.pushCallFrame(cfk, callee, false)
		// U5f: the __close frame's call hook (stock luaD_call into it)
		ls.hookCallEvent(ls.currentFrame)
		return 1
	}
	return 0
}

// The metamethod-dispatch states (U5d, row 22 — stock ltm.c's
// luaD_call-vs-luaD_callnoyield dispatch plus its completion):
//   mmSync   the call ran to completion under this Go frame (a Go-context
//            dispatch, or a C metamethod in VM context) — the result is
//            the return value, the staging window popped
//   mmPushed a Lua metamethod in VM context became a kind-6 continuation
//            frame — the interrupted instruction suspends (vmFinishMM
//            completes it when the metamethod returns)
//   mmExit   the driver loop must exit (a C-metamethod completion ended
//            the thread or handed off to a Go region)
const (
	mmSync   uint8 = iota
	mmPushed
	mmExit
)

// mmYieldable is stock ltm.c's rule — "metamethod may yield only when
// called from Lua code" (luaT_callTM's isLuacode(L->ci)): the dispatch
// happens in a running Lua frame with no Go call region between (stock's
// nny == 0). Go-context dispatches — host API under L.Call/callR, G
// function bodies, sort comparators — stay noyield; a yield there raises
// the catchable boundary error.
func (ls *LState) mmYieldable() bool {
	if ls.noyieldDepth != 0 {
		return false
	}
	cf := ls.currentFrame
	return cf != nil && cf.Fn != nil && !cf.Fn.IsG
}

// callMM dispatches a staged metamethod call — op@base, args@base+1..,
// base = reg.Top()-nargs-1 — under stock's yieldability rule. From Go
// context (or for a C metamethod, which has no continuation — stock's
// callclosemethod uses luaD_callnoyield for exactly this shape) it runs
// to completion noyield with the result at the window base and the
// window popped (mmSync). From VM context a Lua metamethod becomes a
// kind-6 continuation frame carrying contA/contB — opcode-specific
// fold/loop state for vmFinishMM (CONCAT's remaining fold) — and the
// interrupted instruction suspends (mmPushed) or the loop exits (mmExit).
func (ls *LState) callMM(nargs, nret int, contA, contB int) (LValue, uint8) {
	reg := ls.reg
	base := reg.Top() - nargs - 1
	if !ls.mmYieldable() {
		ls.Call(nargs, nret)
		v := LNil
		if nret > 0 {
			v = reg.Get(base)
		}
		reg.SetTop(base)
		return v, mmSync
	}
	op := reg.Get(base)
	bfn, callee, ccmt := ls.resolveCall(op, base+1)
	if bfn != nil && bfn.IsG {
		// the depth bump must survive a raise mid-call: scope it so the
		// deferred decrement runs on the unwind too
		ls.noyieldDepth++
		done := func() (done bool) {
			defer func() { ls.noyieldDepth-- }()
			ls.pushCallFrame(callFrame{
				Fn:         bfn,
				Pc:         0,
				Base:       base,
				LocalBase:  base + 1,
				ReturnBase: base,
				NArgs:      nargs + ccmt,
				NRet:       nret,
				Parent:     ls.currentFrame,
				TailCall:   0,
				CCmt:       ccmt,
			}, callee, false)
			ls.hookCallEvent(ls.currentFrame)
			return callGFunction(ls, false)
		}()
		if done {
			return LNil, mmExit
		}
		v := LNil
		if nret > 0 {
			v = reg.Get(base)
		}
		reg.SetTop(base)
		return v, mmSync
	}
	ls.pushCallFrame(callFrame{
		Fn:         bfn,
		Pc:         0,
		Base:       base,
		LocalBase:  base + 1,
		ReturnBase: base,
		NArgs:      nargs + ccmt,
		NRet:       nret,
		Parent:     ls.currentFrame,
		TailCall:   0,
		CCmt:       ccmt,
		Pcall:      6,
		ContRA:     contA,
		ContB:      contB,
	}, callee, false)
	// U5f: the kind-6 metamethod frame's call hook (stock luaD_call)
	ls.hookCallEvent(ls.currentFrame)
	return LNil, mmPushed
}

// loopAttempt runs one mainLoop attempt under the pcall-marker catch
// (U5d, row 22): an error raised inside a pcall/xpcall marker's body at
// this recovery depth or deeper (Idx >= minIdx) completes the marker
// (false + object — msgh at raise) and restarts the loop at the
// repaired frame. baseframe is mainLoop's stop frame — for a callR
// region it is the callee's own frame, so the loop drives ONLY that
// callee and returns to the Go caller when it finishes (the stackless
// loop would otherwise run the caller's frames to completion inside the
// Go call); nil runs the thread until the stack empties. Anything else
// re-panics to the outer recovery points (threadRun's thread-death
// recover, an enclosing loopAttempt, or a host PCall) — stock's
// lua_pcallk catching at the depth that owns the continuation.
// closeSelfUnwind always re-panics (no recovery point may catch it). A
// restart keeps the baseframe: the marker unwind stops above it
// (m.Idx >= minIdx == baseframe.Idx in the callR shape), so the stop
// frame survives.
func loopAttempt(L *LState, minIdx int, baseframe *callFrame) {
	defer func() {
		if rcv := recover(); rcv != nil {
			if _, isunwind := rcv.(*closeSelfUnwind); isunwind {
				panic(rcv)
			}
			m := L.findPcallMarker(minIdx)
			if m == nil {
				panic(rcv)
			}
			if L.completeErrorMarker(rcv, m) == 1 {
				return
			}
			loopAttempt(L, minIdx, baseframe)
		}
	}()
	L.mainLoop(L, baseframe)
}

// Debug-hook mask bits (stock lua.h's LUA_MASK*, the LUA_HOOK* event
// numbers as bit positions).
const (
	hookMaskCall  uint8 = 1 << 0 // "call" + "tail call"
	hookMaskRet   uint8 = 1 << 1
	hookMaskLine  uint8 = 1 << 2
	hookMaskCount uint8 = 1 << 3
)

// hookInvoke runs the hook function for an event (stock luaD_hook over
// ldblib's hookf): the hook is called with (event, line-or-nil) — hookf
// pushes nil when currentline < 0 — as a callR body, so getinfo(2) from
// inside the hook is the frame the event fired on (stock calls the hook
// directly from C: no intermediate frame) and GetInfo 'n' resolves the
// event names ("sethook", "close", "foo"). callR is no-yield — stock
// hooks run under luaD_callnoyield too, so a yielding hook raises the
// row-84 boundary error. While the hook runs hookBusy blocks nested
// events (stock's allowhook=0), but the per-instruction seam still
// advances hookOldPc/hookCountdown over the hook function's own frames
// — stock's oldpc is thread-global and updated by every traced
// instruction, whatever frame it belongs to.
//
// U5f: the frame the event fired ON (the frame current when this runs —
// the callee for a call event, the returning frame for a return, the
// executing frame for line/count) is marked Hooked (CIST_HOOKED) while
// the hook runs, and (ftransfer, ntransfer) are recorded thread-globally
// (L->transferinfo) — overwritten by every event, reported by getinfo
// 'r' over the marked frame.
func (ls *LState) hookInvoke(event string, line, ftransfer, ntransfer int) {
	if ls.hookBusy || ls.hookFn == nil {
		return
	}
	ls.hookBusy = true
	traced := ls.currentFrame
	if traced != nil {
		traced.Hooked = true
	}
	ls.hookFTransfer = ftransfer
	ls.hookNTransfer = ntransfer
	defer func() {
		if traced != nil {
			traced.Hooked = false
		}
		ls.hookBusy = false
	}()
	base := ls.reg.Top()
	ls.reg.Push(ls.hookFn)
	ls.reg.Push(LString(event))
	if line >= 0 {
		ls.reg.Push(LInteger(line))
	} else {
		ls.reg.Push(LNil)
	}
	ls.callR(2, 0, base)
}

// hookCallEvent fires the CALL hook for a freshly pushed frame (stock
// luaD_hookcall via luaG_tracecall for a Lua callee, precallC's hook
// for a C one). A tailcall-reused frame reports "tail call" (TailCall >
// 0). Vararg Lua frames defer to their OP_VARARGPREP (stock defers the
// hook the same way — the argument shuffle must run first). oldpc=0
// makes the callee's first instruction fire the line hook (pcIdx 0 <=
// 0), matching stock's hookcall entry.
func (ls *LState) hookCallEvent(cf *callFrame) {
	if ls.hookMask == 0 {
		return
	}
	if !cf.Fn.IsG && cf.Fn.Proto.IsVarArg != 0 {
		return // OP_VARARGPREP fires it (and owns the oldpc dance)
	}
	ls.hookOldPc = 0
	if ls.hookMask&hookMaskCall == 0 {
		return
	}
	event := "call"
	if !cf.Fn.IsG && cf.TailCall > 0 {
		event = "tail call"
	}
	// U5f (stock luaD_hookcall vs precallC): the transfer window is the
	// callee's parameters — numparams for a Lua callee (missing ones
	// nil-filled), the actual argument count for a C callee.
	n := cf.NArgs
	if !cf.Fn.IsG {
		n = int(cf.Fn.Proto.NumParameters)
	}
	ls.hookInvoke(event, -1, 1, n)
}

// hookVarargCallEvent is OP_VARARGPREP's trap arm (stock lvm.c): the
// deferred call/tail-call hook for a vararg frame fires after the
// argument shuffle, then oldpc=1 — the instruction after VARARGPREP is
// seen as a new line even when the line does not change.
func (ls *LState) hookVarargCallEvent(cf *callFrame) {
	if ls.hookMask == 0 {
		return
	}
	ls.hookOldPc = 0
	if ls.hookMask&hookMaskCall != 0 {
		event := "call"
		if cf.TailCall > 0 {
			event = "tail call"
		}
		ls.hookInvoke(event, -1, 1, int(cf.Fn.Proto.NumParameters))
	}
	ls.hookOldPc = 1
}

// hookReturnEvent is stock rethook at luaD_poscall: the RETURN event
// fires before the results move (after the frame's tbc closes —
// moveresults' CIST_TBC ordering), with the returning frame still
// current so the hook's getinfo(2) names it; then oldpc tracks the
// CALLER's current instruction (pcRel(savedpc): the call site), so the
// resume instruction compares like any post-call step. A non-Lua caller
// leaves oldpc alone (stock's isLua guard).
//
// U5f: firstres/nres (stock's firstres = top - nres) give the transfer
// window — ftransfer = firstres - virtual func. Our LocalBase IS the
// virtual func cell of stock's vararg layout (there is no copied func
// slot; the params sit exactly where stock puts them after its copy),
// so the 1-based register index stock reports is firstres-LocalBase+1;
// verified against the oracle for Lua returns, G returns, tail calls
// and select-style multrets (/tmp/tr.lua, db.lua:533).
func (ls *LState) hookReturnEvent(cf *callFrame, firstres, nres int) {
	if ls.hookMask == 0 {
		return
	}
	if ls.hookMask&hookMaskRet != 0 {
		ls.hookInvoke("return", -1, firstres-cf.LocalBase+1, nres)
	}
	if parent := cf.Parent; parent != nil && parent.Fn != nil && !parent.Fn.IsG && parent.Fn.Proto != nil {
		ls.hookOldPc = parent.Pc - 1
	}
}

// hookLineAt is luaG_getfuncline over the fork's absolute line array
// (DbgSourcePositions), with traceexec's oldpc-validity guard: an oldpc
// left by ANOTHER proto may index past this one — stock substitutes 0.
// A stripped proto (no lineinfo) answers -1 (db.lua:1016's line-hook
// battery: events still fire, with a nil line).
func hookLineAt(p *FunctionProto, idx int) int {
	if len(p.DbgSourcePositions) == 0 {
		return -1
	}
	if idx < 0 || idx >= len(p.DbgSourcePositions) {
		idx = 0
	}
	return p.DbgSourcePositions[idx]
}

// traceHook is the per-instruction seam (stock luaG_traceexec), run by
// the main loops right after the fetch's Pc++ — pcIdx = cf.Pc-1 is the
// instruction about to run. The count countdown runs even while a hook
// is executing (stock decrements on every traced instruction, hook
// frames included); the count hook fires before the line hook. The line
// test — a jump back (pcIdx <= oldpc) or a line change since the
// instruction oldpc last visited — fires with the instruction's
// absolute line. oldpc advances on every visit, whatever frame it
// belongs to (stock's thread-global oldpc).
func (ls *LState) traceHook(cf *callFrame) {
	// U5f (stock luaG_tracecall's vararg arm, db.lua:124's trace): a
	// vararg proto's instruction 0 — OP_VARARGPREP — never passes through
	// traceexec (trap is 0 at its fetch); its handler fires the deferred
	// call event and sets oldpc=1, so instruction 1 is what traces the
	// entry line. Tracing instruction 0 here would fire that line twice.
	if pcIdx := cf.Pc - 1; pcIdx == 0 && cf.Fn.Proto.IsVarArg != 0 {
		return
	}
	if ls.hookMask&hookMaskCount != 0 {
		ls.hookCountdown--
		if ls.hookCountdown <= 0 {
			ls.hookCountdown = ls.hookCount
			ls.hookInvoke("count", -1, 0, 0)
		}
	}
	if ls.hookMask&hookMaskLine != 0 {
		pcIdx := cf.Pc - 1
		p := cf.Fn.Proto
		// traceexec's oldpc-validity guard comes BEFORE the comparison:
		// an oldpc left by another proto (or a hook frame) may index past
		// this one — stock substitutes 0, so a jump-back test against a
		// polluted oldpc degrades to npci <= 0 (the entry line event)
		oldpc := ls.hookOldPc
		if oldpc < 0 || oldpc >= len(p.DbgSourcePositions) {
			oldpc = 0
		}
		if pcIdx <= oldpc || hookLineAt(p, pcIdx) != hookLineAt(p, oldpc) {
			ls.hookInvoke("line", hookLineAt(p, pcIdx), 0, 0)
		}
		ls.hookOldPc = pcIdx
	}
}

// firePendingReturnHook is the resume half of a C call that suspended
// mid-body (coroutine.yield): stock finishCfunc redoes the poscall, so
// the yield's RETURN event fires when the thread resumes, before the
// suspended frame's next instruction (coroutine.lua's trace: the 4th
// event). The G frame was popped at the suspension; its caller is the
// frame now on top — the oldpc walk uses that frame's call site.
func (ls *LState) firePendingReturnHook() {
	ls.retHookPending = false
	if ls.hookMask&hookMaskRet != 0 {
		// the suspended G frame is already popped in the fork's model —
		// stock's finishCfunc redoes poscall over the still-live C frame
		// with the resume window (firstres = func, nres = yielded count).
		// (0,0) here diverges for a 'r' query in this resume-seam hook
		// only; no corpus reads one (coroutine.lua checks event names).
		ls.hookInvoke("return", -1, 0, 0)
	}
	if cf := ls.currentFrame; cf != nil && cf.Fn != nil && !cf.Fn.IsG && cf.Fn.Proto != nil {
		ls.hookOldPc = cf.Pc - 1
	}
}

func (ls *LState) callR(nargs, nret, rbase int) {
	// U5d (row 22, stock lua_call/lua_pcall from C = luaD_callnoyield):
	// a call driven from a Go frame has no continuation — mark the
	// region no-yield so a yield inside raises the catchable boundary
	// error instead of unwinding this live Go frame. A plain yield
	// unwinds the Go stack NORMALLY (gfnret<0 returns through every
	// level), so the deferred decrement still runs.
	ls.noyieldDepth++
	defer func() { ls.noyieldDepth-- }()
	base := ls.reg.Top() - nargs - 1
	if rbase < 0 {
		rbase = base
	}
	lv := ls.reg.Get(base)
	fn, callee, ccmt := ls.resolveCall(lv, base+1)
	ls.pushCallFrame(callFrame{
		Fn:         fn,
		Pc:         0,
		Base:       base,
		LocalBase:  base + 1,
		ReturnBase: rbase,
		NArgs:      nargs + ccmt,
		NRet:       nret,
		Parent:     ls.currentFrame,
		TailCall:   0,
		CCmt:       ccmt,
	}, callee, false)
	// U5f: the Go-driven call is stock's luaD_call — the call hook fires
	// at the callee's first instruction (tracecall/precallC)
	ls.hookCallEvent(ls.currentFrame)
	if ls.G.MainThread == nil {
		ls.G.MainThread = ls
		ls.G.CurrentThread = ls
		loopAttempt(ls, 0, nil)
	} else {
		// markers the callee's code pushes (Idx >= its own) complete HERE
		// so the Go caller of this call keeps running (stock: pcallk
		// catches at the depth that owns the Go frame — gsub continues);
		// baseframe = the callee's frame keeps the loop scoped to it (the
		// old callR passed ls.currentFrame for exactly this)
		loopAttempt(ls, ls.stack.Sp()-1, ls.currentFrame)
	}
	if nret != MultRet {
		ls.reg.SetTop(rbase + nret)
	}
}

// getField and friends take the varinfo hint of the OBJECT operand
// (row 62): stock luaV_finishget/luaV_finishset raise luaG_typeerror on
// the table at the FIRST level of the __index/__newindex chain — the
// only level whose value lives in a register (or, for GETTABUP/
// SETTABUP, an upvalue cell), so the hint applies at i == 0 only.

// The field walkers return the metamethod-dispatch state alongside their
// value (U5d, row 22): mmSync completes the instruction here; mmPushed/
// mmExit mean a VM-dispatched __index/__newindex became a kind-6 frame —
// the caller's instruction suspends and vmFinishMM completes it on resume
// (the host-API wrappers run in Go context, where the state is always
// mmSync).

func (ls *LState) getField(obj LValue, key LValue, hint varinfoHint) (LValue, uint8) {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			ret := tb.RawGet(key)
			if ret != LNil {
				return ret, mmSync
			}
		}
		metaindex := ls.metaOp1(curobj, "__index")
		if metaindex == LNil {
			if !istable {
				h := ""
				if i == 0 {
					h = hint.resolve(ls)
				}
				ls.RaiseError("attempt to index a %s value%s", curobj.Type().String(), h)
			}
			return LNil, mmSync
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(key)
			return ls.callMM(2, 1, 0, 0)
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in gettable")
	return nil, mmSync
}

func (ls *LState) getFieldString(obj LValue, key string, hint varinfoHint) (LValue, uint8) {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			ret := tb.RawGetString(key)
			if ret != LNil {
				return ret, mmSync
			}
		}
		metaindex := ls.metaOp1(curobj, "__index")
		if metaindex == LNil {
			if !istable {
				h := ""
				if i == 0 {
					h = hint.resolve(ls)
				}
				ls.RaiseError("attempt to index a %s value%s", curobj.Type().String(), h)
			}
			return LNil, mmSync
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(LString(key))
			return ls.callMM(2, 1, 0, 0)
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in gettable")
	return nil, mmSync
}

func (ls *LState) setField(obj LValue, key LValue, value LValue, hint varinfoHint) uint8 {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			if tb.RawGet(key) != LNil {
				ls.RawSet(tb, key, value)
				return mmSync
			}
		}
		metaindex := ls.metaOp1(curobj, "__newindex")
		if metaindex == LNil {
			if !istable {
				h := ""
				if i == 0 {
					h = hint.resolve(ls)
				}
				ls.RaiseError("attempt to index a %s value%s", curobj.Type().String(), h)
			}
			ls.RawSet(tb, key, value)
			return mmSync
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(key)
			ls.reg.Push(value)
			_, st := ls.callMM(3, 0, 0, 0)
			return st
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in settable")
	return mmSync
}

func (ls *LState) setFieldString(obj LValue, key string, value LValue, hint varinfoHint) uint8 {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			if tb.RawGetString(key) != LNil {
				tb.RawSetString(key, value)
				return mmSync
			}
		}
		metaindex := ls.metaOp1(curobj, "__newindex")
		if metaindex == LNil {
			if !istable {
				h := ""
				if i == 0 {
					h = hint.resolve(ls)
				}
				ls.RaiseError("attempt to index a %s value%s", curobj.Type().String(), h)
			}
			tb.RawSetString(key, value)
			return mmSync
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(LString(key))
			ls.reg.Push(value)
			_, st := ls.callMM(3, 0, 0, 0)
			return st
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in settable")
	return mmSync
}

/* }}} */

/* api methods {{{ */

func NewState(opts ...Options) *LState {
	var ls *LState
	if len(opts) == 0 {
		ls = newLState(Options{
			CallStackSize: CallStackSize,
			RegistrySize:  RegistrySize,
		})
		ls.OpenLibs()
	} else {
		if opts[0].CallStackSize < 1 {
			opts[0].CallStackSize = CallStackSize
		}
		if opts[0].RegistrySize < 128 {
			opts[0].RegistrySize = RegistrySize
		}
		if opts[0].RegistryMaxSize < opts[0].RegistrySize {
			opts[0].RegistryMaxSize = 0 // disable growth if max size is smaller than initial size
		} else {
			// if growth enabled, grow step is set
			if opts[0].RegistryGrowStep < 1 {
				opts[0].RegistryGrowStep = RegistryGrowStep
			}
		}
		ls = newLState(opts[0])
		if !opts[0].SkipOpenLibs {
			ls.OpenLibs()
		}
	}
	return ls
}

func (ls *LState) IsClosed() bool {
	return ls.stack == nil
}

func (ls *LState) Close() {
	atomic.AddInt32(&ls.stop, 1)
	if ls.stack == nil {
		return
	}
	// U5f (stock close_state → callallpendingfinalizers): a close runs
	// the finalizers already queued before tearing anything down. Their
	// errors are warned, not raised (GCTM's shape)
	ls.drainFinalizers()
	// U4d (stock close_state): a full close runs the MAIN thread's
	// Lua-level exits — upvalues first, then every tbc entry as a 1-arg
	// __close(obj) (YIELD-mapped-to-OK shape; failures are swallowed like
	// close_state's ignored closeprotected status). Suspended coroutines'
	// entries are NOT closed here (each thread owns its list).
	th := ls
	if ls.G.MainThread != nil {
		th = ls.G.MainThread
	}
	th.closeUpvalues(0)
	th.closeTBCProtected(0, LNil)
	for _, file := range ls.G.tempFiles {
		// ignore errors in these operations
		file.Close()
		os.Remove(file.Name())
	}
	ls.stack.FreeAll()
	ls.stack = nil
}

/* registry operations {{{ */

func (ls *LState) GetTop() int {
	return ls.reg.Top() - ls.currentLocalBase()
}

func (ls *LState) SetTop(idx int) {
	base := ls.currentLocalBase()
	newtop := ls.indexToReg(idx) + 1
	if newtop < base {
		ls.reg.SetTop(base)
	} else {
		ls.reg.SetTop(newtop)
	}
}

func (ls *LState) Replace(idx int, value LValue) {
	base := ls.currentLocalBase()
	if idx > 0 {
		reg := base + idx - 1
		if reg < ls.reg.Top() {
			ls.reg.Set(reg, value)
		}
	} else if idx == 0 {
	} else if idx > RegistryIndex {
		if tidx := ls.reg.Top() + idx; tidx >= base {
			ls.reg.Set(tidx, value)
		}
	} else {
		switch idx {
		case RegistryIndex:
			if tb, ok := value.(*LTable); ok {
				ls.G.Registry = tb
			} else {
				ls.RaiseError("registry must be a table(%v)", value.Type().String())
			}
		case EnvironIndex:
			if ls.currentFrame == nil {
				ls.RaiseError("no calling environment")
			}
			if tb, ok := value.(*LTable); ok {
				ls.currentFrame.Fn.Env = tb
			} else {
				ls.RaiseError("environment must be a table(%v)", value.Type().String())
			}
		case GlobalsIndex:
			if tb, ok := value.(*LTable); ok {
				ls.G.Global = tb
			} else {
				ls.RaiseError("_G must be a table(%v)", value.Type().String())
			}
		default:
			fn := ls.currentFrame.Fn
			index := GlobalsIndex - idx - 1
			if index < len(fn.Upvalues) {
				fn.Upvalues[index].SetValue(value)
			}
		}
	}
}

func (ls *LState) Get(idx int) LValue {
	base := ls.currentLocalBase()
	if idx > 0 {
		reg := base + idx - 1
		if reg < ls.reg.Top() {
			return ls.reg.Get(reg)
		}
		return LNil
	} else if idx == 0 {
		return LNil
	} else if idx > RegistryIndex {
		tidx := ls.reg.Top() + idx
		if tidx < base {
			return LNil
		}
		return ls.reg.Get(tidx)
	} else {
		switch idx {
		case RegistryIndex:
			return ls.G.Registry
		case EnvironIndex:
			if ls.currentFrame == nil {
				return ls.Env
			}
			return ls.currentFrame.Fn.Env
		case GlobalsIndex:
			return ls.G.Global
		default:
			fn := ls.currentFrame.Fn
			index := GlobalsIndex - idx - 1
			if index < len(fn.Upvalues) {
				return fn.Upvalues[index].Value()
			}
			return LNil
		}
	}
	return LNil
}

func (ls *LState) Push(value LValue) {
	ls.reg.Push(value)
}

func (ls *LState) Pop(n int) {
	for i := 0; i < n; i++ {
		if ls.GetTop() == 0 {
			ls.RaiseError("register underflow")
		}
		ls.reg.Pop()
	}
}

func (ls *LState) Insert(value LValue, index int) {
	reg := ls.indexToReg(index)
	top := ls.reg.Top()
	if reg >= top {
		ls.reg.Set(reg, value)
		return
	}
	if reg <= ls.currentLocalBase() {
		reg = ls.currentLocalBase()
	}
	top--
	for ; top >= reg; top-- {
		ls.reg.Set(top+1, ls.reg.Get(top))
	}
	ls.reg.Set(reg, value)
}

func (ls *LState) Remove(index int) {
	reg := ls.indexToReg(index)
	top := ls.reg.Top()
	switch {
	case reg >= top:
		return
	case reg < ls.currentLocalBase():
		return
	case reg == top-1:
		ls.Pop(1)
		return
	}
	for i := reg; i < top-1; i++ {
		ls.reg.Set(i, ls.reg.Get(i+1))
	}
	ls.reg.SetTop(top - 1)
}

/* }}} */

/* object allocation {{{ */

func (ls *LState) NewTable() *LTable {
	return newLTable(defaultArrayCap, defaultHashCap)
}

func (ls *LState) CreateTable(acap, hcap int) *LTable {
	return newLTable(acap, hcap)
}

// NewThread returns a new LState that shares with the original state all global objects.
// If the original state has context.Context, the new state has a new child context of the original state and this function returns its cancel function.
func (ls *LState) NewThread() (*LState, context.CancelFunc) {
	thread := newLState(ls.Options)
	thread.G = ls.G
	thread.Env = ls.Env
	var f context.CancelFunc = nil
	if ls.ctx != nil {
		thread.mainLoop = mainLoopWithContext
		thread.ctx, f = context.WithCancel(ls.ctx)
		thread.ctxCancelFn = f
	}
	return thread, f
}

func (ls *LState) NewFunctionFromProto(proto *FunctionProto) *LFunction {
	fn := newLFunctionL(proto, ls.Env, int(proto.NumUpvalues))
	if idx := proto.EnvUpvalueIndex; idx >= 0 {
		uv := &Upvalue{}
		uv.Close()
		uv.SetValue(ls.Env)
		fn.Upvalues[idx] = uv
	}
	return fn
}

func (ls *LState) NewUserData() *LUserData {
	return &LUserData{
		Env:       ls.currentEnv(),
		Metatable: LNil,
	}
}

func (ls *LState) NewFunction(fn LGFunction) *LFunction {
	return newLFunctionG(fn, ls.currentEnv(), 0)
}

func (ls *LState) NewClosure(fn LGFunction, upvalues ...LValue) *LFunction {
	cl := newLFunctionG(fn, ls.currentEnv(), len(upvalues))
	for i, lv := range upvalues {
		cl.Upvalues[i] = &Upvalue{}
		cl.Upvalues[i].Close()
		cl.Upvalues[i].SetValue(lv)
	}
	return cl
}

/* }}} */

/* toType {{{ */

func (ls *LState) ToBool(n int) bool {
	return LVAsBool(ls.Get(n))
}

func (ls *LState) ToInt(n int) int {
	if lv, ok := ls.Get(n).(LInteger); ok {
		return int(lv)
	}
	if lv, ok := ls.Get(n).(LNumber); ok {
		return int(lv)
	}
	if lv, ok := ls.Get(n).(LString); ok {
		if num, err := parseNumber(string(lv)); err == nil {
			return int(LVAsNumber(num))
		}
	}
	return 0
}

func (ls *LState) ToInt64(n int) int64 {
	if lv, ok := ls.Get(n).(LInteger); ok {
		return int64(lv)
	}
	if lv, ok := ls.Get(n).(LNumber); ok {
		return int64(lv)
	}
	if lv, ok := ls.Get(n).(LString); ok {
		if num, err := parseNumber(string(lv)); err == nil {
			return int64(LVAsNumber(num))
		}
	}
	return 0
}

func (ls *LState) ToNumber(n int) LNumber {
	return LVAsNumber(ls.Get(n))
}

func (ls *LState) ToString(n int) string {
	return LVAsString(ls.Get(n))
}

func (ls *LState) ToTable(n int) *LTable {
	if lv, ok := ls.Get(n).(*LTable); ok {
		return lv
	}
	return nil
}

func (ls *LState) ToFunction(n int) *LFunction {
	if lv, ok := ls.Get(n).(*LFunction); ok {
		return lv
	}
	return nil
}

func (ls *LState) ToUserData(n int) *LUserData {
	if lv, ok := ls.Get(n).(*LUserData); ok {
		return lv
	}
	return nil
}

func (ls *LState) ToThread(n int) *LState {
	if lv, ok := ls.Get(n).(*LState); ok {
		return lv
	}
	return nil
}

/* }}} */

/* error & debug operations {{{ */

func (ls *LState) registryOverflow() {
	ls.RaiseError("registry overflow")
}

// This function is equivalent to luaL_error( http://www.lua.org/manual/5.1/manual.html#luaL_error ).
// U2f: the position target is the CURRENT frame when raised from Lua
// code (luaG_runerror semantics — the VM's own raises) and the caller
// when raised from a G function (luaL_error semantics) — and a G
// caller yields NO position (stock's luaL_where over a C frame is
// empty; the old skipg walk skipped straight past it).
func (ls *LState) RaiseError(format string, args ...interface{}) {
	level := 0
	if cf := ls.currentFrame; cf != nil && cf.Fn != nil && cf.Fn.IsG {
		level = 1
	}
	ls.raiseErrorAt(level, format, args...)
}

// raiseErrorAt prefixes unconditionally (an empty stockWhere adds
// nothing) — the internal-raise path where level 0 names the running
// Lua frame itself; raiseError keeps error()'s level-0-means-none rule.
func (ls *LState) raiseErrorAt(level int, format string, args ...interface{}) {
	message := format
	if len(args) > 0 {
		message = fmt.Sprintf(format, args...)
	}
	if prefix := ls.stockWhere(level); prefix != "" {
		message = fmt.Sprintf("%v %v", prefix, message)
	}
	if ls.reg.IsFull() {
		ls.reg.forceResize(ls.reg.Top() + 1)
	}
	ls.reg.Push(LString(message))
	ls.Panic(ls)
}

// This function is equivalent to lua_error( http://www.lua.org/manual/5.1/manual.html#lua_error ).
func (ls *LState) Error(lv LValue, level int) {
	if str, ok := lv.(LString); ok {
		ls.raiseError(level, "%s", string(str))
	} else {
		// non-string error values: no raise-time upvalue close — see
		// raiseError's note (row 44); PCall's recovery owns the close
		ls.Push(lv)
		ls.Panic(ls)
	}
}

// GetInfo is lua_getinfo over the fork's Debug struct (U5c: stock's
// option-filtered fields). A leading '>' queries the given function
// instead of dbg.frame's — stock sets ci=NULL there, so the
// frame-relative options answer their no-frame defaults ('l' -1, 'n'
// empty, 't' false/0) while 'S'/'u' still read the function. Returns
// the 'f' payload (nil without 'f'); the activelines table lands in
// Debug.ActiveLines ('L'). An option outside "flnSrtuL" returns the
// invalid-option error the debug library converts to its argerror
// (stock lua_getinfo answers 0 and db_getinfo raises).
func (ls *LState) GetInfo(what string, dbg *Debug, fn LValue) (LValue, error) {
	isfunc := strings.HasPrefix(what, ">")
	if isfunc {
		what = what[1:]
	}
	var f *LFunction
	if isfunc {
		fok, ok := fn.(*LFunction)
		if !ok {
			return LNil, newApiErrorS(ApiErrorRun, "can not get debug info(an object in not a function)")
		}
		f = fok
	} else {
		f = dbg.frame.Fn
	}
	frame := dbg.frame
	if isfunc {
		frame = nil
	}

	retfn := false
	retlines := false
	for _, c := range what {
		switch c {
		case 'f':
			retfn = true
		case 'S':
			// stock funcinfo — mode-independent; 5.5 dropped the "tail"
			// what (a tail-called frame reports "Lua"/"main")
			if f.IsG {
				dbg.What = "C"
				dbg.Source = "=[C]"
				dbg.LineDefined = -1
				dbg.LastLineDefined = -1
			} else {
				// U5f: a stripped chunk's NULL source answers "=?"
				// (stock funcinfo's no-source arm; chunkid drops the "="
				// so short_src renders "?"). An explicit empty chunkname
				// (load(s, "")) is a real empty source — "" as-is, and
				// short_src renders `[string ""]` (db.lua:73)
				dbg.Source = f.Proto.SourceName
				if f.Proto.SourceIsNull {
					dbg.Source = "=?"
				}
				dbg.LineDefined = f.Proto.LineDefined
				dbg.LastLineDefined = f.Proto.LastLineDefined
				if dbg.LineDefined == 0 {
					dbg.What = "main"
				} else {
					dbg.What = "Lua"
				}
			}
		case 'l':
			if frame != nil && !frame.Fn.IsG && frame.Pc > 0 &&
				len(f.Proto.DbgSourcePositions) > 0 { // U5f: stripped → -1
				dbg.CurrentLine = f.Proto.DbgSourcePositions[frame.Pc-1]
			} else {
				dbg.CurrentLine = -1
			}
		case 'u':
			dbg.NUpvalues = len(f.Upvalues)
			if f.IsG {
				dbg.IsVarArg = true
				dbg.NParams = 0
			} else {
				dbg.IsVarArg = f.Proto.IsVarArg != 0
				dbg.NParams = int(f.Proto.NumParameters)
			}
		case 't':
			if frame != nil {
				dbg.IsTailCall = frame.TailCall > 0
				dbg.ExtraArgs = frame.CCmt
			} else {
				dbg.IsTailCall = false
				dbg.ExtraArgs = 0
			}
		case 'n':
			// stock getfuncname: a tail-called frame has no name; the
			// name comes from the CALLER — funcnamefromcall over the
			// caller's current instruction (funcnamefromcode: the call
			// site's getobjname kind, a metamethod event, or the for
			// iterator); a C caller yields nothing. U5f: a caller running
			// a hook (CIST_HOOKED — funcnamefromcall's first arm, before
			// the isLua dispatch) names the hook's own frame "?" / "hook"
			dbg.NameWhat = ""
			dbg.Name = ""
			if frame != nil && frame.TailCall == 0 {
				if parent := frame.Parent; parent != nil {
					if parent.Hooked {
						dbg.NameWhat = "hook"
						dbg.Name = "?"
					} else if parent.Fin {
						// U5f (funcnamefromcall's CIST_FIN arm, before
						// the isLua dispatch): the caller is running this
						// frame as the __gc finalizer — named even over a
						// C caller (a collectgarbage-driven finalizer)
						dbg.NameWhat = "metamethod"
						dbg.Name = "__gc"
					} else if parent.Fn != nil && !parent.Fn.IsG && parent.Fn.Proto != nil {
						if kind, name := funcNameFromCode(parent.Fn.Proto, parent.Pc-1); kind != "" {
							dbg.NameWhat = kind
							dbg.Name = name
						}
					}
				}
			}
		case 'r':
			// U5f (stock auxgetinfo): the transfer window of the event the
			// hook was last invoked for, reported ONLY over the frame it
			// fired on (CIST_HOOKED) — call/return windows for the traced
			// frame, which db.lua's transfer battery reads via getinfo(2)
			// + getlocal(2, ftransfer..). Anything else answers 0/0.
			dbg.FTransfer = 0
			dbg.NTransfer = 0
			if frame != nil && frame.Hooked {
				dbg.FTransfer = ls.hookFTransfer
				dbg.NTransfer = ls.hookNTransfer
			}
		case 'L':
			retlines = true
		default:
			return LNil, newApiErrorS(ApiErrorRun, "invalid option")
		}
	}

	if retlines {
		// stock collectvalidlines: nil for C functions; else
		// {line=true} over the lineinfo — a vararg proto skips
		// instruction 0 (its VARARGPREP, which carries the function's
		// definition line). A stripped proto (NULL lineinfo, U5f-1's
		// string.dump(f, true)) answers an EMPTY table — stock zeroes
		// nlines, and db.lua's getinfo-over-dump battery pins it
		if f.IsG {
			dbg.ActiveLines = nil
		} else {
			p := f.Proto
			dbg.ActiveLines = ls.NewTable()
			i := 0
			if p.IsVarArg != 0 {
				i = 1
			}
			nlines := len(p.DbgSourcePositions)
			if len(p.Code) < nlines {
				nlines = len(p.Code)
			}
			for ; i < nlines; i++ {
				dbg.ActiveLines.RawSetInt(p.DbgSourcePositions[i], LTrue)
			}
		}
	}

	if retfn {
		return f, nil
	}
	return LNil, nil

}

// GetStack is lua_getstack. Cross-thread queries over a SUSPENDED
// thread (U5c) run over stock's continuation model: 5.5 keeps the
// yielded C function's CallInfo alive while the thread is suspended,
// so level 0 is that C frame and the thread's own frames start at
// level 1. The fork's Go stack unwinds the yield's G frame instead —
// the running and normal-at-resume threads keep their boundary frame
// as a REAL frame, only the suspended ones need the shift. Level 0
// hands back a synthetic G frame (C-shaped for 'S'/'u'/'l'/'t'; 'f'
// on it is not stock's yield function — unpinned corner).
func (ls *LState) GetStack(level int) (*Debug, bool) {
	if level >= 0 && ls.G != nil && ls.G.CurrentThread != nil &&
		ls.G.CurrentThread != ls && ls.G.CurrentThread.Parent != ls &&
		ls.stack != nil && !ls.stack.IsEmpty() {
		if level == 0 {
			return &Debug{frame: &callFrame{Fn: newLFunctionG(nil, nil, 0), Parent: ls.currentFrame}}, true
		}
		level--
	}
	frame := ls.currentFrame
	// U5f (stock lua_getstack): one frame = one level — a tail call
	// REPLACES its frame (istailcall is a flag on the surviving ci), it
	// does not leave phantom levels behind. The old `level -= TailCall`
	// walked past the stack for a tail-called callee's caller
	// (db.lua:627's getinfo(2) over h→g1→g→f).
	for ; level > 0 && frame != nil; frame = frame.Parent {
		level--
	}

	if level == 0 && frame != nil {
		return &Debug{frame: frame}, true
	}
	// U5f: stock lua_getstack rejects negative levels outright ("invalid
	// (negative) level" returns 0) — debug.getlocal/setlocal must argerror
	// "level out of range" for them (db.lua:260's pcall battery). The old
	// bottom-frame fallback made debug.setlocal(-1, 1, 10) succeed.
	return &Debug{}, false
}

// findVarargCell (stock findvararg, ldebug.c): the register cell of the
// (-n)-th extra argument of a hidden-model vararg frame — any negative
// index names one, "(vararg)" the generic name. The extras keep the
// cells the caller left them in — [Base+nparams+1, LocalBase) — both
// before and after OP_VARARGPREP's shift (the shuffle moves the fixed
// params above them, never the extras themselves; OP_VARARG reads the
// same window). A table-model function (promoted) has no addressable
// varargs (stock guards on PF_VAHID); C frames have no proto at all.
func (ls *LState) findVarargCell(frame *callFrame, no int) (int, bool) {
	proto := frame.Fn.Proto
	if proto == nil || proto.IsVarArg&VarArgHidden == 0 {
		return 0, false
	}
	np := int(proto.NumParameters)
	nextra := frame.NArgs - np
	if nextra < 0 {
		nextra = 0
	}
	if no < 0 && no >= -nextra {
		return frame.Base + np - no, true
	}
	return 0, false
}

func (ls *LState) GetLocal(dbg *Debug, no int) (string, LValue) {
	frame := dbg.frame
	// U5f: negative indices address the varargs (stock luaG_findlocal
	// routes n < 0 to findvararg before the local-name lookup — the
	// old path fell through to "(temporary)" for them; db.lua's vararg
	// battery pins "(vararg)" and the settable cells)
	if no < 0 {
		if cell, ok := ls.findVarargCell(frame, no); ok {
			return "(vararg)", ls.reg.Get(cell)
		}
		return "", LNil
	}
	if name := ls.findLocal(frame, no); len(name) > 0 {
		return name, ls.reg.Get(frame.LocalBase + no - 1)
	}
	return "", LNil
}

func (ls *LState) SetLocal(dbg *Debug, no int, lv LValue) string {
	frame := dbg.frame
	if no < 0 {
		if cell, ok := ls.findVarargCell(frame, no); ok {
			ls.reg.Set(cell, lv)
			return "(vararg)"
		}
		return ""
	}
	if name := ls.findLocal(frame, no); len(name) > 0 {
		ls.reg.Set(frame.LocalBase+no-1, lv)
		return name
	}
	return ""
}

func (ls *LState) GetUpvalue(fn *LFunction, no int) (string, LValue) {
	no--
	if no >= 0 && no < len(fn.Upvalues) {
		// U5f (stock aux_upvalue's C-closure arm): a Go closure's upvalue
		// is addressable — its name is "" rather than "absent"
		// (db.lua:597: getupvalue(gmatch-closure, 1) == "")
		if fn.IsG {
			return "", fn.Upvalues[no].Value()
		}
		// U5f (stock aux_upvalue's Lua arm): a nameless upvalue — a
		// stripped proto carries no DbgUpvalues — is "(no name)" while
		// the index stays addressable (db.lua:972's chunk-without-debug)
		if no >= len(fn.Proto.DbgUpvalues) {
			return "(no name)", fn.Upvalues[no].Value()
		}
		return fn.Proto.DbgUpvalues[no], fn.Upvalues[no].Value()
	}
	return "", LNil
}

func (ls *LState) SetUpvalue(fn *LFunction, no int, lv LValue) string {
	no--
	if no >= 0 && no < len(fn.Upvalues) {
		fn.Upvalues[no].SetValue(lv)
		if fn.IsG {
			return "" // as GetUpvalue: a C (Go) closure's upvalue name
		}
		if no >= len(fn.Proto.DbgUpvalues) {
			return "(no name)" // as GetUpvalue: stripped, nameless
		}
		return fn.Proto.DbgUpvalues[no]
	}
	return ""
}

/* }}} */

/* env operations {{{ */

func (ls *LState) GetFEnv(obj LValue) LValue {
	switch lv := obj.(type) {
	case *LFunction:
		return lv.Env
	case *LUserData:
		return lv.Env
	case *LState:
		return lv.Env
	}
	return LNil
}

func (ls *LState) SetFEnv(obj LValue, env LValue) {
	tb, ok := env.(*LTable)
	if !ok {
		ls.RaiseError("cannot use %v as an environment", env.Type().String())
	}

	switch lv := obj.(type) {
	case *LFunction:
		lv.Env = tb
	case *LUserData:
		lv.Env = tb
	case *LState:
		lv.Env = tb
	}
	/* do nothing */
}

/* }}} */

/* table operations {{{ */

func (ls *LState) RawGet(tb *LTable, key LValue) LValue {
	return tb.RawGet(key)
}

func (ls *LState) RawGetInt(tb *LTable, key int) LValue {
	return tb.RawGetInt(key)
}

func (ls *LState) GetField(obj LValue, skey string) LValue {
	v, _ := ls.getFieldString(obj, skey, varinfoHint{})
	return v
}

func (ls *LState) GetTable(obj LValue, key LValue) LValue {
	v, _ := ls.getField(obj, key, varinfoHint{})
	return v
}

func (ls *LState) RawSet(tb *LTable, key LValue, value LValue) {
	if n, ok := key.(LNumber); ok && math.IsNaN(float64(n)) {
		ls.RaiseError("table index is NaN")
	} else if key == LNil {
		ls.RaiseError("table index is nil")
	}
	tb.RawSet(key, value)
}

func (ls *LState) RawSetInt(tb *LTable, key int, value LValue) {
	tb.RawSetInt(key, value)
}

func (ls *LState) SetField(obj LValue, key string, value LValue) {
	_ = ls.setFieldString(obj, key, value, varinfoHint{})
}

func (ls *LState) SetTable(obj LValue, key LValue, value LValue) {
	_ = ls.setField(obj, key, value, varinfoHint{})
}

func (ls *LState) ForEach(tb *LTable, cb func(LValue, LValue)) {
	tb.ForEach(cb)
}

func (ls *LState) GetGlobal(name string) LValue {
	return ls.GetField(ls.Get(GlobalsIndex), name)
}

func (ls *LState) SetGlobal(name string, value LValue) {
	ls.SetField(ls.Get(GlobalsIndex), name, value)
}

func (ls *LState) Next(tb *LTable, key LValue) (LValue, LValue) {
	return tb.Next(key)
}

/* }}} */

/* unary operations {{{ */

func (ls *LState) ObjLen(v1 LValue) int {
	if v1.Type() == LTString {
		return len(string(v1.(LString)))
	}
	op := ls.metaOp1(v1, "__len")
	if op.Type() == LTFunction {
		ls.Push(op)
		ls.Push(v1)
		ls.Call(1, 1)
		ret := ls.reg.Pop()
		if iv, ok := ret.(LInteger); ok {
			return int(iv)
		}
		if ret.Type() == LTNumber {
			return int(ret.(LNumber))
		}
	} else if v1.Type() == LTTable {
		return v1.(*LTable).Len()
	}
	return 0
}

/* }}} */

/* binary operations {{{ */

func (ls *LState) Concat(values ...LValue) string {
	top := ls.reg.Top()
	for _, value := range values {
		ls.reg.Push(value)
	}
	ret, _ := stringConcat(ls, len(values), ls.reg.Top()-1, nil)
	ls.reg.SetTop(top)
	return LVAsString(ret)
}

func (ls *LState) LessThan(lhs, rhs LValue) bool {
	v, _ := lessThan(ls, lhs, rhs)
	return v
}

func (ls *LState) Equal(lhs, rhs LValue) bool {
	v, _ := equals(ls, lhs, rhs, false)
	return v
}

func (ls *LState) RawEqual(lhs, rhs LValue) bool {
	v, _ := equals(ls, lhs, rhs, true)
	return v
}

/* }}} */

/* register operations {{{ */

func (ls *LState) Register(name string, fn LGFunction) {
	ls.SetGlobal(name, ls.NewFunction(fn))
}

/* }}} */

/* load and function call operations {{{ */

// Load parses, compiles, and wraps a chunk as a callable function. The
// chunk's _ENV upvalue (D3) is seeded with the thread's environment.
func (ls *LState) Load(reader io.Reader, name string) (*LFunction, error) {
	return ls.loadAux(reader, name, ls.currentEnv())
}

// LoadEnv is Load with an explicit chunk environment (5.2 load's env
// argument): the compiled main chunk's _ENV upvalue cell is seeded with
// env, so the chunk (and every closure it creates) resolves globals
// there. A nil env falls back to the thread environment; 5.5 accepts
// ANY value for env (calls.lua:415's `load(..., 123)` — only actual
// global accesses care that it is a table).
func (ls *LState) LoadEnv(reader io.Reader, name string, env LValue) (*LFunction, error) {
	if env == LNil {
		env = ls.currentEnv()
	}
	return ls.loadAux(reader, name, env)
}

func (ls *LState) loadAux(reader io.Reader, name string, env LValue) (*LFunction, error) {
	chunk, err := parse.Parse(reader, name)
	if err != nil {
		return nil, newApiErrorE(ApiErrorSyntax, err)
	}
	proto, err := Compile(chunk, name)
	if err != nil {
		return nil, newApiErrorE(ApiErrorSyntax, err)
	}
	return ls.newMainFunction(proto, env), nil
}

// loadBinaryAux is loadAux's binary arm (U5f): undump a precompiled
// chunk (dump.go, stock lundump.c) and wrap it as the main function.
// The env folds live in newMainFunction (a Go-nil env = argument absent
// seeds the globals; an explicit value seeds as-is).
func (ls *LState) loadBinaryAux(data []byte, name string, env LValue) (*LFunction, error) {
	proto, err := undumpProto(data, name)
	if err != nil {
		return nil, newApiErrorS(ApiErrorSyntax, err.Error())
	}
	return ls.newMainFunction(proto, env), nil
}

// newMainFunction wraps a compiled (or undumped) main proto as a
// callable (U5f re-pin: stock f_parser + lua_load + load_aux). f_parser's
// luaF_initupvals (lfunc.c) fills EVERY upvalue of the fresh main with a
// new CLOSED nil-valued cell; lua_load's tail (lapi.c) then seeds upvalue
// #1 with the registry globals; an explicit load env argument overwrites
// upvalue #1 again in load_aux. Upvalue #1 is NOT necessarily _ENV — a
// dumped non-main loaded as a chunk keeps its own upvalue order
// (calls.lua:418: `local y=x` makes x upvalue 1 and _ENV 2, and the
// no-env load leaves _ENV nil until setupvalue sets it). The callers
// resolve a nil env to the thread environment first (the interp's
// registry-globals analog).
func (ls *LState) newMainFunction(proto *FunctionProto, env LValue) *LFunction {
	envtable, _ := env.(*LTable)
	if envtable == nil {
		envtable = ls.currentEnv()
	}
	fn := newLFunctionL(proto, envtable, int(proto.NumUpvalues))
	if env == nil {
		env = ls.G.Global // stock lua_load: the registry globals
	}
	for i := range fn.Upvalues {
		uv := &Upvalue{}
		uv.Close()
		uv.SetValue(LNil) // stock setnilvalue: a typed nil, not a nil LValue
		fn.Upvalues[i] = uv
	}
	if len(fn.Upvalues) > 0 {
		fn.Upvalues[0].SetValue(env)
	}
	return fn
}

func (ls *LState) Call(nargs, nret int) {
	ls.callR(nargs, nret, -1)
}

func (ls *LState) PCall(nargs, nret int, errfunc *LFunction) (err error) {
	err = nil
	sp := ls.stack.Sp()
	base := ls.reg.Top() - nargs - 1
	oldpanic := ls.Panic
	ls.Panic = panicWithoutTraceback
	if errfunc != nil {
		ls.hasErrorFunc = true
	}
	defer func() {
		ls.Panic = oldpanic
		ls.hasErrorFunc = false
		rcv := recover()
		if rcv != nil {
			if _, isunwind := rcv.(*closeSelfUnwind); isunwind {
				// U4d (stock luaD_throwbaselevel): a thread closing
				// itself unwinds EVERY pcall level of that thread; no
				// recovery point may catch it (threadRun's recover
				// terminates the thread)
				panic(rcv)
			}
			// close exactly the dying frames' upvalues (register index
			// >= base — every frame the pcall pushed lives at or above
			// it; surviving frames keep theirs open, matching C Lua's
			// luaD_pcall luaF_close(L, old_base)). Raise no longer
			// closes (row 44); without this, recovered slots would be
			// reused while stale open upvalues still alias them.
			ls.closeUpvalues(base)
			if _, ok := rcv.(*ApiError); !ok {
				err = newApiErrorS(ApiErrorPanic, fmt.Sprint(rcv))
				if ls.Options.IncludeGoStackTrace {
					buf := make([]byte, 4096)
					runtime.Stack(buf, false)
					err.(*ApiError).StackTrace = strings.Trim(string(buf), "\000") + "\n" + ls.stackTrace(nil, 0)
				}
			} else {
				err = rcv.(*ApiError)
			}
			if errfunc != nil {
				// U5a (stock luaD_growstack's ERRORSTACKSIZE): a frame-stack
				// overflow leaves the stack FULL, so the message handler call
				// below would re-raise "stack overflow" and never run. Stock
				// reallocs to ERRORSTACKSIZE before raising for exactly this;
				// the fork's fixed frame array cannot grow (VM loops cache
				// *callFrame pointers into it), so the topmost DYING frames
				// unwind temporarily instead — their cells stay in the array,
				// just invisible to the handler's traceback. A handler needing
				// more than the reserve re-raises where stock reports
				// luaD_errerr's "error in error handling" (divergence ledger
				// row). The deferred SetSp(sp) restores the level either way.
				if ls.stack.IsFull() {
					drop := ls.stack.Sp() - errorFrameReserve
					if drop < sp {
						drop = sp
					}
					ls.stack.SetSp(drop)
					ls.currentFrame = ls.stack.Last()
				}
				// U5f: the handler runs through runMsgh's loop — stock
				// luaG_errormsg re-invokes it for every nested raise (C-call
				// counting; errors.lua:636), collapsing to luaD_errerr only
				// on the second-level ceiling or a register-stack overflow
				// (row 78e). A failing handler's frames die inside runMsgh's
				// protected invocation; the outer close dance below is
				// shared with the no-handler arm.
				ls.Panic = panicWithoutTraceback
				obj := ls.runMsgh(errfunc, err.(*ApiError).Object)
				ls.Panic = oldpanic
				err = newApiError(ApiErrorError, obj)
			} else if len(err.(*ApiError).StackTrace) == 0 {
				err.(*ApiError).StackTrace = ls.stackTrace(nil, 0)
			}
			// same call-stack restore before the closes as the errfunc arm
			ls.stack.SetSp(sp)
			if sp == 0 {
				ls.currentFrame = nil
			} else {
				ls.currentFrame = ls.stack.Last()
			}
			ls.closeErrorPath(base, &err)
			ls.stack.SetSp(sp)
			ls.currentFrame = ls.stack.Last()
			ls.reg.SetTop(base)
		}
		ls.stack.SetSp(sp)
		if sp == 0 {
			ls.currentFrame = nil
		}
	}()

	ls.Call(nargs, nret)

	return
}

func (ls *LState) GPCall(fn LGFunction, data LValue) error {
	ls.Push(newLFunctionG(fn, ls.currentEnv(), 0))
	ls.Push(data)
	return ls.PCall(1, MultRet, nil)
}

func (ls *LState) CallByParam(cp P, args ...LValue) error {
	ls.Push(cp.Fn)
	for _, arg := range args {
		ls.Push(arg)
	}

	if cp.Protect {
		return ls.PCall(len(args), cp.NRet, cp.Handler)
	}
	ls.Call(len(args), cp.NRet)
	return nil
}

/* }}} */

/* metatable operations {{{ */

func (ls *LState) GetMetatable(obj LValue) LValue {
	return ls.metatable(obj, false)
}

func (ls *LState) SetMetatable(obj LValue, mt LValue) {
	switch mt.(type) {
	case *LNilType, *LTable:
	default:
		ls.RaiseError("metatable must be a table or nil, but got %v", mt.Type().String())
	}

	switch v := obj.(type) {
	case *LTable:
		v.Metatable = mt
		// U5f: setmetatable time is the finalizer registration point
		// (stock luaC_checkfinalizer) — a non-nil __gc field makes the
		// object collectable-with-finalizer
		ls.syncFinalizer(v, mt)
	case *LUserData:
		if v.Light {
			// U5c: light userdata shares the basic-type table (stock
			// lua_setmetatable's G->mt arm)
			ls.G.builtinMts[basicTypeIndex(v)] = mt
		} else {
			v.Metatable = mt
			ls.syncFinalizer(v, mt) // U5f: as tables, above
		}
	default:
		ls.G.builtinMts[basicTypeIndex(obj)] = mt
	}
}

// basicTypeIndex keys a basic-type metatable by the LUA basic type, not
// the fork's subtype tag: the integer/float pair is one number type
// (U4e — debug.setmetatable(10, mt) must cover 3.45 too, and
// getmetatable(3.0) must see a metatable set through 10).
func basicTypeIndex(v LValue) int {
	if t := v.Type(); t == LTInteger {
		return int(LTNumber)
	} else {
		return int(t)
	}
}

/* }}} */

/* coroutine operations {{{ */

func (ls *LState) Status(th *LState) string {
	status := "suspended"
	if th.Dead {
		status = "dead"
	} else if ls.G.CurrentThread == th {
		status = "running"
	} else if ls.Parent == th {
		status = "normal"
	}
	return status
}

func (ls *LState) Resume(th *LState, fn *LFunction, args ...LValue) (ResumeState, error, []LValue) {
	isstarted := th.isStarted()
	if !isstarted {
		base := 0
		th.stack.Push(callFrame{
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
	}

	if ls.G.CurrentThread == th {
		return ResumeError, newApiErrorS(ApiErrorRun, "cannot resume non-suspended coroutine"), nil
	}
	if th.Dead {
		return ResumeError, newApiErrorS(ApiErrorRun, "cannot resume dead coroutine"), nil
	}
	th.Parent = ls
	ls.G.CurrentThread = th
	if !isstarted {
		cf := th.stack.Last()
		th.currentFrame = cf
		th.SetTop(0)
		for _, arg := range args {
			th.Push(arg)
		}
		cf.NArgs = len(args)
		th.initCallFrame(cf)
		th.Panic = panicWithoutTraceback
		// U5f: a fresh thread's entry is stock luaD_call into the body —
		// the call hook fires at its first instruction (luaG_tracecall;
		// a vararg body defers to its OP_VARARGPREP)
		th.hookCallEvent(th.currentFrame)
	} else {
		for _, arg := range args {
			th.Push(arg)
		}
	}
	top := ls.GetTop()
	threadRun(th)
	haserror := LVIsFalse(ls.Get(top + 1))
	ret := make([]LValue, 0, ls.GetTop())
	for idx := top + 2; idx <= ls.GetTop(); idx++ {
		ret = append(ret, ls.Get(idx))
	}
	if len(ret) == 0 {
		ret = append(ret, LNil)
	}
	ls.SetTop(top)

	if haserror {
		return ResumeError, newApiError(ApiErrorRun, ret[0]), nil
	} else if th.stack.IsEmpty() {
		return ResumeOK, nil, ret
	}
	return ResumeYield, nil, ret
}

func (ls *LState) Yield(values ...LValue) int {
	ls.SetTop(0)
	for _, lv := range values {
		ls.Push(lv)
	}
	return -1
}

func (ls *LState) XMoveTo(other *LState, n int) {
	if ls == other {
		return
	}
	top := ls.GetTop()
	n = intMin(n, top)
	for i := n; i > 0; i-- {
		other.Push(ls.Get(top - i + 1))
	}
	ls.SetTop(top - n)
}

/* }}} */

/* GopherLua original APIs {{{ */

// Set maximum memory size. This function can only be called from the main thread.
func (ls *LState) SetMx(mx int) {
	if ls.Parent != nil {
		ls.RaiseError("sub threads are not allowed to set a memory limit")
	}
	go func() {
		limit := uint64(mx * 1024 * 1024) //MB
		var s runtime.MemStats
		for atomic.LoadInt32(&ls.stop) == 0 {
			runtime.ReadMemStats(&s)
			if s.Alloc >= limit {
				fmt.Println("out of memory")
				os.Exit(3)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}()
}

// SetContext set a context ctx to this LState. The provided ctx must be non-nil.
func (ls *LState) SetContext(ctx context.Context) {
	ls.mainLoop = mainLoopWithContext
	ls.ctx = ctx
}

// Context returns the LState's context. To change the context, use WithContext.
func (ls *LState) Context() context.Context {
	return ls.ctx
}

// RemoveContext removes the context associated with this LState and returns this context.
func (ls *LState) RemoveContext() context.Context {
	oldctx := ls.ctx
	ls.mainLoop = mainLoop
	ls.ctx = nil
	return oldctx
}

// Converts the Lua value at the given acceptable index to the chan LValue.
func (ls *LState) ToChannel(n int) chan LValue {
	if lv, ok := ls.Get(n).(LChannel); ok {
		return (chan LValue)(lv)
	}
	return nil
}

// RemoveCallerFrame removes the stack frame above the current stack frame. This is useful in tail calls. It returns
// the new current frame.
func (ls *LState) RemoveCallerFrame() *callFrame {
	cs := ls.stack
	sp := cs.Sp()
	parentFrame := cs.At(sp - 2)
	currentFrame := cs.At(sp - 1)
	parentsParentFrame := parentFrame.Parent
	*parentFrame = *currentFrame
	parentFrame.Parent = parentsParentFrame
	parentFrame.Idx = sp - 2
	cs.Pop()
	return parentFrame
}

/* }}} */

/* }}} */

//
