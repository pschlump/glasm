package lua

import (
	"fmt"
	"strings"
)

// Vararg model flags (U5b, stock lobject.h PF_*): every vararg function
// starts hidden-args (VARARGPREP copies func+params above the extras and
// shifts the frame); a whole-value use / index write / upvalue capture of
// the named parameter promotes the function to the table model at finish
// time (needvatab — VARARGPREP then builds {1..n, n=n} into the param
// slot, GETVARG rewrites to GETTABLE, VARARG gets k=1). NumParameters
// counts the fixed params only — the vararg parameter itself occupies the
// register AT NumParameters, reserved by parlist like stock's
// `adjustlocalvars(ls, 1)` + `luaK_reserveregs`.
const (
	VarArgHidden uint8 = 1 // PF_VAHID — hidden extra args (stock default)
	VarArgTable  uint8 = 2 // PF_VATAB — materialized vararg table
)

// VarArgAny is the isvararg(p) test mask (either model marks the proto).
const VarArgAny = VarArgHidden | VarArgTable

type DbgLocalInfo struct {
	Name    string
	StartPc int
	EndPc   int
}

type DbgCall struct {
	Name string
	Pc   int
	// Method records a colon-syntax call site (`obj:name(...)`): stock's
	// luaL_argerror does not count the implicit self in the argument
	// number and has a dedicated "bad self" error for arg 1 (U3b).
	Method bool
}

// Upvaldesc is a compiled upvalue-capture descriptor (U2c, stock
// lobject.h Upvaldesc minus the kind field — <const> attributes arrive
// with U5): InStack captures the parent's register Idx, otherwise the
// parent's upvalue Idx. These replace the fork's old pseudo-instructions
// after OP_CLOSURE.
type Upvaldesc struct {
	InStack bool
	Idx     uint8
	Name    string
}

type FunctionProto struct {
	SourceName         string
	LineDefined        int
	LastLineDefined    int
	NumUpvalues        uint8
	NumParameters      uint8
	IsVarArg           uint8
	NumUsedRegisters   uint8
	Code               []uint32
	Constants          []LValue
	FunctionPrototypes []*FunctionProto
	Upvalues           []Upvaldesc

	// EnvUpvalueIndex: the upvalue slot holding _ENV, the table global
	// accesses resolve through (5.2 scheme, upgrade plan D3). -1 when the
	// function references no globals. Used by the loader to seed the
	// main chunk's _ENV cell (GETTABUP/SETTABUP address the cell by the
	// B operand directly).
	EnvUpvalueIndex int8

	DbgSourcePositions []int
	DbgLocals          []*DbgLocalInfo
	DbgCalls           []DbgCall
	DbgUpvalues        []string

	stringConstants []string
}

/* Upvalue {{{ */

type Upvalue struct {
	next   *Upvalue
	reg    *registry
	index  int
	value  LValue
	closed bool
}

func (uv *Upvalue) Value() LValue {
	//if uv.IsClosed() {
	if uv.closed || uv.reg == nil {
		return uv.value
	}
	//return uv.reg.Get(uv.index)
	return uv.reg.array[uv.index]
}

func (uv *Upvalue) SetValue(value LValue) {
	if uv.IsClosed() {
		uv.value = value
	} else {
		uv.reg.Set(uv.index, value)
	}
}

func (uv *Upvalue) Close() {
	value := uv.Value()
	uv.closed = true
	uv.value = value
}

func (uv *Upvalue) IsClosed() bool {
	return uv.closed || uv.reg == nil
}

func UpvalueIndex(i int) int {
	return GlobalsIndex - i
}

/* }}} */

/* FunctionProto {{{ */

func newFunctionProto(name string) *FunctionProto {
	return &FunctionProto{
		SourceName:         name,
		LineDefined:        0,
		LastLineDefined:    0,
		NumUpvalues:        0,
		NumParameters:      0,
		IsVarArg:           0,
		NumUsedRegisters:   2,
		EnvUpvalueIndex:    -1,
		Code:               make([]uint32, 0, 128),
		Constants:          make([]LValue, 0, 32),
		FunctionPrototypes: make([]*FunctionProto, 0, 16),
		Upvalues:           make([]Upvaldesc, 0, 8),

		DbgSourcePositions: make([]int, 0, 128),
		DbgLocals:          make([]*DbgLocalInfo, 0, 16),
		DbgCalls:           make([]DbgCall, 0, 128),
		DbgUpvalues:        make([]string, 0, 16),

		stringConstants: make([]string, 0, 32),
	}
}

// StringConstants exposes the string-constants table used by the
// *KS opcodes and SETGLOBAL (backend access).
func (fp *FunctionProto) StringConstants() []string { return fp.stringConstants }

func (fp *FunctionProto) String() string {
	return fp.str(1, 0)
}

func (fp *FunctionProto) str(level int, count int) string {
	indent := strings.Repeat("  ", level-1)
	buf := []string{}
	buf = append(buf, fmt.Sprintf("%v; function [%v] definition (level %v)\n",
		indent, count, level))
	buf = append(buf, fmt.Sprintf("%v; %v upvalues, %v params, %v stacks\n",
		indent, fp.NumUpvalues, fp.NumParameters, fp.NumUsedRegisters))
	for reg, linfo := range fp.DbgLocals {
		buf = append(buf, fmt.Sprintf("%v.local %v ; %v\n", indent, linfo.Name, reg))
	}
	for reg, upvalue := range fp.DbgUpvalues {
		buf = append(buf, fmt.Sprintf("%v.upvalue %v ; %v\n", indent, upvalue, reg))
	}
	for reg, conzt := range fp.Constants {
		buf = append(buf, fmt.Sprintf("%v.const %v ; %v\n", indent, conzt.String(), reg))
	}
	buf = append(buf, "\n")

	protono := 0
	for no, code := range fp.Code {
		inst := opGetOpCode(code)
		if inst == OP_CLOSURE {
			buf = append(buf, "\n")
			buf = append(buf, fp.FunctionPrototypes[protono].str(level+1, protono))
			buf = append(buf, "\n")
			protono++
		}
		buf = append(buf, fmt.Sprintf("%v[%03d] %v (line:%v)\n",
			indent, no+1, opToString(code), fp.DbgSourcePositions[no]))

	}
	buf = append(buf, fmt.Sprintf("%v; end of function\n", indent))
	return strings.Join(buf, "")
}

/* }}} */

/* LFunction {{{ */

func newLFunctionL(proto *FunctionProto, env *LTable, nupvalue int) *LFunction {
	return &LFunction{
		IsG: false,
		Env: env,

		Proto:     proto,
		GFunction: nil,
		Upvalues:  make([]*Upvalue, nupvalue),
	}
}

func newLFunctionG(gfunc LGFunction, env *LTable, nupvalue int) *LFunction {
	return &LFunction{
		IsG: true,
		Env: env,

		Proto:     nil,
		GFunction: gfunc,
		Upvalues:  make([]*Upvalue, nupvalue),
	}
}

func (fn *LFunction) LocalName(regno, pc int) (string, bool) {
	if fn.IsG {
		return "", false
	}
	p := fn.Proto
	for i := 0; i < len(p.DbgLocals) && p.DbgLocals[i].StartPc < pc; i++ {
		if pc < p.DbgLocals[i].EndPc {
			regno--
			if regno == 0 {
				return p.DbgLocals[i].Name, true
			}
		}
	}
	return "", false
}

/* }}} */
