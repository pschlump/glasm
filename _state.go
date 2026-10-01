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

	"github.com/pschlump/golua55/parse"
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
	What            string
	Source          string
	CurrentLine     int
	NUpvalues       int
	LineDefined     int
	LastLineDefined int
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
		builtinMts: make(map[int]LValue),
		tempFiles:  make([]*os.File, 0, 10),
	}
}

/* }}} */

/* package local methods {{{ */

func panicWithTraceback(L *LState) {
	err := newApiError(ApiErrorRun, L.Get(-1))
	err.StackTrace = L.stackTrace(0)
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
	if top-frame.LocalBase >= no {
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
		// =literal, or [string "..."]) — ChunkID is the shared renderer
		sourcename = parse.ChunkID(proto.SourceName)
	} else if skipg {
		return ls.where(level+1, skipg)
	}
	line := ""
	if proto != nil {
		line = fmt.Sprintf("%v:", proto.DbgSourcePositions[cf.Pc-1])
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
	return fmt.Sprintf("%v:%v:", parse.ChunkID(cf.Fn.Proto.SourceName), cf.Fn.Proto.DbgSourcePositions[cf.Pc-1])
}

func (ls *LState) stackTrace(level int) string {
	buf := []string{}
	header := "stack traceback:"
	if ls.currentFrame != nil {
		i := 0
		for dbg, ok := ls.GetStack(i); ok; dbg, ok = ls.GetStack(i) {
			cf := dbg.frame
			buf = append(buf, fmt.Sprintf("\t%v in %v", ls.Where(i), ls.formattedFrameFuncName(cf)))
			if !cf.Fn.IsG && cf.TailCall > 0 {
				for tc := cf.TailCall; tc > 0; tc-- {
					buf = append(buf, "\t(tailcall): ?")
					i++
				}
			}
			i++
		}
	}
	buf = append(buf, fmt.Sprintf("\t%v: %v", "[G]", "?"))
	buf = buf[intMax(0, intMin(level, len(buf))):len(buf)]
	if len(buf) > 20 {
		newbuf := make([]string, 0, 20)
		newbuf = append(newbuf, buf[0:7]...)
		newbuf = append(newbuf, "\t...")
		newbuf = append(newbuf, buf[len(buf)-7:len(buf)]...)
		buf = newbuf
	}
	return fmt.Sprintf("%s\n%s", header, strings.Join(buf, "\n"))
}

func (ls *LState) formattedFrameFuncName(fr *callFrame) string {
	name, ischunk := ls.frameFuncName(fr)
	if ischunk {
		return name
	}
	// U5a: VM-dispatched frames (metamethods, for iterators) render
	// stock's namewhat form ("in metamethod 'close'"), not "function '...'"
	if strings.HasPrefix(name, "metamethod '") || name == "for iterator" {
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
		metatable = obj.Metatable
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

func (ls *LState) metaCall(lvalue LValue) (*LFunction, bool) {
	if fn, ok := lvalue.(*LFunction); ok {
		return fn, false
	}
	if fn, ok := ls.metaOp1(lvalue, "__call").(*LFunction); ok {
		return fn, true
	}
	return nil, false
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

		if (proto.IsVarArg & VarArgIsVarArg) == 0 {
			if nargs < int(proto.NumUsedRegisters) {
				nargs = int(proto.NumUsedRegisters)
			}
			newSize := cf.LocalBase + nargs
			// +inline-call ls.reg.checkSize newSize
			for i := np; i < nargs; i++ {
				ls.reg.array[cf.LocalBase+i] = LNil
			}
			ls.reg.top = cf.LocalBase + int(proto.NumUsedRegisters)
		} else {
			/* swap vararg positions:
					   closure
					   namedparam1 <- lbase
					   namedparam2
					   vararg1
					   vararg2

			           TO

					   closure
					   nil
					   nil
					   vararg1
					   vararg2
					   namedparam1 <- lbase
					   namedparam2
			*/
			nvarargs := nargs - np
			if nvarargs < 0 {
				nvarargs = 0
			}

			ls.reg.SetTop(cf.LocalBase + nargs + np)
			for i := 0; i < np; i++ {
				//ls.reg.Set(cf.LocalBase+nargs+i, ls.reg.Get(cf.LocalBase+i))
				ls.reg.array[cf.LocalBase+nargs+i] = ls.reg.array[cf.LocalBase+i]
				//ls.reg.Set(cf.LocalBase+i, LNil)
				ls.reg.array[cf.LocalBase+i] = LNil
			}

			if CompatVarArg {
				ls.reg.SetTop(cf.LocalBase + nargs + np + 1)
				if (proto.IsVarArg & VarArgNeedsArg) != 0 {
					argtb := newLTable(nvarargs, 0)
					for i := 0; i < nvarargs; i++ {
						argtb.RawSetInt(i+1, ls.reg.Get(cf.LocalBase+np+i))
					}
					argtb.RawSetString("n", LNumber(nvarargs))
					//ls.reg.Set(cf.LocalBase+nargs+np, argtb)
					ls.reg.array[cf.LocalBase+nargs+np] = argtb
				} else {
					ls.reg.array[cf.LocalBase+nargs+np] = LNil
				}
			}
			cf.LocalBase += nargs
			maxreg := cf.LocalBase + int(proto.NumUsedRegisters)
			ls.reg.SetTop(maxreg)
		}
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

func (ls *LState) callR(nargs, nret, rbase int) {
	base := ls.reg.Top() - nargs - 1
	if rbase < 0 {
		rbase = base
	}
	lv := ls.reg.Get(base)
	fn, meta := ls.metaCall(lv)
	ls.pushCallFrame(callFrame{
		Fn:         fn,
		Pc:         0,
		Base:       base,
		LocalBase:  base + 1,
		ReturnBase: rbase,
		NArgs:      nargs,
		NRet:       nret,
		Parent:     ls.currentFrame,
		TailCall:   0,
	}, lv, meta)
	if ls.G.MainThread == nil {
		ls.G.MainThread = ls
		ls.G.CurrentThread = ls
		ls.mainLoop(ls, nil)
	} else {
		ls.mainLoop(ls, ls.currentFrame)
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

func (ls *LState) getField(obj LValue, key LValue, hint varinfoHint) LValue {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			ret := tb.RawGet(key)
			if ret != LNil {
				return ret
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
			return LNil
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(key)
			ls.Call(2, 1)
			return ls.reg.Pop()
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in gettable")
	return nil
}

func (ls *LState) getFieldString(obj LValue, key string, hint varinfoHint) LValue {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			ret := tb.RawGetString(key)
			if ret != LNil {
				return ret
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
			return LNil
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(LString(key))
			ls.Call(2, 1)
			return ls.reg.Pop()
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in gettable")
	return nil
}

func (ls *LState) setField(obj LValue, key LValue, value LValue, hint varinfoHint) {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			if tb.RawGet(key) != LNil {
				ls.RawSet(tb, key, value)
				return
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
			return
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(key)
			ls.reg.Push(value)
			ls.Call(3, 0)
			return
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in settable")
}

func (ls *LState) setFieldString(obj LValue, key string, value LValue, hint varinfoHint) {
	curobj := obj
	for i := 0; i < MaxTableGetLoop; i++ {
		tb, istable := curobj.(*LTable)
		if istable {
			if tb.RawGetString(key) != LNil {
				tb.RawSetString(key, value)
				return
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
			return
		}
		if metaindex.Type() == LTFunction {
			ls.reg.Push(metaindex)
			ls.reg.Push(curobj)
			ls.reg.Push(LString(key))
			ls.reg.Push(value)
			ls.Call(3, 0)
			return
		} else {
			curobj = metaindex
		}
	}
	ls.RaiseError("too many recursions in settable")
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

func (ls *LState) GetInfo(what string, dbg *Debug, fn LValue) (LValue, error) {
	if !strings.HasPrefix(what, ">") {
		fn = dbg.frame.Fn
	} else {
		what = what[1:]
	}
	f, ok := fn.(*LFunction)
	if !ok {
		return LNil, newApiErrorS(ApiErrorRun, "can not get debug info(an object in not a function)")
	}

	retfn := false
	for _, c := range what {
		switch c {
		case 'f':
			retfn = true
		case 'S':
			if dbg.frame != nil && dbg.frame.Parent == nil {
				dbg.What = "main"
			} else if f.IsG {
				// U4d stock pin: builtin (Go) functions are C functions —
				// getinfo().what is "C" (coroutine.lua:249 checks the
				// __close caller's level; db.lua:38 pins it generally)
				dbg.What = "C"
			} else if dbg.frame != nil && dbg.frame.TailCall > 0 {
				dbg.What = "tail"
			} else {
				dbg.What = "Lua"
			}
			if !f.IsG {
				dbg.Source = f.Proto.SourceName
				dbg.LineDefined = f.Proto.LineDefined
				dbg.LastLineDefined = f.Proto.LastLineDefined
			}
		case 'l':
			if !f.IsG && dbg.frame != nil {
				if dbg.frame.Pc > 0 {
					dbg.CurrentLine = f.Proto.DbgSourcePositions[dbg.frame.Pc-1]
				}
			} else {
				dbg.CurrentLine = -1
			}
		case 'u':
			dbg.NUpvalues = len(f.Upvalues)
		case 'n':
			if dbg.frame != nil {
				dbg.Name = ls.rawFrameFuncName(dbg.frame)
			}
		default:
			return LNil, newApiErrorS(ApiErrorRun, "invalid what: "+string(c))
		}
	}

	if retfn {
		return f, nil
	}
	return LNil, nil

}

func (ls *LState) GetStack(level int) (*Debug, bool) {
	frame := ls.currentFrame
	for ; level > 0 && frame != nil; frame = frame.Parent {
		level--
		if !frame.Fn.IsG {
			level -= frame.TailCall
		}
	}

	if level == 0 && frame != nil {
		return &Debug{frame: frame}, true
	} else if level < 0 && ls.stack.Sp() > 0 {
		return &Debug{frame: ls.stack.At(0)}, true
	}
	return &Debug{}, false
}

func (ls *LState) GetLocal(dbg *Debug, no int) (string, LValue) {
	frame := dbg.frame
	if name := ls.findLocal(frame, no); len(name) > 0 {
		return name, ls.reg.Get(frame.LocalBase + no - 1)
	}
	return "", LNil
}

func (ls *LState) SetLocal(dbg *Debug, no int, lv LValue) string {
	frame := dbg.frame
	if name := ls.findLocal(frame, no); len(name) > 0 {
		ls.reg.Set(frame.LocalBase+no-1, lv)
		return name
	}
	return ""
}

func (ls *LState) GetUpvalue(fn *LFunction, no int) (string, LValue) {
	if fn.IsG {
		return "", LNil
	}

	no--
	if no >= 0 && no < len(fn.Upvalues) {
		return fn.Proto.DbgUpvalues[no], fn.Upvalues[no].Value()
	}
	return "", LNil
}

func (ls *LState) SetUpvalue(fn *LFunction, no int, lv LValue) string {
	if fn.IsG {
		return ""
	}

	no--
	if no >= 0 && no < len(fn.Upvalues) {
		fn.Upvalues[no].SetValue(lv)
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
	return ls.getFieldString(obj, skey, varinfoHint{})
}

func (ls *LState) GetTable(obj LValue, key LValue) LValue {
	return ls.getField(obj, key, varinfoHint{})
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
	ls.setFieldString(obj, key, value, varinfoHint{})
}

func (ls *LState) SetTable(obj LValue, key LValue, value LValue) {
	ls.setField(obj, key, value, varinfoHint{})
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
	ret := stringConcat(ls, len(values), ls.reg.Top()-1, nil)
	ls.reg.SetTop(top)
	return LVAsString(ret)
}

func (ls *LState) LessThan(lhs, rhs LValue) bool {
	return lessThan(ls, lhs, rhs)
}

func (ls *LState) Equal(lhs, rhs LValue) bool {
	return equals(ls, lhs, rhs, false)
}

func (ls *LState) RawEqual(lhs, rhs LValue) bool {
	return equals(ls, lhs, rhs, true)
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
// there. A nil env falls back to the thread environment.
func (ls *LState) LoadEnv(reader io.Reader, name string, env *LTable) (*LFunction, error) {
	if env == nil {
		env = ls.currentEnv()
	}
	return ls.loadAux(reader, name, env)
}

func (ls *LState) loadAux(reader io.Reader, name string, env *LTable) (*LFunction, error) {
	chunk, err := parse.Parse(reader, name)
	if err != nil {
		return nil, newApiErrorE(ApiErrorSyntax, err)
	}
	proto, err := Compile(chunk, name)
	if err != nil {
		return nil, newApiErrorE(ApiErrorSyntax, err)
	}
	fn := newLFunctionL(proto, env, int(proto.NumUpvalues))
	// seed the main chunk's _ENV cell (the only upvalue a main proto
	// declares — Compile registers it at index 0)
	if idx := proto.EnvUpvalueIndex; idx >= 0 {
		uv := &Upvalue{}
		uv.Close()
		uv.SetValue(env)
		fn.Upvalues[idx] = uv
	}
	return fn, nil
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
					err.(*ApiError).StackTrace = strings.Trim(string(buf), "\000") + "\n" + ls.stackTrace(0)
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
				ls.Push(errfunc)
				ls.Push(err.(*ApiError).Object)
				ls.Panic = panicWithoutTraceback
				defer func() {
					ls.Panic = oldpanic
					rcv := recover()
					if rcv != nil {
						if _, isunwind := rcv.(*closeSelfUnwind); isunwind {
							panic(rcv) // base-level unwind: never caught here
						}
						// the errfunc itself panicked — its frames die too
						ls.closeUpvalues(base)
						if _, ok := rcv.(*ApiError); !ok {
							err = newApiErrorS(ApiErrorPanic, fmt.Sprint(rcv))
							if ls.Options.IncludeGoStackTrace {
								buf := make([]byte, 4096)
								runtime.Stack(buf, false)
								err.(*ApiError).StackTrace = strings.Trim(string(buf), "\000") + ls.stackTrace(0)
							}
						} else {
							err = rcv.(*ApiError)
							err.(*ApiError).StackTrace = ls.stackTrace(0)
						}
						// U5a: restore the CALL STACK before the error-path
						// closes (stock luaD_pcall's L->ci = old_ci precedes
						// luaD_closeprotected) — the dying frames must be
						// off the frame stack when __close metamethods run,
						// so the metamethod's caller is the recovering
						// boundary (locals.lua's error-in-__close battery
						// pins debug.getinfo(2).name == "pcall" inside
						// them). The REGISTER top stays high: SetTop clears
						// cells above it and the to-be-closed values still
						// live there.
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
				}()
				ls.Call(1, 1)
				err = newApiError(ApiErrorError, ls.Get(-1))
			} else if len(err.(*ApiError).StackTrace) == 0 {
				err.(*ApiError).StackTrace = ls.stackTrace(0)
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
	case *LUserData:
		v.Metatable = mt
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
