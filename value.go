package lua

import (
	"context"
	"fmt"
	"os"
	"strconv"
)

type LValueType int

const (
	LTNil LValueType = iota
	LTBool
	LTNumber
	LTString
	LTFunction
	LTUserData
	LTThread
	LTTable
	LTChannel
	// LTInteger is the 5.3+ integer subtype (U2/D1). It is a distinct
	// LValueType so type switches can tell int from float, but it names
	// "number": Lua's type() must keep reporting the basic type.
	LTInteger
)

var lValueNames = [10]string{"nil", "boolean", "number", "string", "function", "userdata", "thread", "table", "channel", "number"}

func (vt LValueType) String() string {
	return lValueNames[int(vt)]
}

type LValue interface {
	String() string
	Type() LValueType
}

// LVIsFalse returns true if a given LValue is a nil or false otherwise false.
func LVIsFalse(v LValue) bool { return v == LNil || v == LFalse }

// LVIsFalse returns false if a given LValue is a nil or false otherwise true.
func LVAsBool(v LValue) bool { return v != LNil && v != LFalse }

// LVAsString returns string representation of a given LValue
// if the LValue is a string or number, otherwise an empty string.
func LVAsString(v LValue) string {
	switch sn := v.(type) {
	case LString, LNumber, LInteger:
		return sn.String()
	default:
		return ""
	}
}

// LVCanConvToString returns true if a given LValue is a string or number
// otherwise false.
func LVCanConvToString(v LValue) bool {
	switch v.(type) {
	case LString, LNumber, LInteger:
		return true
	default:
		return false
	}
}

// LVAsNumber tries to convert a given LValue to a number (float view).
// The integer subtype converts to its float value; use parseNumber when
// the syntactic subtype of a string must be preserved.
func LVAsNumber(v LValue) LNumber {
	switch lv := v.(type) {
	case LNumber:
		return lv
	case LInteger:
		return LNumber(float64(lv))
	case LString:
		if num, err := parseNumber(string(lv)); err == nil {
			return LVAsNumber(num)
		}
	}
	return LNumber(0)
}

type LNilType struct{}

func (nl *LNilType) String() string   { return "nil" }
func (nl *LNilType) Type() LValueType { return LTNil }

var LNil = LValue(&LNilType{})

type LBool bool

func (bl LBool) String() string {
	if bool(bl) {
		return "true"
	}
	return "false"
}
func (bl LBool) Type() LValueType { return LTBool }

var LTrue = LBool(true)
var LFalse = LBool(false)

// LInteger is the 64-bit integer subtype of "number" (5.3+, U2/D1).
// Integer-ness is observable: tostring(2) == "2" while tostring(2.0) ==
// "2.0" (once float printing re-pins at U5), table keys normalize
// t[2.0] to the integer 2, and //, shifts and integer for-loops require
// it. Arithmetic on mixed subtypes promotes to float.
type LInteger int64

func (nm LInteger) String() string {
	return strconv.FormatInt(int64(nm), 10)
}

func (nm LInteger) Type() LValueType { return LTInteger }

// fmt.Formatter interface
func (nm LInteger) Format(f fmt.State, c rune) {
	switch c {
	case 'q', 's':
		defaultFormat(nm.String(), f, c)
	case 'b', 'c', 'd', 'o', 'x', 'X', 'U', 'i':
		defaultFormat(int64(nm), f, c)
	case 'e', 'E', 'f', 'F', 'g', 'G':
		defaultFormat(float64(nm), f, c)
	default:
		defaultFormat(int64(nm), f, c)
	}
}

type LString string

func (st LString) String() string   { return string(st) }
func (st LString) Type() LValueType { return LTString }

// fmt.Formatter interface
func (st LString) Format(f fmt.State, c rune) {
	switch c {
	case 'd', 'i':
		if nm, err := parseNumber(string(st)); err != nil {
			defaultFormat(LVAsNumber(nm), f, 'd')
		} else {
			defaultFormat(string(st), f, 's')
		}
	default:
		defaultFormat(string(st), f, c)
	}
}

// String is the engine-wide float spelling: the stock tostringbuffFloat
// (%.15g, re-read, %.17g, ".0" on integer-looking results — U3i, the D7
// re-pin pulled forward because math.lua's numeral round-trip asserts
// tostring(1.0) == "1.0"). The old Go dialect printed "1" there.
func (nm LNumber) String() string {
	return numberToStringStock(float64(nm))
}

func (nm LNumber) Type() LValueType { return LTNumber }

// fmt.Formatter interface
func (nm LNumber) Format(f fmt.State, c rune) {
	switch c {
	case 'q', 's':
		defaultFormat(nm.String(), f, c)
	case 'b', 'c', 'd', 'o', 'x', 'X', 'U':
		defaultFormat(int64(nm), f, c)
	case 'e', 'E', 'f', 'F', 'g', 'G':
		defaultFormat(float64(nm), f, c)
	case 'i':
		defaultFormat(int64(nm), f, 'd')
	default:
		if isInteger(nm) {
			defaultFormat(int64(nm), f, c)
		} else {
			defaultFormat(float64(nm), f, c)
		}
	}
}

type LTable struct {
	Metatable LValue

	array   []LValue
	dict    map[LValue]LValue
	strdict map[string]LValue
	keys    []LValue
	k2i     map[LValue]int
}

func (tb *LTable) String() string   { return fmt.Sprintf("table: %p", tb) }
func (tb *LTable) Type() LValueType { return LTTable }

type LFunction struct {
	IsG       bool
	Env       *LTable
	Proto     *FunctionProto
	GFunction LGFunction
	Upvalues  []*Upvalue
}
type LGFunction func(*LState) int

func (fn *LFunction) String() string   { return fmt.Sprintf("function: %p", fn) }
func (fn *LFunction) Type() LValueType { return LTFunction }

type Global struct {
	MainThread    *LState
	CurrentThread *LState
	Registry      *LTable
	Global        *LTable

	// randState is the math.random xoshiro256** state (U3b). Stock keeps
	// it in a userdata upvalue shared by the math functions; the Go side
	// hangs it on the Global — one stream per lua_State, shared across
	// coroutines, observationally identical.
	randState rngState

	// resumeDepth counts nested coroutine resumes (U4d). Stock bounds
	// them with nCcalls >= LUAI_MAXCCALLS ("C stack overflow"); the fork
	// has no C-call counter, so coResume tracks the resume chain depth
	// here — without it, mutual resume recursion overflows the Go stack
	// fatally (official coroutine.lua's "infinite recursion" block).
	resumeDepth int

	builtinMts map[int]LValue
	tempFiles  []*os.File
	gccount    int32
}

// luaMaxCCalls mirrors stock LUAI_MAXCCALLS (ldo.h): the nested C-call
// ceiling, applied here to nested resumes.
const luaMaxCCalls = 200

type LState struct {
	G       *Global
	Parent  *LState
	Env     *LTable
	Panic   func(*LState)
	Dead    bool
	Options Options

	stop         int32
	reg          *registry
	stack        callFrameStack
	alloc        *allocator
	currentFrame *callFrame
	wrapped      bool
	uvcache      *Upvalue
	hasErrorFunc bool
	mainLoop     func(*LState, *callFrame)
	ctx          context.Context
	ctxCancelFn  context.CancelFunc
	// deadErr records the error object a coroutine died with (U3h:
	// coroutine.close returns it as false+err for an error-dead
	// coroutine, stock lua_closethread's status).
	deadErr LValue
	// tbclist is the to-be-closed stack (U4, stock L->tbclist): entries
	// keyed by the same absolute register indexes the upvalue registry
	// uses, innermost (highest) first, so cross-frame ordering holds and
	// closeTBC(level) reaches exactly the entries a block/function owns.
	tbclist *tbcEntry
}

// tbcEntry is one to-be-closed variable registration (U4). The value
// lives in the register slot; it is read at close time (assignment to a
// <close> local is a compile error, so the slot is stable).
type tbcEntry struct {
	idx  int
	prev *tbcEntry
}

func (ls *LState) String() string   { return fmt.Sprintf("thread: %p", ls) }
func (ls *LState) Type() LValueType { return LTThread }

type LUserData struct {
	Value     interface{}
	Env       *LTable
	Metatable LValue
}

func (ud *LUserData) String() string   { return fmt.Sprintf("userdata: %p", ud) }
func (ud *LUserData) Type() LValueType { return LTUserData }

type LChannel chan LValue

func (ch LChannel) String() string   { return fmt.Sprintf("channel: %p", ch) }
func (ch LChannel) Type() LValueType { return LTChannel }
