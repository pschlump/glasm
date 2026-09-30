package lua

import (
	"fmt"
	"math"
	"reflect"

	"github.com/pschlump/golua55/ast"
	"github.com/pschlump/golua55/parse"
)

/* internal constants & structs  {{{ */

const maxRegisters = 200

type expContextType int

const (
	ecGlobal expContextType = iota
	ecUpvalue
	ecLocal
	ecTable
	ecVararg
	ecMethod
	ecNone
)

const regNotDefined = opMaxArgsA + 1
const labelNoJump = 0

type expcontext struct {
	ctype expContextType
	reg   int
	// varargopt >= 0: wants varargopt+1 results, i.e  a = func()
	// varargopt = -1: ignore results             i.e  func()
	// varargopt = -2: receive all results        i.e  a = {func()}
	varargopt int
}

// table-key addressing modes for assignment/constructor stores (the 5.5
// encoding splits keyed stores into SETFIELD/SETI/SETTABLE).
type keyModeType int

const (
	keyReg  keyModeType = iota // key lives in a register
	keyKStr                    // key is a pooled string constant (K[B])
	keyInt                     // key is an unsigned immediate (B)
)

type assigncontext struct {
	ec          *expcontext
	keyMode     keyModeType
	keyReg      int
	keyData     int // K index (keyKStr) or immediate (keyInt)
	valuerk     int // register or kMark|kidx (RKC value operand)
	needmove    bool
	envLocalIdx int
}

type lblabels struct {
	t int
	f int
	e int
	b bool
}

type constLValueExpr struct {
	ast.ExprBase

	Value LValue
}

/* }}} */

/* utilities {{{ */
var _ecnone0 = &expcontext{ecNone, regNotDefined, 0}
var _ecnonem1 = &expcontext{ecNone, regNotDefined, -1}
var _ecnonem2 = &expcontext{ecNone, regNotDefined, -2}
var ecfuncdef = &expcontext{ecMethod, regNotDefined, 0}

func ecupdate(ec *expcontext, ctype expContextType, reg, varargopt int) {
	if ec == _ecnone0 || ec == _ecnonem1 || ec == _ecnonem2 {
		panic("can not update ec cache")
	}
	ec.ctype = ctype
	ec.reg = reg
	ec.varargopt = varargopt
}

func ecnone(varargopt int) *expcontext {
	switch varargopt {
	case 0:
		return _ecnone0
	case -1:
		return _ecnonem1
	case -2:
		return _ecnonem2
	}
	return &expcontext{ecNone, regNotDefined, varargopt}
}

func shouldmove(ec *expcontext, reg int) bool {
	return ec.ctype == ecLocal && ec.reg != regNotDefined && ec.reg != reg
}

func sline(pos ast.PositionHolder) int {
	return pos.Line()
}

func eline(pos ast.PositionHolder) int {
	line := pos.LastLine()
	if line == 0 {
		return pos.Line()
	}
	return line
}

func savereg(ec *expcontext, reg int) int {
	if ec.ctype != ecLocal || ec.reg == regNotDefined {
		return reg
	}
	return ec.reg
}

func raiseCompileError(context *funcContext, line int, format string, args ...interface{}) {
	msg := fmt.Sprintf(format, args...)
	panic(&CompileError{context: context, Line: line, Message: msg})
}

func isVarArgReturnExpr(expr ast.Expr) bool {
	switch ex := expr.(type) {
	case *ast.FuncCallExpr:
		return !ex.AdjustRet
	case *ast.Comma3Expr:
		return !ex.AdjustRet
	}
	return false
}

/* utilities }}} */

type gotoLabelDesc struct { // {{{
	Id                 int
	Name               string
	Pc                 int
	Line               int
	NumActiveLocalVars int
	// NeedClose (stock Labeldesc.close): the goto was hoisted out of a
	// block with upvalues/tbc while escaping some variable's scope — its
	// CLOSE placeholder is required when the goto resolves.
	NeedClose bool
}

func newLabelDesc(id int, name string, pc, line, n int) *gotoLabelDesc {
	return &gotoLabelDesc{
		Id:                 id,
		Name:               name,
		Pc:                 pc,
		Line:               line,
		NumActiveLocalVars: n,
	}
} // }}}

type CompileError struct { // {{{
	context *funcContext
	Line    int
	Message string
}

func (e *CompileError) Error() string {
	return fmt.Sprintf("%v:%d: %s", parse.ChunkID(e.context.Proto.SourceName), e.Line, e.Message)
} // }}}

type codeStore struct { // {{{
	codes []uint32
	lines []int
	pc    int
}

func (cd *codeStore) Add(inst uint32, line int) {
	if l := len(cd.codes); l <= 0 || cd.pc == l {
		cd.codes = append(cd.codes, inst)
		cd.lines = append(cd.lines, line)
	} else {
		cd.codes[cd.pc] = inst
		cd.lines[cd.pc] = line
	}
	cd.pc++
}

func (cd *codeStore) AddABC(op int, a int, b int, c int, line int) {
	cd.Add(opCreateABC(op, a, b, c), line)
}

func (cd *codeStore) AddABCk(op int, a int, b int, c int, k int, line int) {
	cd.Add(opCreateABCk(op, a, b, c, k), line)
}

func (cd *codeStore) AddvABCk(op int, a int, b int, c int, k int, line int) {
	cd.Add(opCreatevABCk(op, a, b, c, k), line)
}

func (cd *codeStore) AddABx(op int, a int, bx int, line int) {
	cd.Add(opCreateABx(op, a, bx), line)
}

// AddAsBx: the sBx (17-bit signed) form — LOADI/LOADF.
func (cd *codeStore) AddAsBx(op int, a int, sbx int, line int) {
	cd.Add(opCreateAsBx(op, a, sbx), line)
}

// AddAx: the Ax (25-bit) form — EXTRAARG.
func (cd *codeStore) AddAx(op int, ax int, line int) {
	cd.Add(opCreateAx(op, ax), line)
}

// AddSJ: the sJ (25-bit signed) form — OP_JMP. During compilation the
// payload holds the label id; patchCode rewrites it to the pc-relative
// offset (jump target - (pc + 1), stock fixjump's convention).
func (cd *codeStore) AddSJ(op int, sj int, line int) {
	cd.Add(opCreateSJ(op, sj), line)
}

// PropagateKMV pops a trailing scratch LOADK (K index within the 8-bit
// RKC range) so the value folds into the consuming instruction's k-flag
// operand; otherwise the value stays in a fresh register.
func (cd *codeStore) PropagateKMV(top int, save *int, reg *int, inc int) {
	lastinst := cd.Last()
	if opGetArgA(lastinst) >= top {
		switch opGetOpCode(lastinst) {
		case OP_LOADK:
			cindex := opGetArgBx(lastinst)
			if cindex <= opMaxIndexK {
				cd.Pop()
				*save = opRkAsk(cindex)
				return
			}
		case OP_MOVE:
			cd.Pop()
			*save = opGetArgB(lastinst)
			return
		}
	}
	*save = *reg
	*reg = *reg + inc
}

func (cd *codeStore) PropagateMV(top int, save *int, reg *int, inc int) {
	lastinst := cd.Last()
	if opGetArgA(lastinst) >= top {
		switch opGetOpCode(lastinst) {
		case OP_MOVE:
			cd.Pop()
			*save = opGetArgB(lastinst)
			return
		}
	}
	*save = *reg
	*reg = *reg + inc
}

func (cd *codeStore) SetOpCode(pc int, v int) {
	opSetOpCode(&cd.codes[pc], v)
}

func (cd *codeStore) SetA(pc int, v int) {
	opSetArgA(&cd.codes[pc], v)
}

func (cd *codeStore) SetB(pc int, v int) {
	opSetArgB(&cd.codes[pc], v)
}

func (cd *codeStore) SetC(pc int, v int) {
	opSetArgC(&cd.codes[pc], v)
}

func (cd *codeStore) SetvB(pc int, v int) {
	opSetArgvB(&cd.codes[pc], v)
}

func (cd *codeStore) SetvC(pc int, v int) {
	opSetArgvC(&cd.codes[pc], v)
}

func (cd *codeStore) Setk(pc int, v int) {
	opSetArgk(&cd.codes[pc], v)
}

func (cd *codeStore) SetBx(pc int, v int) {
	opSetArgBx(&cd.codes[pc], v)
}

func (cd *codeStore) SetAx(pc int, v int) {
	opSetArgAx(&cd.codes[pc], v)
}

func (cd *codeStore) SetSJ(pc int, v int) {
	opSetArgsJ(&cd.codes[pc], v)
}

func (cd *codeStore) At(pc int) uint32 {
	return cd.codes[pc]
}

func (cd *codeStore) List() []uint32 {
	return cd.codes[:cd.pc]
}

func (cd *codeStore) PosList() []int {
	return cd.lines[:cd.pc]
}

func (cd *codeStore) LastPC() int {
	return cd.pc - 1
}

func (cd *codeStore) Last() uint32 {
	if cd.pc == 0 {
		return opInvalidInstruction
	}
	return cd.codes[cd.pc-1]
}

func (cd *codeStore) Pop() {
	cd.pc--
} /* }}} Code */

/* {{{ VarNamePool */

type varNamePoolValue struct {
	Index int
	Name  string
}

type varNamePool struct {
	names []string
	// kinds holds the variable kind per name (U4; stock lparser.h VDK*/
	// Vardesc kinds — see the varKind* constants). Parallel to names;
	// separate per block, so sibling-block locals sharing a register
	// never share a kind.
	kinds  []int
	offset int
}

func newVarNamePool(offset int) *varNamePool {
	return &varNamePool{make([]string, 0, 16), make([]int, 0, 16), offset}
}

func (vp *varNamePool) Names() []string {
	return vp.names
}

func (vp *varNamePool) List() []varNamePoolValue {
	result := make([]varNamePoolValue, len(vp.names), len(vp.names))
	for i, name := range vp.names {
		result[i].Index = i + vp.offset
		result[i].Name = name
	}
	return result
}

func (vp *varNamePool) LastIndex() int {
	return vp.offset + len(vp.names)
}

func (vp *varNamePool) Find(name string) int {
	for i := len(vp.names) - 1; i >= 0; i-- {
		if vp.names[i] == name {
			return i + vp.offset
		}
	}
	return -1
}

func (vp *varNamePool) RegisterUnique(name string) int {
	index := vp.Find(name)
	if index < 0 {
		return vp.Register(name)
	}
	return index
}

func (vp *varNamePool) Register(name string) int {
	vp.names = append(vp.names, name)
	vp.kinds = append(vp.kinds, varKindReg)
	return len(vp.names) - 1 + vp.offset
}

// RegisterKind is Register with an explicit variable kind (U4 attributes).
func (vp *varNamePool) RegisterKind(name string, kind int) int {
	vp.names = append(vp.names, name)
	vp.kinds = append(vp.kinds, kind)
	return len(vp.names) - 1 + vp.offset
}

// kindAt is the kind of the local at global (function-wide) index.
func (vp *varNamePool) kindAt(index int) int {
	return vp.kinds[index-vp.offset]
}

// countKind counts the locals of a kind declared in this block's pool.
func (vp *varNamePool) countKind(kind int) int {
	n := 0
	for _, k := range vp.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

/*
	Local variable kinds (U4; stock lparser.h's VDK* numbers — only the

three the fork models at U4: the RDKCTC compile-time constants and the
GDK* global-declaration kinds arrive with U5's resolver).
*/
const (
	varKindReg   = 0 // VDKREG — a regular local
	varKindConst = 1 // RDKCONST — <const>: assignment-locked
	varKindTBC   = 3 // RDKTOCLOSE — <close>: assignment-locked + OP_TBC
)

// attribToKind maps an AST attribute string to a variable kind.
func attribToKind(attrib string) int {
	switch attrib {
	case "const":
		return varKindConst
	case "close":
		return varKindTBC
	}
	return varKindReg
}

/* }}} VarNamePool */

/* FuncContext {{{ */

type codeBlock struct {
	LocalVars  *varNamePool
	BreakLabel int
	Parent     *codeBlock
	RefUpvalue bool
	LineStart  int
	LastLine   int
	// firstLabelIndex / firstGotoIndex (stock BlockCnt.firstlabel /
	// firstgoto): indexes into the function-wide active-label and pending-
	// goto lists captured at block entry. A block's solveGotos matches only
	// its own labels (>= firstLabelIndex) and only its own gotos (>=
	// firstGotoIndex); everything else hoists outward.
	firstLabelIndex int
	firstGotoIndex  int
	// entryNactvar is the active-local count at block entry (stock
	// bl->nactvar) — the level a hoisted goto drops to, and the level a
	// last-statement label's scope is pinned to.
	entryNactvar int
}

func newCodeBlock(localvars *varNamePool, blabel int, parent *codeBlock, pos ast.PositionHolder, firstLabelIndex, firstGotoIndex, entryNactvar int) *codeBlock {
	bl := &codeBlock{localvars, blabel, parent, false, 0, 0, firstLabelIndex, firstGotoIndex, entryNactvar}
	if pos != nil {
		bl.LineStart = pos.Line()
		bl.LastLine = pos.LastLine()
	}
	return bl
}

func (b *codeBlock) LocalVarsCount() int {
	count := 0
	for block := b; block != nil; block = block.Parent {
		count += len(block.LocalVars.Names())
	}
	return count
}

type funcContext struct {
	Proto    *FunctionProto
	Code     *codeStore
	Parent   *funcContext
	Upvalues *varNamePool
	Block    *codeBlock
	Blocks   []*codeBlock
	regTop   int
	labelId  int
	labelPc  map[int]int
	// labels (stock ls->dyd->label, per function): the ACTIVE labels,
	// block-truncated at LeaveBlock — a label is visible only while its
	// block is open. gotos (ls->dyd->gt): pending gotos, order-preserving,
	// removed as they resolve; both lists share one desc type.
	labels []*gotoLabelDesc
	gotos  []*gotoLabelDesc
	// NeedsClose (stock fs->needclose): this function's frames can hold
	// open upvalues or to-be-closed variables at return — the RETURN
	// k bit / RETURN0→RETURN rewrite in compileReturnStmt.
	NeedsClose bool
	// tbcActive is stock's insidetbc, time-ordered: it counts <close>
	// declarations compiled but not yet closed (decremented when the
	// owning block leaves). OP_TAILCALL promotion is suppressed while
	// set — the frame's closes must not be skipped.
	tbcActive int
}

func newFuncContext(sourcename string, parent *funcContext) *funcContext {
	fc := &funcContext{
		Proto:    newFunctionProto(sourcename),
		Code:     &codeStore{make([]uint32, 0, 1024), make([]int, 0, 1024), 0},
		Parent:   parent,
		Upvalues: newVarNamePool(0),
		Block:    newCodeBlock(newVarNamePool(0), labelNoJump, nil, nil, 0, 0, 0),
		regTop:   0,
		labelId:  1,
		labelPc:  map[int]int{},
	}
	fc.Blocks = []*codeBlock{fc.Block}
	return fc
}

// findActiveLabel (stock findlabel): the first active label named name at
// or after list index ilb.
func (fc *funcContext) findActiveLabel(name string, ilb int) *gotoLabelDesc {
	for i := ilb; i < len(fc.labels); i++ {
		if fc.labels[i].Name == name {
			return fc.labels[i]
		}
	}
	return nil
}

// localNameAtCount is the name of the local at function-wide active count
// (stock getlocalvardesc(fs, nactvar)) — the variable whose scope a goto
// would have to enter.
func (fc *funcContext) localNameAtCount(count int) string {
	var chain []*codeBlock
	for block := fc.Block; block != nil; block = block.Parent {
		chain = append(chain, block)
	}
	for i := len(chain) - 1; i >= 0; i-- {
		for _, name := range chain[i].LocalVars.Names() {
			if count == 0 {
				return name
			}
			count--
		}
	}
	return "?"
}

// closeGoto (stock closegoto): resolve gotos[g] against label; bup is the
// finishing block's upvalue flag. A goto's code is [JMP][dead CLOSE]
// (newgotoentry's placeholder); when the jump needs a close the pair
// SWAPS so the CLOSE runs first, at the label's variable level.
func (fc *funcContext) closeGoto(g int, label *gotoLabelDesc, bup bool) {
	gt := fc.gotos[g]
	if gt.NumActiveLocalVars < label.NumActiveLocalVars {
		raiseCompileError(fc, label.Line+1, "<goto %s> at line %d jumps into the scope of '%s'",
			gt.Name, gt.Line, fc.localNameAtCount(gt.NumActiveLocalVars))
	}
	if gt.NeedClose || (label.NumActiveLocalVars < gt.NumActiveLocalVars && bup) {
		code := fc.Code
		code.codes[gt.Pc+1] = code.codes[gt.Pc] // jump moves after the close
		code.codes[gt.Pc] = opCreateABC(OP_CLOSE, label.NumActiveLocalVars, 0, 0)
		gt.Pc++
	}
	fc.Code.SetSJ(gt.Pc, label.Id)
	fc.gotos = append(fc.gotos[:g], fc.gotos[g+1:]...)
}

// solveGotos (stock solvegotos at leaveblock): each pending goto of THIS
// block matches a label of THIS block (>= firstLabelIndex); the rest hoist
// to the block's entry level — dropping out of a variable's scope in a
// block with upvalues marks them for a close. The block's labels retire.
func (fc *funcContext) solveGotos() {
	bl := fc.Block
	for igt := bl.firstGotoIndex; igt < len(fc.gotos); {
		gt := fc.gotos[igt]
		if lb := fc.findActiveLabel(gt.Name, bl.firstLabelIndex); lb != nil {
			fc.closeGoto(igt, lb, bl.RefUpvalue)
		} else {
			if bl.RefUpvalue && gt.NumActiveLocalVars > bl.entryNactvar {
				gt.NeedClose = true
			}
			gt.NumActiveLocalVars = bl.entryNactvar
			igt++
		}
	}
	fc.labels = fc.labels[:bl.firstLabelIndex]
}

// finishFunctionGotos is the function-root arm of stock leaveblock: the
// root block's gotos resolve, and any still pending is the error.
func (fc *funcContext) finishFunctionGotos() {
	fc.solveGotos()
	if len(fc.gotos) > 0 {
		gt := fc.gotos[0]
		raiseCompileError(fc, fc.Proto.LastLineDefined, "no visible label '%s' for <goto> at line %d", gt.Name, gt.Line)
	}
}

func (fc *funcContext) NewLabel() int {
	ret := fc.labelId
	fc.labelId++
	return ret
}

func (fc *funcContext) SetLabelPc(label int, pc int) {
	fc.labelPc[label] = pc
}

func (fc *funcContext) GetLabelPc(label int) int {
	return fc.labelPc[label]
}

// constEqual: LValue equality for the constant pool that keeps -0.0
// and +0.0 distinct. Go's float == (and therefore interface ==) treats
// them equal, so the dedup used to fold a source `0` onto a `-0.0`
// slot (or vice versa — whichever literal interned first won), which
// is observable (1/x, %.14g printing) and deviates from stock Lua 5.1,
// which does not dedup constants at all (row 42, found by the M6e
// fuzzer). NaN literals never dedup (NaN != NaN), matching their
// never-equal semantics.
func constEqual(a, b LValue) bool {
	na, ok := a.(LNumber)
	if !ok {
		return a == b
	}
	nb, ok := b.(LNumber)
	if !ok {
		return false
	}
	return na == nb && math.Signbit(float64(na)) == math.Signbit(float64(nb))
}

func (fc *funcContext) ConstIndex(value LValue) int {
	ctype := value.Type()
	for i, lv := range fc.Proto.Constants {
		if lv.Type() == ctype && constEqual(lv, value) {
			return i
		}
	}
	fc.Proto.Constants = append(fc.Proto.Constants, value)
	v := len(fc.Proto.Constants) - 1
	if v > opMaxArgAx {
		raiseCompileError(fc, fc.Proto.LineDefined, "too many constants")
	}
	return v
}

// kStringIndex interns a string constant for a K-operand slot (8-bit)
// and reports whether the index fits the K range. The 5.5 encoding
// addresses shortstring keys through B/C fields; an over-pooled key takes
// stock's fallback (luaK_indexed/isKstr): the key (and, for upvalue
// tables, the table itself) loads into a register and the access uses the
// register-form opcode (GETTABLE/SETTABLE). U3a: the fork used to raise
// "constant table overflow" here — stock never does; large main chunks
// (official strings.lua/math.lua) need the fallback.
func (fc *funcContext) kStringIndex(s string, line int) (int, bool) {
	idx := fc.ConstIndex(LString(s))
	return idx, idx <= opMaxIndexK
}

// loadStringK loads a string constant into a register (LOADK, or
// LOADKX+EXTRAARG past the Bx range) — the K-overflow fallback's key
// materializer.
func loadStringK(fc *funcContext, reg int, s string, line int) {
	cindex := fc.ConstIndex(LString(s))
	if cindex <= opMaxArgBx {
		fc.Code.AddABx(OP_LOADK, reg, cindex, line)
	} else {
		fc.Code.AddABx(OP_LOADKX, reg, 0, line)
		fc.Code.AddAx(OP_EXTRAARG, cindex, line)
	}
}

func (fc *funcContext) BlockLocalVarsCount() int {
	count := 0
	for block := fc.Block; block != nil; block = block.Parent {
		count += len(block.LocalVars.Names())
	}
	return count
}

func (fc *funcContext) RegisterLocalVar(name string) int {
	ret := fc.Block.LocalVars.Register(name)
	fc.Proto.DbgLocals = append(fc.Proto.DbgLocals, &DbgLocalInfo{Name: name, StartPc: fc.Code.LastPC() + 1})
	fc.SetRegTop(fc.RegTop() + 1)
	return ret
}

// RegisterLocalVarKind is RegisterLocalVar with a variable kind (U4
// attributes: <const>/<close> locals; for-loop control variables declare
// varKindConst like stock lparser.c).
func (fc *funcContext) RegisterLocalVarKind(name string, kind int) int {
	ret := fc.Block.LocalVars.RegisterKind(name, kind)
	fc.Proto.DbgLocals = append(fc.Proto.DbgLocals, &DbgLocalInfo{Name: name, StartPc: fc.Code.LastPC() + 1})
	fc.SetRegTop(fc.RegTop() + 1)
	return ret
}

// checkReadOnlyLocal raises stock's const-assignment error (check_readonly,
// lparser.c) when name resolves to a const or to-be-closed local of this
// function, or to a const local of an enclosing function reached through
// the upvalue chain. Line is the assignment's (the LHS name's).
func checkReadOnlyLocal(context *funcContext, name string, line int) {
	if idx, block := context.FindLocalVarAndBlock(name); idx > -1 {
		if block.LocalVars.kindAt(idx) != varKindReg {
			raiseCompileError(context, line, "attempt to assign to const variable '%s'", name)
		}
		return
	}
	if capturedUpvalueKind(context, name) != varKindReg {
		raiseCompileError(context, line, "attempt to assign to const variable '%s'", name)
	}
}

// capturedUpvalueKind resolves the variable kind behind a name this
// function captures from an enclosing one (stock's Upvaldesc.kind,
// computed by walking the chain — the capture descriptors are appended
// after the child body compiles, so the descriptors themselves are not
// readable yet; the walk always terminates at some ancestor's local).
func capturedUpvalueKind(context *funcContext, name string) int {
	for fn := context.Parent; fn != nil; fn = fn.Parent {
		if idx, block := fn.FindLocalVarAndBlock(name); idx > -1 {
			return block.LocalVars.kindAt(idx)
		}
		if fn.Upvalues.Find(name) > -1 {
			continue // passthrough capture: keep walking
		}
	}
	return varKindReg
}

func (fc *funcContext) FindLocalVarAndBlock(name string) (int, *codeBlock) {
	for block := fc.Block; block != nil; block = block.Parent {
		if index := block.LocalVars.Find(name); index > -1 {
			return index, block
		}
	}
	return -1, nil
}

func (fc *funcContext) FindLocalVar(name string) int {
	idx, _ := fc.FindLocalVarAndBlock(name)
	return idx
}

func (fc *funcContext) LocalVars() []varNamePoolValue {
	result := make([]varNamePoolValue, 0, 32)
	for _, block := range fc.Blocks {
		result = append(result, block.LocalVars.List()...)
	}
	return result
}

func (fc *funcContext) EnterBlock(blabel int, pos ast.PositionHolder) {
	entryNactvar := fc.BlockLocalVarsCount()
	fc.Block = newCodeBlock(newVarNamePool(fc.RegTop()), blabel, fc.Block, pos, len(fc.labels), len(fc.gotos), entryNactvar)
	fc.Blocks = append(fc.Blocks, fc.Block)
}

func (fc *funcContext) CloseUpvalues() int {
	n := -1
	if fc.Block.RefUpvalue {
		n = fc.Block.Parent.LocalVars.LastIndex()
		fc.Code.AddABC(OP_CLOSE, n, 0, 0, fc.Block.LastLine)
	}
	return n
}

func (fc *funcContext) LeaveBlock() int {
	closed := fc.CloseUpvalues()
	fc.EndScope()

	// U4: the block's <close> declarations are closed by its OP_CLOSE /
	// the enclosing return — tailcall promotion is allowed again below
	// them (stock leaveblock releasing bl->insidetbc).
	fc.tbcActive -= fc.Block.LocalVars.countKind(varKindTBC)

	// U4c (stock solvegotos at leaveblock): resolve this block's gotos
	// against its labels, hoist the rest, retire the labels
	fc.solveGotos()

	fc.Block = fc.Block.Parent
	fc.SetRegTop(fc.Block.LocalVars.LastIndex())
	return closed
}

func (fc *funcContext) EndScope() {
	for _, vr := range fc.Block.LocalVars.List() {
		fc.Proto.DbgLocals[vr.Index].EndPc = fc.Code.LastPC()
	}
}

func (fc *funcContext) SetRegTop(top int) {
	if top > maxRegisters {
		raiseCompileError(fc, fc.Proto.LineDefined, "too many local variables")
	}
	fc.regTop = top
}

func (fc *funcContext) RegTop() int {
	return fc.regTop
}

/* FuncContext }}} */

/* U2c: constant/immediate operand plumbing {{{ */

// operand kinds for the constant-selection paths (stock lcode.c's
// VKINT/VKFLT/VK "keep the numeral" discipline adapted to the fork's
// compile-then-propagate flow).
type opndKind int

const (
	opndReg   opndKind = iota // value lives in a register
	opndInt                   // integer constant (LOADI pop or pooled)
	opndFloat                 // float constant with an exact integer value
	opndK                     // any other pooled constant (LOADK pop)
)

type opnd struct {
	kind    opndKind
	reg     int // register when kind == opndReg
	kidx    int // pool index when pooled (-1 for LOADI/LOADF immediates)
	ival    int64
	fval    float64
	isFloat bool // the source literal was a float (orderI's C flag)
}

// compileConstOperand compiles expr into register `reg`. When the emitted
// code was a scratch constant load above the live registers, the load is
// popped and the constant returned as the operand (numericOnly selects
// between arith operands and comparison operands, which may keep strings
// for EQK).
func compileConstOperand(fc *funcContext, expr ast.Expr, reg int, numericOnly bool) (opnd, int) {
	startpc := fc.Code.LastPC()
	compileExpr(fc, reg, expr, ecnone(0))
	last := fc.Code.Last()
	// The trailing load is attributable to the expression only when it is
	// the expression's ENTIRE code (U4e): an and/or operand ends with the
	// never-executed other arm's load behind a jump, and popping that
	// steals the dead arm's value while the live arm stays in the
	// register — `5 + (23 or 2)` compiled to `23 + 2` (events.lua's
	// basic-type metatable battery). Multi-instruction code keeps the
	// register form; the value is already where it belongs.
	if fc.Code.LastPC() == startpc+1 && opGetArgA(last) >= fc.RegTop() {
		switch opGetOpCode(last) {
		case OP_LOADI:
			fc.Code.Pop()
			return opnd{kind: opndInt, ival: int64(opGetArgSbx(last)), kidx: -1}, reg
		case OP_LOADF:
			fc.Code.Pop()
			sb := int64(opGetArgSbx(last))
			return opnd{kind: opndFloat, ival: sb, fval: float64(sb), isFloat: true, kidx: -1}, reg
		case OP_LOADK:
			idx := opGetArgBx(last)
			if idx <= opMaxIndexK {
				switch n := fc.Proto.Constants[idx].(type) {
				case LInteger:
					fc.Code.Pop()
					return opnd{kind: opndInt, ival: int64(n), kidx: idx}, reg
				case LNumber:
					fc.Code.Pop()
					o := opnd{kind: opndK, kidx: idx, fval: float64(n)}
					if iv, ok := floatToIntExact(float64(n)); ok {
						o = opnd{kind: opndFloat, ival: iv, fval: float64(n), isFloat: true, kidx: idx}
					}
					return o, reg
				default:
					if !numericOnly {
						fc.Code.Pop()
						return opnd{kind: opndK, kidx: idx}, reg
					}
				}
			}
		}
	}
	return opnd{kind: opndReg, reg: reg}, reg + 1
}

// materializeReg re-emits a constant operand into a register (the
// selection fell back to a register form).
func materializeReg(fc *funcContext, o opnd, reg int, line int) {
	switch o.kind {
	case opndInt:
		if fitsBx(o.ival) {
			fc.Code.AddAsBx(OP_LOADI, reg, int(o.ival), line)
		} else {
			fc.Code.AddABx(OP_LOADK, reg, materializeK(fc, o), line)
		}
	case opndFloat:
		if o.kidx >= 0 {
			fc.Code.AddABx(OP_LOADK, reg, o.kidx, line)
		} else if fitsBx(o.ival) {
			fc.Code.AddAsBx(OP_LOADF, reg, int(o.ival), line)
		} else {
			fc.Code.AddABx(OP_LOADK, reg, materializeK(fc, o), line)
		}
	default:
		fc.Code.AddABx(OP_LOADK, reg, o.kidx, line)
	}
}

// materializeK interns a constant operand into the pool (K-operand form),
// returning the index.
func materializeK(fc *funcContext, o opnd) int {
	if o.kidx >= 0 {
		return o.kidx
	}
	switch o.kind {
	case opndInt:
		return fc.ConstIndex(LInteger(o.ival))
	default:
		return fc.ConstIndex(LNumber(o.fval))
	}
}

// loadNumber emits the stock luaK_int/luaK_float selection: integers and
// floats with an exact integer value inside the sBx range load inline
// (LOADI/LOADF), everything else goes through the constant pool (LOADK,
// or LOADKX+EXTRAARG past the Bx range).
func loadNumber(fc *funcContext, reg int, v LValue, line int) {
	code := fc.Code
	if iv, ok := v.(LInteger); ok {
		if fitsBx(int64(iv)) {
			code.AddAsBx(OP_LOADI, reg, int(iv), line)
			return
		}
	} else if fv, ok := v.(LNumber); ok {
		if fi, ok2 := floatToIntExact(float64(fv)); ok2 && fitsBx(fi) {
			code.AddAsBx(OP_LOADF, reg, int(fi), line)
			return
		}
	}
	cindex := fc.ConstIndex(v)
	if cindex <= opMaxArgBx {
		code.AddABx(OP_LOADK, reg, cindex, line)
	} else {
		code.AddABx(OP_LOADKX, reg, 0, line)
		code.AddAx(OP_EXTRAARG, cindex, line)
	}
}

// intKeyLiteral reports the immediate table-key value of a literal
// integer expression in 0..255 (stock isCint — the GETI/SETI range).
func intKeyLiteral(expr ast.Expr) (int, bool) {
	if ex, ok := expr.(*ast.NumberExpr); ok {
		if lv, err := parseNumber(ex.Value); err == nil {
			if iv, ok := lv.(LInteger); ok && int64(iv) >= 0 && int64(iv) <= opMaxArgsC {
				return int(iv), true
			}
		}
	}
	return 0, false
}

/* }}} */

func compileChunk(context *funcContext, chunk []ast.Stmt, untilFollows bool) { // {{{
	for i, stmt := range chunk {
		lastStmt := true
		for j := i + 1; j < len(chunk); j++ {
			_, ok := chunk[j].(*ast.LabelStmt)
			if !ok {
				lastStmt = false
				break
			}
		}
		compileStmt(context, stmt, lastStmt && !untilFollows)
	}
} // }}}

func compileBlock(context *funcContext, chunk []ast.Stmt) { // {{{
	if len(chunk) == 0 {
		return
	}
	ph := &ast.Node{}
	ph.SetLine(sline(chunk[0]))
	ph.SetLastLine(eline(chunk[len(chunk)-1]))
	context.EnterBlock(labelNoJump, ph)
	for i, stmt := range chunk {
		lastStmt := true
		for j := i + 1; j < len(chunk); j++ {
			_, ok := chunk[j].(*ast.LabelStmt)
			if !ok {
				lastStmt = false
				break
			}
		}
		compileStmt(context, stmt, lastStmt)
	}
	context.LeaveBlock()
} // }}}

func compileStmt(context *funcContext, stmt ast.Stmt, isLastStmt bool) { // {{{
	switch st := stmt.(type) {
	case *ast.AssignStmt:
		compileAssignStmt(context, st)
	case *ast.LocalAssignStmt:
		compileLocalAssignStmt(context, st)
	case *ast.FuncCallStmt:
		compileFuncCallExpr(context, context.RegTop(), st.Expr.(*ast.FuncCallExpr), ecnone(-1))
	case *ast.DoBlockStmt:
		context.EnterBlock(labelNoJump, st)
		compileChunk(context, st.Stmts, false)
		context.LeaveBlock()
	case *ast.WhileStmt:
		compileWhileStmt(context, st)
	case *ast.RepeatStmt:
		compileRepeatStmt(context, st)
	case *ast.FuncDefStmt:
		compileFuncDefStmt(context, st)
	case *ast.ReturnStmt:
		compileReturnStmt(context, st)
	case *ast.IfStmt:
		compileIfStmt(context, st)
	case *ast.BreakStmt:
		compileBreakStmt(context, st)
	case *ast.NumberForStmt:
		compileNumberForStmt(context, st)
	case *ast.GenericForStmt:
		compileGenericForStmt(context, st)
	case *ast.LabelStmt:
		compileLabelStmt(context, st, isLastStmt)
	case *ast.GotoStmt:
		compileGotoStmt(context, st)
	}
} // }}}

func compileAssignStmtLeft(context *funcContext, stmt *ast.AssignStmt) (int, []*assigncontext) { // {{{
	reg := context.RegTop()
	acs := make([]*assigncontext, 0, len(stmt.Lhs))
	for _, lhs := range stmt.Lhs {
		switch st := lhs.(type) {
		case *ast.IdentExpr:
			identtype := getIdentRefType(context, context, st)
			// U4: stock restassign's check_readonly — const/tbc locals
			// and const upvalues are assignment targets no longer.
			checkReadOnlyLocal(context, st.Value, sline(st))
			ec := &expcontext{identtype, regNotDefined, 0}
			ac := &assigncontext{ec: ec, envLocalIdx: -1}
			switch identtype {
			case ecGlobal:
				context.ConstIndex(LString(st.Value))
				if envlocal, _ := envAccess(context); envlocal > -1 {
					ac.envLocalIdx = envlocal
				}
			case ecUpvalue:
				context.Upvalues.RegisterUnique(st.Value)
			case ecLocal:
				ec.reg = context.FindLocalVar(st.Value)
			}
			acs = append(acs, ac)
		case *ast.AttrGetExpr:
			ac := &assigncontext{ec: &expcontext{ecTable, regNotDefined, 0}, envLocalIdx: -1}
			compileExprWithKMVPropagation(context, st.Object, &reg, &ac.ec.reg)
			if str, ok := st.Key.(*ast.StringExpr); ok {
				if kidx, fits := context.kStringIndex(str.Value, sline(st)); fits {
					ac.keyMode = keyKStr
					ac.keyData = kidx
				} else {
					// K-overflow fallback: key into a register
					ac.keyMode = keyReg
					ac.keyReg = reg
					loadStringK(context, reg, str.Value, sline(st))
					reg++
				}
			} else if idx, ok := intKeyLiteral(st.Key); ok {
				ac.keyMode = keyInt
				ac.keyData = idx
			} else {
				ac.keyMode = keyReg
				ac.keyReg = reg
				reg += compileExpr(context, reg, st.Key, ecnone(0))
			}
			acs = append(acs, ac)

		default:
			panic("invalid left expression.")
		}
	}
	return reg, acs
} // }}}

func compileAssignStmtRight(context *funcContext, stmt *ast.AssignStmt, reg int, acs []*assigncontext) (int, []*assigncontext) { // {{{
	lennames := len(stmt.Lhs)
	lenexprs := len(stmt.Rhs)
	namesassigned := 0

	for namesassigned < lennames {
		ac := acs[namesassigned]
		ec := ac.ec
		var expr ast.Expr = nil
		if namesassigned >= lenexprs {
			expr = &ast.NilExpr{}
			expr.SetLine(sline(stmt.Lhs[namesassigned]))
			expr.SetLastLine(eline(stmt.Lhs[namesassigned]))
		} else if isVarArgReturnExpr(stmt.Rhs[namesassigned]) && (lenexprs-namesassigned-1) <= 0 {
			varargopt := lennames - namesassigned - 1
			regstart := reg
			reginc := compileExpr(context, reg, stmt.Rhs[namesassigned], ecnone(varargopt))
			reg += reginc
			for i := namesassigned; i < namesassigned+int(reginc); i++ {
				acs[i].needmove = true
				if acs[i].ec.ctype == ecTable {
					acs[i].valuerk = regstart + (i - namesassigned)
				}
			}
			namesassigned = lennames
			continue
		}

		if expr == nil {
			expr = stmt.Rhs[namesassigned]
		}
		idx := reg
		if ec.ctype == ecLocal && localAssignNeedsTemp(stmt, namesassigned) {
			// Multiple assignment is simultaneous (Lua 5.1: all RHS
			// expressions are evaluated before any assignment is
			// performed). Compiling the RHS straight into the target
			// local's register would clobber it before a later RHS
			// expression reads the old value (a, b = b, a+b evaluated
			// sequentially). Hazardous targets evaluate into a window
			// slot instead; the reverse assignment loop below moves it,
			// which also restores stock 5.1's leftmost-wins order for
			// duplicate targets (a, a = 1, 2 → 1). (Upstream
			// yuin/golua55 has the sequential bug; found by _cli-tests
			// fib vs C Lua.) varargopt 0: a non-last RHS adjusts to
			// exactly one value (`...` and call results alike); the
			// multi-return case is the branch above.
			reginc := compileExpr(context, reg, expr, ecnone(0))
			ac.needmove = reginc != 0
			reg += reginc
		} else {
			reginc := compileExpr(context, reg, expr, ec)
			if ec.ctype == ecTable {
				if _, ok := expr.(*ast.LogicalOpExpr); !ok {
					context.Code.PropagateKMV(context.RegTop(), &ac.valuerk, &reg, reginc)
				} else {
					ac.valuerk = idx
					reg += reginc
				}
			} else {
				ac.needmove = reginc != 0
				reg += reginc
			}
		}
		namesassigned += 1
	}

	rightreg := reg - 1

	// extra right exprs
	for i := namesassigned; i < lenexprs; i++ {
		varargopt := -1
		if i != lenexprs-1 {
			varargopt = 0
		}
		reg += compileExpr(context, reg, stmt.Rhs[i], ecnone(varargopt))
	}
	return rightreg, acs
} // }}}

// localAssignNeedsTemp reports whether the local target at index i of a
// multiple assignment must not be direct-written: a later RHS expression
// still reads the target's old value (a, b = b, a+b), or the same local
// is assigned twice (a, a = 1, 2 — the reverse assignment loop must own
// the ordering). Anything the walker cannot classify reads the target
// conservatively (table/function literals may mention or capture it).
func localAssignNeedsTemp(stmt *ast.AssignStmt, i int) bool {
	name := stmt.Lhs[i].(*ast.IdentExpr).Value
	dups := 0
	for _, lhs := range stmt.Lhs {
		if id, ok := lhs.(*ast.IdentExpr); ok && id.Value == name {
			dups++
		}
	}
	if dups > 1 {
		return true
	}
	for j := i + 1; j < len(stmt.Rhs); j++ {
		if exprMentionsIdent(stmt.Rhs[j], name) {
			return true
		}
	}
	return false
}

// exprMentionsIdent reports whether expr reads the local `name`.
func exprMentionsIdent(expr ast.Expr, name string) bool { // {{{
	switch ex := expr.(type) {
	case *ast.IdentExpr:
		return ex.Value == name
	case *ast.AttrGetExpr:
		return exprMentionsIdent(ex.Object, name) || exprMentionsIdent(ex.Key, name)
	case *ast.FuncCallExpr:
		if exprMentionsIdent(ex.Func, name) {
			return true
		}
		if ex.Receiver != nil && exprMentionsIdent(ex.Receiver, name) {
			return true
		}
		for _, arg := range ex.Args {
			if exprMentionsIdent(arg, name) {
				return true
			}
		}
		return false
	case *ast.LogicalOpExpr:
		return exprMentionsIdent(ex.Lhs, name) || exprMentionsIdent(ex.Rhs, name)
	case *ast.RelationalOpExpr:
		return exprMentionsIdent(ex.Lhs, name) || exprMentionsIdent(ex.Rhs, name)
	case *ast.ArithmeticOpExpr:
		return exprMentionsIdent(ex.Lhs, name) || exprMentionsIdent(ex.Rhs, name)
	case *ast.StringConcatOpExpr:
		return exprMentionsIdent(ex.Lhs, name) || exprMentionsIdent(ex.Rhs, name)
	case *ast.UnaryMinusOpExpr:
		return exprMentionsIdent(ex.Expr, name)
	case *ast.UnaryNotOpExpr:
		return exprMentionsIdent(ex.Expr, name)
	case *ast.UnaryLenOpExpr:
		return exprMentionsIdent(ex.Expr, name)
	case *ast.UnaryBNotOpExpr:
		return exprMentionsIdent(ex.Expr, name)
	case *ast.Comma3Expr, *ast.StringExpr, *ast.NumberExpr,
		*ast.TrueExpr, *ast.FalseExpr, *ast.NilExpr:
		return false
	default:
		// table constructors, function literals, const exprs: may read
		// or capture the target — treat as a mention
		return true
	}
} // }}}

func compileAssignStmt(context *funcContext, stmt *ast.AssignStmt) { // {{{
	code := context.Code
	lennames := len(stmt.Lhs)
	reg, acs := compileAssignStmtLeft(context, stmt)
	reg, acs = compileAssignStmtRight(context, stmt, reg, acs)

	for i := lennames - 1; i >= 0; i-- {
		ex := stmt.Lhs[i]
		switch acs[i].ec.ctype {
		case ecLocal:
			if acs[i].needmove {
				code.AddABC(OP_MOVE, context.FindLocalVar(ex.(*ast.IdentExpr).Value), reg, 0, sline(ex))
				reg -= 1
			}
		case ecGlobal:
			name := ex.(*ast.IdentExpr).Value
			kidx, fits := context.kStringIndex(name, sline(ex))
			// the value lives in a register for global targets (only
			// ecTable targets propagate their value into a K operand)
			if acs[i].envLocalIdx > -1 {
				// D3: _ENV shadowed by a local — the global write is a
				// keyed table write on that register
				if fits {
					code.AddABCk(OP_SETFIELD, acs[i].envLocalIdx, kidx, reg, 0, sline(ex))
				} else {
					loadStringK(context, reg+1, name, sline(ex))
					code.AddABCk(OP_SETTABLE, acs[i].envLocalIdx, reg+1, reg, 0, sline(ex))
				}
			} else if fits {
				code.AddABCk(OP_SETTABUP, envUpvalueIndexOf(context), kidx, reg, 0, sline(ex))
			} else {
				// K-overflow fallback (stock luaK_indexed): the upvalue
				// table and the key both go into registers
				code.AddABC(OP_GETUPVAL, reg+2, envUpvalueIndexOf(context), 0, sline(ex))
				loadStringK(context, reg+1, name, sline(ex))
				code.AddABCk(OP_SETTABLE, reg+2, reg+1, reg, 0, sline(ex))
			}
			reg -= 1
		case ecUpvalue:
			code.AddABC(OP_SETUPVAL, reg, context.Upvalues.RegisterUnique(ex.(*ast.IdentExpr).Value), 0, sline(ex))
			reg -= 1
		case ecTable:
			vreg, vk := opIndexOrReg(acs[i].valuerk), 0
			if opIsK(acs[i].valuerk) {
				vk = 1
			}
			switch acs[i].keyMode {
			case keyKStr:
				code.AddABCk(OP_SETFIELD, acs[i].ec.reg, acs[i].keyData, vreg, vk, sline(ex))
			case keyInt:
				code.AddABCk(OP_SETI, acs[i].ec.reg, acs[i].keyData, vreg, vk, sline(ex))
			default:
				code.AddABCk(OP_SETTABLE, acs[i].ec.reg, acs[i].keyReg, vreg, vk, sline(ex))
			}
			if !opIsK(acs[i].valuerk) {
				reg -= 1
			}
		}
	}
} // }}}

func compileRegAssignment(context *funcContext, names []string, exprs []ast.Expr, reg int, nvars int, line int) { // {{{
	lennames := len(names)
	lenexprs := len(exprs)
	namesassigned := 0
	ec := &expcontext{}

	for namesassigned < lennames && namesassigned < lenexprs {
		if isVarArgReturnExpr(exprs[namesassigned]) && (lenexprs-namesassigned-1) <= 0 {

			varargopt := nvars - namesassigned
			ecupdate(ec, ecVararg, reg, varargopt-1)
			compileExpr(context, reg, exprs[namesassigned], ec)
			reg += varargopt
			namesassigned = lennames
		} else {
			ecupdate(ec, ecLocal, reg, 0)
			compileExpr(context, reg, exprs[namesassigned], ec)
			reg += 1
			namesassigned += 1
		}
	}

	// extra left names
	if lennames > namesassigned {
		restleft := lennames - namesassigned - 1
		context.Code.AddABC(OP_LOADNIL, reg, restleft, 0, line)
		reg += restleft
	}

	// extra right exprs
	for i := namesassigned; i < lenexprs; i++ {
		varargopt := -1
		if i != lenexprs-1 {
			varargopt = 0
		}
		ecupdate(ec, ecNone, reg, varargopt)
		reg += compileExpr(context, reg, exprs[i], ecnone(varargopt))
	}
} // }}}

func compileLocalAssignStmt(context *funcContext, stmt *ast.LocalAssignStmt) { // {{{
	reg := context.RegTop()
	if len(stmt.Names) == 1 && len(stmt.Exprs) == 1 {
		if _, ok := stmt.Exprs[0].(*ast.FunctionExpr); ok {
			context.RegisterLocalVarKind(stmt.Names[0], localStmtAttrib(stmt, 0))
			compileRegAssignment(context, stmt.Names, stmt.Exprs, reg, len(stmt.Names), sline(stmt))
			compileStmtTBC(context, stmt)
			return
		}
	}

	compileRegAssignment(context, stmt.Names, stmt.Exprs, reg, len(stmt.Names), sline(stmt))
	for i, name := range stmt.Names {
		context.RegisterLocalVarKind(name, localStmtAttrib(stmt, i))
	}
	compileStmtTBC(context, stmt)
}

// localStmtAttrib is stmt.Attribs[i] as a variable kind ("" names —
// plain locals — keep varKindReg).
func localStmtAttrib(stmt *ast.LocalAssignStmt, i int) int {
	if stmt.Attribs == nil || i >= len(stmt.Attribs) {
		return varKindReg
	}
	return attribToKind(stmt.Attribs[i])
}

// compileStmtTBC registers a statement's <close> local (at most one — the
// parser enforces it) AFTER its initializers evaluated (stock localstat's
// checktoclose tail): OP_TBC A = the variable's register; the owning
// block gains RefUpvalue so block-exit/break/goto CLOSE fires like a
// captured local's would, and the function is marked NeedsClose for the
// RETURN k bit.
func compileStmtTBC(context *funcContext, stmt *ast.LocalAssignStmt) {
	for i, name := range stmt.Names {
		if localStmtAttrib(stmt, i) != varKindTBC {
			continue
		}
		reg := context.FindLocalVar(name)
		context.Code.AddABC(OP_TBC, reg, 0, 0, sline(stmt))
		context.Block.RefUpvalue = true
		context.NeedsClose = true
		context.tbcActive++
	}
} // }}}

func compileReturnStmt(context *funcContext, stmt *ast.ReturnStmt) { // {{{
	lenexprs := len(stmt.Exprs)
	code := context.Code
	reg := context.RegTop()
	a := reg
	// U4 (stock luaK_finish): a function marked for closing signals it on
	// its returns — RETURN/TAILCALL get the k bit; the simple RETURN0/1
	// become RETURN (the only return opcode that closes). B keeps the
	// fork's fixed-results convention (B = nres+1), so the rewritten
	// RETURN0/1 forms carry B=1/B=2.
	kc := 0
	if context.NeedsClose {
		kc = 1
	}

	if lenexprs == 0 {
		if kc != 0 {
			code.AddABCk(OP_RETURN, 0, 1, 0, 1, sline(stmt))
		} else {
			code.AddABC(OP_RETURN0, 0, 0, 0, sline(stmt))
		}
		return
	}
	if lenexprs == 1 {
		switch ex := stmt.Exprs[0].(type) {
		case *ast.IdentExpr:
			if idx := context.FindLocalVar(ex.Value); idx > -1 {
				if kc != 0 {
					code.AddABCk(OP_RETURN, idx, 2, 0, 1, sline(stmt))
				} else {
					code.AddABC(OP_RETURN1, idx, 0, 0, sline(stmt))
				}
				return
			}
		case *ast.FuncCallExpr:
			if ex.AdjustRet { // return (func())
				reg += compileExpr(context, reg, ex, ecnone(0))
				code.AddABCk(OP_RETURN, a, 0, 0, kc, sline(stmt))
			} else {
				reg += compileExpr(context, reg, ex, ecnone(-2))
				if context.tbcActive == 0 { // stock insidetbc: no tailcall over an open <close>
					code.SetOpCode(code.LastPC(), OP_TAILCALL)
				}
				code.AddABCk(OP_RETURN, a, 0, 0, kc, sline(stmt))
			}
			return
		}
	}

	lastisvaarg := false
	for i, expr := range stmt.Exprs {
		if i == lenexprs-1 && isVarArgReturnExpr(expr) {
			compileExpr(context, reg, expr, ecnone(-2))
			lastisvaarg = true
		} else {
			reg += compileExpr(context, reg, expr, ecnone(0))
		}
	}
	count := reg - a + 1
	if lastisvaarg {
		count = 0
	}
	context.Code.AddABCk(OP_RETURN, a, count, 0, kc, sline(stmt))
} // }}}

func compileIfStmt(context *funcContext, stmt *ast.IfStmt) { // {{{
	thenlabel := context.NewLabel()
	elselabel := context.NewLabel()
	endlabel := context.NewLabel()

	compileBranchCondition(context, context.RegTop(), stmt.Condition, thenlabel, elselabel, false)
	context.SetLabelPc(thenlabel, context.Code.LastPC())
	compileBlock(context, stmt.Then)
	if len(stmt.Else) > 0 {
		context.Code.AddSJ(OP_JMP, endlabel, sline(stmt))
	}
	context.SetLabelPc(elselabel, context.Code.LastPC())
	if len(stmt.Else) > 0 {
		compileBlock(context, stmt.Else)
		context.SetLabelPc(endlabel, context.Code.LastPC())
	}

} // }}}

func compileBranchCondition(context *funcContext, reg int, expr ast.Expr, thenlabel, elselabel int, hasnextcond bool) { // {{{
	code := context.Code
	flip := 0
	jumplabel := elselabel
	if hasnextcond {
		flip = 1
		jumplabel = thenlabel
	}

	switch ex := expr.(type) {
	case *ast.FalseExpr, *ast.NilExpr:
		if !hasnextcond {
			code.AddSJ(OP_JMP, elselabel, sline(expr))
			return
		}
	case *ast.TrueExpr, *ast.NumberExpr, *ast.StringExpr:
		if !hasnextcond {
			return
		}
	case *ast.UnaryNotOpExpr:
		compileBranchCondition(context, reg, ex.Expr, elselabel, thenlabel, !hasnextcond)
		return
	case *ast.LogicalOpExpr:
		switch ex.Operator {
		case "and":
			nextcondlabel := context.NewLabel()
			compileBranchCondition(context, reg, ex.Lhs, nextcondlabel, elselabel, false)
			context.SetLabelPc(nextcondlabel, context.Code.LastPC())
			compileBranchCondition(context, reg, ex.Rhs, thenlabel, elselabel, hasnextcond)
		case "or":
			nextcondlabel := context.NewLabel()
			compileBranchCondition(context, reg, ex.Lhs, thenlabel, nextcondlabel, true)
			context.SetLabelPc(nextcondlabel, context.Code.LastPC())
			compileBranchCondition(context, reg, ex.Rhs, thenlabel, elselabel, hasnextcond)
		}
		return
	case *ast.RelationalOpExpr:
		compileRelationalOpExprAux(context, reg, ex, flip, jumplabel)
		return
	}

	a := reg
	compileExprWithMVPropagation(context, expr, &reg, &a)
	code.AddABCk(OP_TEST, a, 0, 0, flip, sline(expr))
	code.AddSJ(OP_JMP, jumplabel, sline(expr))
} // }}}

func compileWhileStmt(context *funcContext, stmt *ast.WhileStmt) { // {{{
	thenlabel := context.NewLabel()
	elselabel := context.NewLabel()
	condlabel := context.NewLabel()

	context.SetLabelPc(condlabel, context.Code.LastPC())
	compileBranchCondition(context, context.RegTop(), stmt.Condition, thenlabel, elselabel, false)
	context.SetLabelPc(thenlabel, context.Code.LastPC())
	context.EnterBlock(elselabel, stmt)
	compileChunk(context, stmt.Stmts, false)
	context.CloseUpvalues()
	context.Code.AddSJ(OP_JMP, condlabel, eline(stmt))
	context.LeaveBlock()
	context.SetLabelPc(elselabel, context.Code.LastPC())
} // }}}

func compileRepeatStmt(context *funcContext, stmt *ast.RepeatStmt) { // {{{
	initlabel := context.NewLabel()
	thenlabel := context.NewLabel()
	elselabel := context.NewLabel()

	context.SetLabelPc(initlabel, context.Code.LastPC())
	context.SetLabelPc(elselabel, context.Code.LastPC())
	context.EnterBlock(thenlabel, stmt)
	compileChunk(context, stmt.Stmts, true)
	compileBranchCondition(context, context.RegTop(), stmt.Condition, thenlabel, elselabel, false)

	context.SetLabelPc(thenlabel, context.Code.LastPC())
	n := context.LeaveBlock()

	if n > -1 {
		label := context.NewLabel()
		context.Code.AddSJ(OP_JMP, label, eline(stmt))
		context.SetLabelPc(elselabel, context.Code.LastPC())
		context.Code.AddABC(OP_CLOSE, n, 0, 0, eline(stmt))
		context.Code.AddSJ(OP_JMP, initlabel, eline(stmt))
		context.SetLabelPc(label, context.Code.LastPC())
	}

} // }}}

// compileBreakStmt lowers break as a jump to the innermost loop's end
// label. 5.2 allows break mid-block, so the jump may exit the scope of
// locals in ANY block between the current one and the loop's (inclusive);
// OP_CLOSE at the outermost such boundary closes every open upvalue above
// it (one instruction, register-range semantics). The pre-5.2 code only
// considered the loop block itself and missed nested do/local scopes
// (e.g. `while true do do local t = {}; local f = function() return t
// end; break end end` left t's upvalue open across loop iterations).
func compileBreakStmt(context *funcContext, stmt *ast.BreakStmt) { // {{{
	closeIdx := -1
	for block := context.Block; block != nil; block = block.Parent {
		if block.RefUpvalue && block.Parent != nil {
			// keep overwriting: the OUTERMOST crossed block with captured
			// locals owns the close register
			closeIdx = block.Parent.LocalVars.LastIndex()
		}
		if label := block.BreakLabel; label != labelNoJump {
			if closeIdx > -1 {
				context.Code.AddABC(OP_CLOSE, closeIdx, 0, 0, sline(stmt))
			}
			context.Code.AddSJ(OP_JMP, label, sline(stmt))
			return
		}
	}
	raiseCompileError(context, sline(stmt), "no loop to break")
} // }}}

func compileFuncDefStmt(context *funcContext, stmt *ast.FuncDefStmt) { // {{{
	if stmt.Name.Func == nil {
		reg := context.RegTop()
		var treg int
		compileExprWithKMVPropagation(context, stmt.Name.Receiver, &reg, &treg)
		compileExpr(context, reg, stmt.Func, ecfuncdef)
		kidx, fits := context.kStringIndex(stmt.Name.Method, sline(stmt.Name.Receiver))
		if fits {
			context.Code.AddABC(OP_SETFIELD, treg, kidx, reg, sline(stmt.Name.Receiver))
		} else {
			// K-overflow fallback: key into a register, SETTABLE
			loadStringK(context, reg+1, stmt.Name.Method, sline(stmt.Name.Receiver))
			context.Code.AddABCk(OP_SETTABLE, treg, reg+1, reg, 0, sline(stmt.Name.Receiver))
		}
	} else {
		astmt := &ast.AssignStmt{Lhs: []ast.Expr{stmt.Name.Func}, Rhs: []ast.Expr{stmt.Func}}
		astmt.SetLine(sline(stmt.Func))
		astmt.SetLastLine(eline(stmt.Func))
		compileAssignStmt(context, astmt)
	}
} // }}}

// compileNumberForStmt — stock 5.5 flow (lparser.c fornum + lvm.c
// forprep/FORLOOP): three registers, r0=count (hidden), r1=step (hidden),
// r2=control variable (the visible name). FORPREP falls through into the
// body or skips past the FORLOOP; FORLOOP steps and jumps back while the
// counter (iterations-1) is nonzero — the count never wraps, closing the
// row-57 saturation edge.
func compileNumberForStmt(context *funcContext, stmt *ast.NumberForStmt) { // {{{
	code := context.Code
	endlabel := context.NewLabel()
	ec := &expcontext{}

	context.EnterBlock(endlabel, stmt)
	// register/compile interleaving keeps each operand's temporaries
	// starting exactly at its own target register (r0=count slot,
	// r1=step slot, r2=control slot = the visible name)
	reg := context.RegTop()
	rfor := context.RegisterLocalVar("(for index)")
	ecupdate(ec, ecLocal, rfor, 0)
	compileExpr(context, reg, stmt.Init, ec)

	reg = context.RegTop()
	context.RegisterLocalVar("(for limit)")
	ecupdate(ec, ecLocal, rfor+1, 0)
	compileExpr(context, reg, stmt.Limit, ec)

	if stmt.Step == nil {
		stmt.Step = &ast.NumberExpr{Value: "1"}
		stmt.Step.SetLine(sline(stmt.Init))
	}
	reg = context.RegTop()
	ecupdate(ec, ecLocal, rfor+2, 0)
	compileExpr(context, reg, stmt.Step, ec)

	// U4: the control variable is RDKCONST (stock lparser.c) — the loop
	// steps it internally, user code cannot assign it.
	context.RegisterLocalVarKind(stmt.Name, varKindConst)

	code.AddABx(OP_FORPREP, rfor, 0, sline(stmt))
	preppc := code.LastPC()
	bodypc := code.LastPC() + 1
	compileChunk(context, stmt.Stmts, false)

	context.LeaveBlock()

	flpc := code.LastPC() + 1 // FORLOOP's own index (about to be emitted)
	code.AddABx(OP_FORLOOP, rfor, flpc+1-bodypc, sline(stmt))
	context.SetLabelPc(endlabel, code.LastPC())
	if d := flpc - preppc - 1; d < 0 || d > opMaxArgBx {
		raiseCompileError(context, sline(stmt), "control structure too long")
	} else {
		code.SetBx(preppc, d)
	}
} // }}}

// compileGenericForStmt — the 5.5 four-slot layout: r0=iterator function,
// r1=state, r2=closing variable (TFORPREP swaps it with the control),
// r3=control variable (aliased with the first loop name). The closing
// value's to-be-closed registration is deferred to U5; TFORPREP performs
// only the swap (ledger row).
func compileGenericForStmt(context *funcContext, stmt *ast.GenericForStmt) { // {{{
	code := context.Code
	endlabel := context.NewLabel()
	nnames := len(stmt.Names)

	context.EnterBlock(endlabel, stmt)
	rgen := context.RegisterLocalVar("(for generator)")
	context.RegisterLocalVar("(for state)")
	context.RegisterLocalVar("(for control)")

	compileRegAssignment(context,
		[]string{"(for state)", "(for state)", "(for state)", "(for state)"},
		stmt.Exprs, context.RegTop()-3, 4, sline(stmt))

	code.AddABx(OP_TFORPREP, rgen, 0, sline(stmt))
	preppc := code.LastPC()
	bodypc := code.LastPC() + 1

	// U4: the loop names are RDKCONST (stock lparser.c)
	for _, name := range stmt.Names {
		context.RegisterLocalVarKind(name, varKindConst)
	}
	compileChunk(context, stmt.Stmts, false)

	context.LeaveBlock()

	tfcallpc := code.LastPC() + 1
	code.AddABC(OP_TFORCALL, rgen, 0, nnames, sline(stmt))
	tflpc := code.LastPC() + 1
	code.AddABx(OP_TFORLOOP, rgen, tflpc+1-bodypc, sline(stmt))
	context.SetLabelPc(endlabel, code.LastPC())
	if d := tfcallpc - preppc - 1; d < 0 || d > opMaxArgBx {
		raiseCompileError(context, sline(stmt), "control structure too long")
	} else {
		code.SetBx(preppc, d)
	}
} // }}}

// compileLabelStmt (stock labelstat: checkrepeated + createlabel). A
// label is only visible while its block is open; duplicates are checked
// against every ACTIVE label of the function (nested-block repeats are
// errors, sibling-block repeats are not — the retired label is gone by
// then). A last-statement label sits outside the block's locals.
func compileLabelStmt(context *funcContext, stmt *ast.LabelStmt, isLastStmt bool) { // {{{
	if old := context.findActiveLabel(stmt.Name, 0); old != nil {
		raiseCompileError(context, sline(stmt), "label '%s' already defined on line %d", stmt.Name, old.Line)
	}
	label := newLabelDesc(context.NewLabel(), stmt.Name, context.Code.LastPC(), sline(stmt), context.BlockLocalVarsCount())
	if isLastStmt {
		label.NumActiveLocalVars = context.Block.entryNactvar
	}
	context.labels = append(context.labels, label)
	context.SetLabelPc(label.Id, label.Pc)
} // }}}

// compileGotoStmt (stock gotostat + newgotoentry): a jump plus a DEAD
// CLOSE placeholder after it (B=1 marks it dead). Resolution happens at
// block exit (solveGotos/closeGoto); if the jump turns out to need a
// close, the pair swaps so the CLOSE runs before the jump. The goto never
// searches backward for a label at creation — backward and forward gotos
// resolve identically at leaveblock.
func compileGotoStmt(context *funcContext, stmt *ast.GotoStmt) { // {{{
	context.Code.AddSJ(OP_JMP, labelNoJump, sline(stmt))
	context.Code.AddABC(OP_CLOSE, 0, 1, 0, sline(stmt)) // dead placeholder
	label := newLabelDesc(-1, stmt.Label, context.Code.LastPC()-1, sline(stmt), context.BlockLocalVarsCount())
	context.gotos = append(context.gotos, label)
} // }}}

func compileExpr(context *funcContext, reg int, expr ast.Expr, ec *expcontext) int { // {{{
	code := context.Code
	sreg := savereg(ec, reg)
	sused := 1
	if sreg < reg {
		sused = 0
	}

	switch ex := expr.(type) {
	case *ast.StringExpr:
		code.AddABx(OP_LOADK, sreg, context.ConstIndex(LString(ex.Value)), sline(ex))
		return sused
	case *ast.NumberExpr:
		// integer literals keep their syntactic subtype (U2/D1):
		// parseNumber yields LInteger or LNumber, loadNumber picks
		// LOADI/LOADF/LOADK per stock luaK_int/luaK_float
		num, err := parseNumber(ex.Value)
		if err != nil {
			num = LNumber(math.NaN())
		}
		loadNumber(context, sreg, num, sline(ex))
		return sused
	case *constLValueExpr:
		loadNumber(context, sreg, ex.Value, sline(ex))
		return sused
	case *ast.NilExpr:
		code.AddABC(OP_LOADNIL, sreg, 0, 0, sline(ex))
		return sused
	case *ast.FalseExpr:
		code.AddABC(OP_LOADFALSE, sreg, 0, 0, sline(ex))
		return sused
	case *ast.TrueExpr:
		code.AddABC(OP_LOADTRUE, sreg, 0, 0, sline(ex))
		return sused
	case *ast.IdentExpr:
		switch getIdentRefType(context, context, ex) {
		case ecGlobal:
			// D3: a global reads a field of _ENV. With _ENV shadowed by
			// a local, that is a plain keyed table read; otherwise it is
			// a field of the frame function's _ENV upvalue cell
			// (GETTABUP — same shape as stock). A key past the 8-bit K
			// range takes stock's fallback: key (and the upvalue table)
			// into registers, GETTABLE.
			kidx, fits := context.kStringIndex(ex.Value, sline(ex))
			if envlocal, _ := envAccess(context); envlocal > -1 {
				if fits {
					code.AddABC(OP_GETFIELD, sreg, envlocal, kidx, sline(ex))
				} else {
					loadStringK(context, reg, ex.Value, sline(ex))
					code.AddABC(OP_GETTABLE, sreg, envlocal, reg, sline(ex))
				}
			} else if fits {
				code.AddABC(OP_GETTABUP, sreg, envUpvalueIndexOf(context), kidx, sline(ex))
			} else {
				envreg := reg
				code.AddABC(OP_GETUPVAL, envreg, envUpvalueIndexOf(context), 0, sline(ex))
				loadStringK(context, envreg+1, ex.Value, sline(ex))
				code.AddABC(OP_GETTABLE, sreg, envreg, envreg+1, sline(ex))
			}
		case ecUpvalue:
			code.AddABC(OP_GETUPVAL, sreg, context.Upvalues.RegisterUnique(ex.Value), 0, sline(ex))
		case ecLocal:
			b := context.FindLocalVar(ex.Value)
			code.AddABC(OP_MOVE, sreg, b, 0, sline(ex))
		}
		return sused
	case *ast.Comma3Expr:
		if context.Proto.IsVarArg == 0 {
			raiseCompileError(context, sline(ex), "cannot use '...' outside a vararg function")
		}
		context.Proto.IsVarArg &= ^VarArgNeedsArg
		code.AddABCk(OP_VARARG, sreg, 0, 2+ec.varargopt, 0, sline(ex))
		if context.RegTop() > (sreg+2+ec.varargopt) || ec.varargopt < -1 {
			return 0
		}
		return (sreg + 1 + ec.varargopt) - reg
	case *ast.AttrGetExpr:
		a := sreg
		b := reg
		compileExprWithMVPropagation(context, ex.Object, &reg, &b)
		if str, ok := ex.Key.(*ast.StringExpr); ok {
			kidx, fits := context.kStringIndex(str.Value, sline(ex))
			if fits {
				code.AddABC(OP_GETFIELD, a, b, kidx, sline(ex))
			} else {
				// K-overflow fallback (stock isKstr miss): key into a
				// register, GETTABLE
				loadStringK(context, reg, str.Value, sline(ex))
				code.AddABC(OP_GETTABLE, a, b, reg, sline(ex))
			}
		} else if idx, ok := intKeyLiteral(ex.Key); ok {
			code.AddABC(OP_GETI, a, b, idx, sline(ex))
		} else {
			c := reg
			reg += compileExpr(context, reg, ex.Key, ecnone(0))
			code.AddABC(OP_GETTABLE, a, b, c, sline(ex))
		}
		return sused
	case *ast.TableExpr:
		compileTableExpr(context, reg, ex, ec)
		return 1
	case *ast.ArithmeticOpExpr:
		compileArithmeticOpExpr(context, reg, ex, ec)
		return sused
	case *ast.StringConcatOpExpr:
		compileStringConcatOpExpr(context, reg, ex, ec)
		return sused
	case *ast.UnaryMinusOpExpr, *ast.UnaryNotOpExpr, *ast.UnaryLenOpExpr, *ast.UnaryBNotOpExpr:
		compileUnaryOpExpr(context, reg, ex, ec)
		return sused
	case *ast.RelationalOpExpr:
		compileRelationalOpExpr(context, reg, ex, ec)
		return sused
	case *ast.LogicalOpExpr:
		compileLogicalOpExpr(context, reg, ex, ec)
		return sused
	case *ast.FuncCallExpr:
		return compileFuncCallExpr(context, reg, ex, ec)
	case *ast.FunctionExpr:
		childcontext := newFuncContext(context.Proto.SourceName, context)
		compileFunctionExpr(childcontext, ex, ec)
		protono := len(context.Proto.FunctionPrototypes)
		context.Proto.FunctionPrototypes = append(context.Proto.FunctionPrototypes, childcontext.Proto)
		code.AddABx(OP_CLOSURE, sreg, protono, sline(ex))
		// capture descriptors live in the child proto (stock Upvaldesc);
		// marking the defining block's RefUpvalue drives OP_CLOSE
		// emission as the old pseudo-instructions did
		for _, upvalue := range childcontext.Upvalues.List() {
			localidx, block := context.FindLocalVarAndBlock(upvalue.Name)
			if localidx > -1 {
				childcontext.Proto.Upvalues = append(childcontext.Proto.Upvalues,
					Upvaldesc{InStack: true, Idx: uint8(localidx), Name: upvalue.Name})
				block.RefUpvalue = true
				// stock markupval sets fs->needclose alongside bl->upval
				context.NeedsClose = true
			} else {
				upvalueidx := context.Upvalues.Find(upvalue.Name)
				if upvalueidx < 0 {
					upvalueidx = context.Upvalues.RegisterUnique(upvalue.Name)
				}
				childcontext.Proto.Upvalues = append(childcontext.Proto.Upvalues,
					Upvaldesc{InStack: false, Idx: uint8(upvalueidx), Name: upvalue.Name})
			}
		}
		return sused
	default:
		panic(fmt.Sprintf("expr %v not implemented.", reflect.TypeOf(ex).Elem().Name()))
	}

} // }}}

func compileExprWithPropagation(context *funcContext, expr ast.Expr, reg *int, save *int, propergator func(int, *int, *int, int)) { // {{{
	reginc := compileExpr(context, *reg, expr, ecnone(0))
	if _, ok := expr.(*ast.LogicalOpExpr); ok {
		*save = *reg
		*reg = *reg + reginc
	} else {
		propergator(context.RegTop(), save, reg, reginc)
	}
} // }}}

func compileExprWithKMVPropagation(context *funcContext, expr ast.Expr, reg *int, save *int) { // {{{
	compileExprWithPropagation(context, expr, reg, save, context.Code.PropagateKMV)
} // }}}

func compileExprWithMVPropagation(context *funcContext, expr ast.Expr, reg *int, save *int) { // {{{
	compileExprWithPropagation(context, expr, reg, save, context.Code.PropagateMV)
} // }}}

// constFold folds arithmetic on numerals, porting stock lcode.c
// constfolding/validop (U2b): operands keep their syntactic subtype, int
// ops fold with 5.3 wrap-around semantics, ops that would error are left
// for runtime (division by zero, non-integral bitwise operands), and
// float results equal to 0 or NaN are never folded (avoids -0.0/NaN in
// the constant pool).
func constFold(exp ast.Expr) ast.Expr { // {{{
	switch expr := exp.(type) {
	case *ast.ArithmeticOpExpr:
		lvalue, lisconst := constNumberValue(constFold(expr.Lhs))
		rvalue, risconst := constNumberValue(constFold(expr.Rhs))
		if lisconst && risconst {
			op := arithOpForOperator(expr.Operator)
			if !foldValidOp(op, lvalue, rvalue) {
				return expr
			}
			if v, _, res := arithCompute(op, lvalue, rvalue); res == arithDone {
				if f, isFloat := v.(LNumber); isFloat && (f != f || f == 0) {
					return expr
				}
				return &constLValueExpr{Value: v}
			}
		}
		return expr
	case *ast.UnaryMinusOpExpr:
		expr.Expr = constFold(expr.Expr)
		if value, ok := constNumberValue(expr.Expr); ok {
			if iv, isInt := value.(LInteger); isInt {
				return &constLValueExpr{Value: -iv}
			}
			if fv := value.(LNumber); fv != 0 && fv == fv {
				return &constLValueExpr{Value: -fv}
			}
		}
		return expr
	case *ast.UnaryBNotOpExpr:
		expr.Expr = constFold(expr.Expr)
		if value, ok := constNumberValue(expr.Expr); ok {
			// validop's rule: both operands must convert to integers
			if iv, isInt := value.(LInteger); isInt {
				return &constLValueExpr{Value: ^iv}
			}
			if iv, ok := toIntBitwise(value); ok {
				return &constLValueExpr{Value: ^LInteger(iv)}
			}
		}
		return expr
	default:

		return exp
	}
} // }}}

// constNumberValue extracts the numeral value (subtype preserved) of a
// constant expression.
func constNumberValue(expr ast.Expr) (LValue, bool) {
	if ex, ok := expr.(*ast.NumberExpr); ok {
		lv, err := parseNumber(ex.Value)
		if err != nil {
			return LNumber(math.NaN()), true
		}
		return lv, true
	} else if ex, ok := expr.(*constLValueExpr); ok {
		switch ex.Value.(type) {
		case LNumber, LInteger:
			return ex.Value, true
		}
	}
	return nil, false
}

// arithOpForOperator maps the AST operator string to its opcode.
func arithOpForOperator(opr string) int {
	switch opr {
	case "+":
		return OP_ADD
	case "-":
		return OP_SUB
	case "*":
		return OP_MUL
	case "/":
		return OP_DIV
	case "%":
		return OP_MOD
	case "^":
		return OP_POW
	case "//":
		return OP_IDIV
	case "&":
		return OP_BAND
	case "|":
		return OP_BOR
	case "~":
		return OP_BXOR
	case "<<":
		return OP_SHL
	case ">>":
		return OP_SHR
	}
	panic(fmt.Sprintf("unknown binop: %v", opr))
}

// foldValidOp ports stock lcode.c validop: the folding guard for
// error-prone operations (bitwise needs integer-convertible operands;
// the divisions need a nonzero divisor).
func foldValidOp(op int, v1, v2 LValue) bool {
	switch op {
	case OP_BAND, OP_BOR, OP_BXOR, OP_SHL, OP_SHR, OP_BNOT:
		_, ok1 := toIntBitwise(v1)
		_, ok2 := toIntBitwise(v2)
		return ok1 && ok2
	case OP_DIV, OP_IDIV, OP_MOD:
		return LVAsNumber(v2) != 0
	}
	return true
}

func compileFunctionExpr(context *funcContext, funcexpr *ast.FunctionExpr, ec *expcontext) { // {{{
	context.Proto.LineDefined = sline(funcexpr)
	context.Proto.LastLineDefined = eline(funcexpr)
	if len(funcexpr.ParList.Names) > maxRegisters {
		raiseCompileError(context, context.Proto.LineDefined, "register overflow")
	}
	context.Proto.NumParameters = uint8(len(funcexpr.ParList.Names))
	if ec.ctype == ecMethod {
		context.Proto.NumParameters += 1
		context.RegisterLocalVar("self")
	}
	for _, name := range funcexpr.ParList.Names {
		context.RegisterLocalVar(name)
	}
	if funcexpr.ParList.HasVargs {
		if CompatVarArg {
			context.Proto.IsVarArg = VarArgHasArg | VarArgNeedsArg
			if context.Parent != nil {
				context.RegisterLocalVar("arg")
			}
		}
		context.Proto.IsVarArg |= VarArgIsVarArg
	}
	// VARARGPREP opens every vararg proto (stock's first instruction).
	// The fork's frame shuffle stays at call time — this is a marker the
	// interpreter no-ops (the 5.4+ vararg model is U5; ledger row).
	if context.Proto.IsVarArg != 0 {
		context.Code.AddABC(OP_VARARGPREP, int(context.Proto.NumParameters), 0, 0, context.Proto.LineDefined)
	}
	// the root block's entry level: parameters (+ compat 'arg') only
	context.Block.entryNactvar = context.BlockLocalVarsCount()

	compileChunk(context, funcexpr.Stmts, false)

	context.Code.AddABC(OP_RETURN0, 0, 0, 0, eline(funcexpr))
	context.EndScope()
	context.finishFunctionGotos()
	context.Proto.Code = context.Code.List()
	context.Proto.DbgSourcePositions = context.Code.PosList()
	context.Proto.DbgUpvalues = context.Upvalues.Names()
	context.Proto.NumUpvalues = uint8(len(context.Proto.DbgUpvalues))
	// D3: record where _ENV lives for the loader seeding (the main
	// chunk's _ENV cell is seeded at load; -1 = no globals)
	context.Proto.EnvUpvalueIndex = int8(context.Upvalues.Find(envUpvalueName))
	// the main chunk has no parent to resolve captures: its only
	// upvalue is _ENV (loader-seeded, dummy descriptor)
	if context.Parent == nil {
		for range context.Upvalues.Names() {
			context.Proto.Upvalues = append(context.Proto.Upvalues, Upvaldesc{InStack: false, Idx: 0, Name: envUpvalueName})
		}
	}
	for _, clv := range context.Proto.Constants {
		sv := ""
		if slv, ok := clv.(LString); ok {
			sv = string(slv)
		}
		context.Proto.stringConstants = append(context.Proto.stringConstants, sv)
	}
	patchCode(context)
} // }}}

func compileTableExpr(context *funcContext, reg int, ex *ast.TableExpr, ec *expcontext) { // {{{
	code := context.Code
	tablereg := reg
	reg++
	code.AddvABCk(OP_NEWTABLE, tablereg, 0, 0, 0, sline(ex))
	tablepc := code.LastPC()
	// NEWTABLE's size operands are patched after the constructor is
	// known; EXTRAARG always follows (stock's shape)
	code.AddAx(OP_EXTRAARG, 0, sline(ex))
	regbase := reg

	arraycount := 0 // array items evaluated so far
	stored := 0     // array items already SETLISTed into the table
	lastvararg := false
	for i, field := range ex.Fields {
		islast := i == len(ex.Fields)-1
		if field.Key == nil {
			if islast && isVarArgReturnExpr(field.Value) {
				reg += compileExpr(context, reg, field.Value, ecnone(-2))
				lastvararg = true
			} else {
				reg += compileExpr(context, reg, field.Value, ecnone(0))
				arraycount += 1
			}
		} else {
			regorg := reg
			line := sline(ex)
			var vreg int
			if str, ok := field.Key.(*ast.StringExpr); ok {
				kidx, fits := context.kStringIndex(str.Value, line)
				compileExprWithKMVPropagation(context, field.Value, &reg, &vreg)
				vk := boolInt(opIsK(vreg))
				if fits {
					code.AddABCk(OP_SETFIELD, tablereg, kidx, opIndexOrReg(vreg), vk, line)
				} else {
					// K-overflow fallback: key into a register, SETTABLE
					loadStringK(context, reg, str.Value, line)
					reg++
					code.AddABCk(OP_SETTABLE, tablereg, reg-1, opIndexOrReg(vreg), vk, line)
				}
			} else {
				b := reg
				reg += compileExpr(context, reg, field.Key, ecnone(0))
				compileExprWithKMVPropagation(context, field.Value, &reg, &vreg)
				vk := boolInt(opIsK(vreg))
				code.AddABCk(OP_SETTABLE, tablereg, b, opIndexOrReg(vreg), vk, line)
			}
			reg = regorg
		}
		flush := arraycount % FieldsPerFlush
		if (arraycount != 0 && (flush == 0 || islast)) || lastvararg {
			reg = regbase
			n := arraycount - stored // this batch (fits vB: flush < 64)
			if lastvararg {
				n = 0 // multret: store up to top
			}
			line := field.Value
			if field.Key != nil {
				line = field.Key
			}
			if stored > opMaxArgsvC {
				code.AddvABCk(OP_SETLIST, tablereg, n, stored%(opMaxArgsvC+1), 1, sline(line))
				code.AddAx(OP_EXTRAARG, stored/(opMaxArgsvC+1), sline(line))
			} else {
				code.AddvABCk(OP_SETLIST, tablereg, n, stored, 0, sline(line))
			}
			stored = arraycount
		}
	}
	// patch NEWTABLE's size hints (stock luaK_settablesize): vB is
	// ceillog2(hash size)+1, the array size splits over vC/EXTRAARG
	hashsize := len(ex.Fields) - arraycount
	vb := 0
	if hashsize > 0 {
		vb = ceilLog2(hashsize) + 1
	}
	extra := arraycount / (opMaxArgsvC + 1)
	code.SetvB(tablepc, vb)
	code.SetvC(tablepc, arraycount%(opMaxArgsvC+1))
	code.Setk(tablepc, boolInt(extra > 0))
	code.SetAx(tablepc+1, extra)
	if shouldmove(ec, tablereg) {
		code.AddABC(OP_MOVE, ec.reg, tablereg, 0, sline(ex))
	}
} // }}}

// ceilLog2 is stock luaO_ceillog2 (capacity hint math only).
func ceilLog2(x int) int {
	n := 0
	x--
	for x > 0 {
		x >>= 1
		n++
	}
	return n
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// emitArith writes a binary arithmetic/bitwise operation with its MMBIN
// fallback slot, porting stock lcode.c's selection (codecommutative /
// codearith / codebitwise / finishbinexpneg / the shift immediates):
// small-integer second operands become sC immediates (ADDI/SHLI/SHRI),
// numeric constants in the 8-bit K range ride the *K opcodes, and
// everything else takes the register form — each followed by the
// MMBIN/MMBINI/MMBINK slot carrying the metamethod event and flip.
func emitArith(context *funcContext, a int, lhs, rhs opnd, op int, reg int, line int) {
	code := context.Code
	tm := arithTM(op)
	flip := 0

	// commutative ops may swap a constant into second position
	if op == OP_ADD || op == OP_MUL || op == OP_BAND || op == OP_BOR || op == OP_BXOR {
		if lhs.kind != opndReg && rhs.kind == opndReg {
			lhs, rhs = rhs, lhs
			flip = 1
		}
	}
	// (imm << r): SHLI's immediate is the FIRST operand (stock OPR_SHL)
	if op == OP_SHL && lhs.kind == opndInt && rhs.kind == opndReg && fitsC(lhs.ival) {
		code.AddABCk(OP_SHLI, a, rhs.reg, int2sC(int(lhs.ival)), 0, line)
		code.AddABCk(OP_MMBINI, rhs.reg, int2sC(int(lhs.ival)), tm, 1, line)
		return
	}
	// a non-register lhs must materialize into a register now
	next := reg
	if lhs.kind != opndReg {
		lhs.reg = next
		materializeReg(context, lhs, next, line)
		next++
	}
	v1 := lhs.reg

	// negate-immediate forms: (r - I) as (r + -I), (r << I) as (r >> -I);
	// the MMBINI slot keeps the ORIGINAL operand for metamethods
	if (op == OP_SUB || op == OP_SHL) && rhs.kind == opndInt && fitsC(rhs.ival) && fitsC(-rhs.ival) {
		if op == OP_SUB {
			code.AddABCk(OP_ADDI, a, v1, int2sC(int(-rhs.ival)), 0, line)
		} else {
			code.AddABCk(OP_SHRI, a, v1, int2sC(int(-rhs.ival)), 0, line)
		}
		code.AddABCk(OP_MMBINI, v1, int2sC(int(rhs.ival)), tm, 0, line)
		return
	}
	// ADDI: immediate addition
	if op == OP_ADD && rhs.kind == opndInt && fitsC(rhs.ival) {
		code.AddABCk(OP_ADDI, a, v1, int2sC(int(rhs.ival)), 0, line)
		code.AddABCk(OP_MMBINI, v1, int2sC(int(rhs.ival)), tm, flip, line)
		return
	}
	// SHRI: immediate right shift
	if op == OP_SHR && rhs.kind == opndInt && fitsC(rhs.ival) {
		code.AddABCk(OP_SHRI, a, v1, int2sC(int(rhs.ival)), 0, line)
		code.AddABCk(OP_MMBINI, v1, int2sC(int(rhs.ival)), tm, 0, line)
		return
	}
	// K-operand forms: ADD..BXOR have K variants — the bitwise ones
	// accept only integer constants (BANDK &c read K[C]:integer), and
	// SHL/SHR have none
	bitwise := op == OP_BAND || op == OP_BOR || op == OP_BXOR
	if rhs.kind != opndReg && op != OP_SHL && op != OP_SHR &&
		(!bitwise || rhs.kind == opndInt) &&
		materializeK(context, rhs) <= opMaxIndexK {
		kidx := materializeK(context, rhs)
		kop := op - OP_ADD + OP_ADDK
		code.AddABCk(kop, a, v1, kidx, 0, line)
		code.AddABCk(OP_MMBINK, v1, kidx, tm, flip, line)
		return
	}
	if rhs.kind != opndReg {
		rhs.reg = next
		materializeReg(context, rhs, next, line)
		next++
	}
	code.AddABCk(op, a, v1, rhs.reg, 0, line)
	code.AddABCk(OP_MMBIN, v1, rhs.reg, tm, 0, line)
}

func compileArithmeticOpExpr(context *funcContext, reg int, expr *ast.ArithmeticOpExpr, ec *expcontext) { // {{{
	exp := constFold(expr)
	if ex, ok := exp.(*constLValueExpr); ok {
		exp.SetLine(sline(expr))
		compileExpr(context, reg, ex, ec)
		return
	}
	expr, _ = exp.(*ast.ArithmeticOpExpr)
	a := savereg(ec, reg)
	lhs, reg2 := compileConstOperand(context, expr.Lhs, reg, true)
	rhs, rnext := compileConstOperand(context, expr.Rhs, reg2, true)
	emitArith(context, a, lhs, rhs, arithOpForOperator(expr.Operator), rnext, sline(expr))
} // }}}

func compileStringConcatOpExpr(context *funcContext, reg int, expr *ast.StringConcatOpExpr, ec *expcontext) { // {{{
	code := context.Code
	crange := 1
	for current := expr.Rhs; current != nil; {
		if ex, ok := current.(*ast.StringConcatOpExpr); ok {
			crange += 1
			current = ex.Rhs
		} else {
			current = nil
		}
	}
	a := savereg(ec, reg)
	basereg := reg
	reg += compileExpr(context, reg, expr.Lhs, ecnone(0))
	reg += compileExpr(context, reg, expr.Rhs, ecnone(0))
	for pc := code.LastPC(); pc != 0 && opGetOpCode(code.At(pc)) == OP_CONCAT; pc-- {
		code.Pop()
	}
	// CONCAT's operands are R[A]..R[A+B-1] (the result overwrites the
	// first operand); a local-target destination gets the result moved
	if a != basereg {
		code.AddABC(OP_CONCAT, basereg, crange+1, 0, sline(expr))
		code.AddABC(OP_MOVE, a, basereg, 0, sline(expr))
	} else {
		code.AddABC(OP_CONCAT, a, crange+1, 0, sline(expr))
	}
} // }}}

func compileUnaryOpExpr(context *funcContext, reg int, expr ast.Expr, ec *expcontext) { // {{{
	opcode := 0
	code := context.Code
	var operandexpr ast.Expr
	switch ex := expr.(type) {
	case *ast.UnaryMinusOpExpr:
		exp := constFold(ex)
		if lvexpr, ok := exp.(*constLValueExpr); ok {
			exp.SetLine(sline(expr))
			compileExpr(context, reg, lvexpr, ec)
			return
		}
		ex, _ = exp.(*ast.UnaryMinusOpExpr)
		operandexpr = ex.Expr
		opcode = OP_UNM
	case *ast.UnaryBNotOpExpr:
		exp := constFold(ex)
		if lvexpr, ok := exp.(*constLValueExpr); ok {
			exp.SetLine(sline(expr))
			compileExpr(context, reg, lvexpr, ec)
			return
		}
		ex, _ = exp.(*ast.UnaryBNotOpExpr)
		operandexpr = ex.Expr
		opcode = OP_BNOT
	case *ast.UnaryNotOpExpr:
		switch ex.Expr.(type) {
		case *ast.TrueExpr:
			code.AddABC(OP_LOADFALSE, savereg(ec, reg), 0, 0, sline(expr))
			return
		case *ast.FalseExpr, *ast.NilExpr:
			code.AddABC(OP_LOADTRUE, savereg(ec, reg), 0, 0, sline(expr))
			return
		default:
			opcode = OP_NOT
			operandexpr = ex.Expr
		}
	case *ast.UnaryLenOpExpr:
		opcode = OP_LEN
		operandexpr = ex.Expr
	}

	a := savereg(ec, reg)
	b := reg
	compileExprWithMVPropagation(context, operandexpr, &reg, &b)
	code.AddABC(opcode, a, b, 0, sline(expr))
} // }}}

// compileRelationalOpExprAux emits a comparison feeding a jump, porting
// stock's codeeq/codeorder selection: register operands with EQ/LT/LE,
// sC immediates with EQI/LTI/LEI/GTI/GEI (C marking a float-sourced
// immediate), pooled constants with EQK. k selects which comparison
// outcome takes the following jump (docondjump: cond == k → jump,
// cond != k → skip it).
func compileRelationalOpExprAux(context *funcContext, reg int, expr *ast.RelationalOpExpr, flip int, label int) { // {{{
	code := context.Code
	lhs, reg2 := compileConstOperand(context, expr.Lhs, reg, false)
	rhs, rnext := compileConstOperand(context, expr.Rhs, reg2, false)
	line := sline(expr)

	// (a > b) <=> (b < a); (a >= b) <=> (b <= a) — swap the operand
	// descriptors and compile the mirrored operator (stock luaK_posfix)
	operator := expr.Operator
	switch operator {
	case ">":
		lhs, rhs = rhs, lhs
		operator = "<"
	case ">=":
		lhs, rhs = rhs, lhs
		operator = "<="
	}

	// operand materializer: the register a (possibly constant) operand
	// lands in, allocating a scratch register when needed
	operandReg := func(o *opnd) int {
		if o.kind == opndReg {
			return o.reg
		}
		o.reg = rnext
		materializeReg(context, *o, rnext, line)
		rnext++
		return o.reg
	}

	switch operator {
	case "==", "~=":
		k := 0
		if operator == "~=" {
			k = 1
		}
		k ^= flip
		// the register operand must come first (stock codeeq swaps a
		// leading constant away; == is commutative)
		if lhs.kind != opndReg && rhs.kind == opndReg {
			lhs, rhs = rhs, lhs
		}
		v1 := operandReg(&lhs)
		switch {
		case rhs.kind == opndInt && fitsC(rhs.ival):
			code.AddABCk(OP_EQI, v1, int2sC(int(rhs.ival)), 0, k, line)
		case rhs.kind == opndFloat && fitsC(rhs.ival):
			code.AddABCk(OP_EQI, v1, int2sC(int(rhs.ival)), 1, k, line)
		case rhs.kind != opndReg && materializeK(context, rhs) <= opMaxIndexK:
			code.AddABCk(OP_EQK, v1, materializeK(context, rhs), 0, k, line)
		default:
			v2 := operandReg(&rhs)
			code.AddABCk(OP_EQ, v1, v2, 0, k, line)
		}
	case "<", "<=":
		// immediate second operand → LTI/LEI; immediate first operand →
		// the mirrored GTI/GEI over the SECOND operand's register
		if imm, isFloat, ok := orderImmediate(rhs); ok && lhs.kind == opndReg {
			code.AddABCk(opLTI(operator), lhs.reg, int2sC(imm), boolInt(isFloat), flip, line)
			code.AddSJ(OP_JMP, label, line)
			return
		}
		if imm, isFloat, ok := orderImmediate(lhs); ok && rhs.kind == opndReg {
			code.AddABCk(opGTI(operator), rhs.reg, int2sC(imm), boolInt(isFloat), flip, line)
			code.AddSJ(OP_JMP, label, line)
			return
		}
		v1 := operandReg(&lhs)
		v2 := operandReg(&rhs)
		code.AddABCk(opLT(operator), v1, v2, 0, flip, line)
	}
	code.AddSJ(OP_JMP, label, line)
} // }}}

// the order-op opcode selectors ("<"→LTI, "<="→LEI, and the GTI/GEI
// mirrors used when the immediate is the first operand)
func opLTI(operator string) int {
	if operator == "<=" {
		return OP_LEI
	}
	return OP_LTI
}

func opGTI(operator string) int {
	if operator == "<=" {
		return OP_GEI
	}
	return OP_GTI
}

func opLT(operator string) int {
	if operator == "<=" {
		return OP_LE
	}
	return OP_LT
}

// orderImmediate extracts the sC immediate of a numeric constant operand
// for the order-comparison family (stock isSCnumber).
func orderImmediate(o opnd) (imm int, isFloat bool, ok bool) {
	switch o.kind {
	case opndInt:
		if fitsC(o.ival) {
			return int(o.ival), false, true
		}
	case opndFloat:
		if fitsC(o.ival) {
			return int(o.ival), true, true
		}
	}
	return 0, false, false
}

func compileRelationalOpExpr(context *funcContext, reg int, expr *ast.RelationalOpExpr, ec *expcontext) { // {{{
	a := savereg(ec, reg)
	code := context.Code
	jumplabel := context.NewLabel()
	compileRelationalOpExprAux(context, reg, expr, 1, jumplabel)
	code.AddABC(OP_LFALSESKIP, a, 0, 0, sline(expr))
	context.SetLabelPc(jumplabel, code.LastPC())
	code.AddABC(OP_LOADTRUE, a, 0, 0, sline(expr))
} // }}}

func compileLogicalOpExpr(context *funcContext, reg int, expr *ast.LogicalOpExpr, ec *expcontext) { // {{{
	a := savereg(ec, reg)
	code := context.Code
	endlabel := context.NewLabel()
	lb := &lblabels{context.NewLabel(), context.NewLabel(), endlabel, false}
	nextcondlabel := context.NewLabel()
	if expr.Operator == "and" {
		compileLogicalOpExprAux(context, reg, expr.Lhs, ec, nextcondlabel, endlabel, false, lb)
		context.SetLabelPc(nextcondlabel, code.LastPC())
		compileLogicalOpExprAux(context, reg, expr.Rhs, ec, endlabel, endlabel, false, lb)
	} else {
		compileLogicalOpExprAux(context, reg, expr.Lhs, ec, endlabel, nextcondlabel, true, lb)
		context.SetLabelPc(nextcondlabel, code.LastPC())
		compileLogicalOpExprAux(context, reg, expr.Rhs, ec, endlabel, endlabel, false, lb)
	}

	if lb.b {
		context.SetLabelPc(lb.f, code.LastPC())
		code.AddABC(OP_LFALSESKIP, a, 0, 0, sline(expr))
		context.SetLabelPc(lb.t, code.LastPC())
		code.AddABC(OP_LOADTRUE, a, 0, 0, sline(expr))
	}

	lastinst := code.Last()
	if opGetOpCode(lastinst) == OP_JMP && opGetArgsJ(lastinst) == endlabel {
		code.Pop()
	}

	context.SetLabelPc(endlabel, code.LastPC())
} // }}}

func compileLogicalOpExprAux(context *funcContext, reg int, expr ast.Expr, ec *expcontext, thenlabel, elselabel int, hasnextcond bool, lb *lblabels) { // {{{
	code := context.Code
	flip := 0
	jumplabel := elselabel
	if hasnextcond {
		flip = 1
		jumplabel = thenlabel
	}

	switch ex := expr.(type) {
	case *ast.FalseExpr:
		if elselabel == lb.e {
			code.AddSJ(OP_JMP, lb.f, sline(expr))
			lb.b = true
		} else {
			code.AddSJ(OP_JMP, elselabel, sline(expr))
		}
		return
	case *ast.NilExpr:
		if elselabel == lb.e {
			compileExpr(context, reg, expr, ec)
			code.AddSJ(OP_JMP, lb.e, sline(expr))
		} else {
			code.AddSJ(OP_JMP, elselabel, sline(expr))
		}
		return
	case *ast.TrueExpr:
		if thenlabel == lb.e {
			code.AddSJ(OP_JMP, lb.t, sline(expr))
			lb.b = true
		} else {
			code.AddSJ(OP_JMP, thenlabel, sline(expr))
		}
		return
	case *ast.NumberExpr, *ast.StringExpr:
		if thenlabel == lb.e {
			compileExpr(context, reg, expr, ec)
			code.AddSJ(OP_JMP, lb.e, sline(expr))
		} else {
			code.AddSJ(OP_JMP, thenlabel, sline(expr))
		}
		return
	case *ast.LogicalOpExpr:
		switch ex.Operator {
		case "and":
			nextcondlabel := context.NewLabel()
			compileLogicalOpExprAux(context, reg, ex.Lhs, ec, nextcondlabel, elselabel, false, lb)
			context.SetLabelPc(nextcondlabel, context.Code.LastPC())
			compileLogicalOpExprAux(context, reg, ex.Rhs, ec, thenlabel, elselabel, hasnextcond, lb)
		case "or":
			nextcondlabel := context.NewLabel()
			compileLogicalOpExprAux(context, reg, ex.Lhs, ec, thenlabel, nextcondlabel, true, lb)
			context.SetLabelPc(nextcondlabel, context.Code.LastPC())
			compileLogicalOpExprAux(context, reg, ex.Rhs, ec, thenlabel, elselabel, hasnextcond, lb)
		}
		return
	case *ast.RelationalOpExpr:
		if thenlabel == elselabel {
			flip ^= 1
			jumplabel = lb.t
			lb.b = true
		} else if thenlabel == lb.e {
			jumplabel = lb.t
			lb.b = true
		} else if elselabel == lb.e {
			jumplabel = lb.f
			lb.b = true
		}
		compileRelationalOpExprAux(context, reg, ex, flip, jumplabel)
		return
	}

	a := reg
	sreg := savereg(ec, a)
	isLastAnd := elselabel == lb.e && thenlabel != elselabel
	isLastOr := thenlabel == lb.e && hasnextcond

	if ident, ok := expr.(*ast.IdentExpr); ok && (isLastAnd || isLastOr) && getIdentRefType(context, context, ident) == ecLocal {
		b := context.FindLocalVar(ident.Value)
		op := OP_TESTSET
		if sreg == b {
			op = OP_TEST
		}
		code.AddABCk(op, sreg, b, 0, flip, sline(expr))
	} else if !hasnextcond && thenlabel == elselabel {
		reg += compileExpr(context, reg, expr, &expcontext{ec.ctype, intMax(a, sreg), ec.varargopt})
		last := context.Code.Last()
		if opGetOpCode(last) == OP_MOVE && opGetArgA(last) == a {
			context.Code.SetA(context.Code.LastPC(), sreg)
		} else {
			context.Code.AddABC(OP_MOVE, sreg, a, 0, sline(expr))
		}
	} else {
		reg += compileExpr(context, reg, expr, ecnone(0))
		if !hasnextcond {
			code.AddABCk(OP_TEST, a, 0, 0, flip, sline(expr))
		} else {
			code.AddABCk(OP_TESTSET, sreg, a, 0, flip, sline(expr))
		}
	}
	code.AddSJ(OP_JMP, jumplabel, sline(expr))
} // }}}

func compileFuncCallExpr(context *funcContext, reg int, expr *ast.FuncCallExpr, ec *expcontext) int { // {{{
	funcreg := reg
	if ec.ctype == ecLocal && ec.reg == (int(context.Proto.NumParameters)-1) {
		funcreg = ec.reg
		reg = ec.reg
	}
	argc := len(expr.Args)
	islastvararg := false
	name := "(anonymous)"

	if expr.Func != nil { // hoge.func()
		reg += compileExpr(context, reg, expr.Func, ecnone(0))
		name = getExprName(context, expr.Func)
	} else { // hoge:method()
		b := reg
		compileExprWithMVPropagation(context, expr.Receiver, &reg, &b)
		if kidx := context.ConstIndex(LString(expr.Method)); kidx <= opMaxIndexK {
			context.Code.AddABC(OP_SELF, funcreg, b, kidx, sline(expr))
		} else {
			// long method names past the K range: stock's fallback
			// (luaK_self) — MOVE the receiver, GETTABLE the method
			keyreg := reg
			reg++
			context.Code.AddABx(OP_LOADK, keyreg, kidx, sline(expr))
			context.Code.AddABC(OP_MOVE, funcreg+1, b, 0, sline(expr))
			context.Code.AddABC(OP_GETTABLE, funcreg, b, keyreg, sline(expr))
		}
		// increments a register for an implicit "self"
		reg = b + 1
		reg2 := funcreg + 2
		if reg2 > reg {
			reg = reg2
		}
		argc += 1
		name = string(expr.Method)
	}

	for i, ar := range expr.Args {
		islastvararg = (i == len(expr.Args)-1) && isVarArgReturnExpr(ar)
		if islastvararg {
			compileExpr(context, reg, ar, ecnone(-2))
		} else {
			reg += compileExpr(context, reg, ar, ecnone(0))
		}
	}
	b := argc + 1
	if islastvararg {
		b = 0
	}
	context.Code.AddABC(OP_CALL, funcreg, b, ec.varargopt+2, sline(expr))
	context.Proto.DbgCalls = append(context.Proto.DbgCalls, DbgCall{Pc: context.Code.LastPC(), Name: name, Method: expr.Func == nil})

	if ec.varargopt == 0 && shouldmove(ec, funcreg) {
		context.Code.AddABC(OP_MOVE, ec.reg, funcreg, 0, sline(expr))
		return 1
	}
	if context.RegTop() > (funcreg+2+ec.varargopt) || ec.varargopt < -1 {
		return 0
	}
	return ec.varargopt + 1
} // }}}

// getIdentRefType classifies a name: a local of the compiling function
// (ecLocal), a local of an enclosing function or a named upvalue visible
// through the chain (ecUpvalue — the closure-capture loop wires the cell
// at OP_CLOSURE), or ecGlobal. U1 (D3): globals resolve through _ENV, so
// the upvalue pool itself is a resolution target — the main chunk's _ENV
// lives there (Compile registers it at index 0), and that is also how a
// nested function finds it before the chain falls off to ecGlobal.
func getIdentRefType(context *funcContext, current *funcContext, expr *ast.IdentExpr) expContextType { // {{{
	if current == nil {
		return ecGlobal
	}
	if current.FindLocalVar(expr.Value) > -1 {
		if current == context {
			return ecLocal
		}
		return ecUpvalue
	}
	if current.Upvalues.Find(expr.Value) > -1 {
		return ecUpvalue
	}
	return getIdentRefType(context, current.Parent, expr)
} // }}}

// envUpvalueName is the upvalue globals resolve through (Lua 5.2's _ENV
// scheme, upgrade plan D3).
const envUpvalueName = "_ENV"

// envAccess resolves how the function being compiled sees _ENV: either a
// local (`local _ENV = t` shadows the upvalue and global accesses compile
// as table reads on that register) or the _ENV upvalue (registered on
// first use; for the main chunk Compile pre-registers it at index 0, for
// any nested function RegisterUnique adds it and the OP_CLOSURE capture
// descriptors wire the cell from the enclosing scope).
func envAccess(context *funcContext) (localIdx, upvalIdx int) {
	if idx := context.FindLocalVar(envUpvalueName); idx > -1 {
		return idx, -1
	}
	return -1, context.Upvalues.RegisterUnique(envUpvalueName)
}

// envUpvalueIndexOf is envAccess's upvalue arm (GETTABUP/SETTABUP B).
func envUpvalueIndexOf(context *funcContext) int {
	_, idx := envAccess(context)
	return idx
}

func getExprName(context *funcContext, expr ast.Expr) string { // {{{
	switch ex := expr.(type) {
	case *ast.IdentExpr:
		return ex.Value
	case *ast.AttrGetExpr:
		switch kex := ex.Key.(type) {
		case *ast.StringExpr:
			return kex.Value
		}
		return "?"
	}
	return "?"
} // }}}

// patchCode finalizes a proto: resolves label-id jump payloads to pc
// offsets (with jump-to-jump chaining, stock luaK_finish) and computes
// NumUsedRegisters from the instruction stream.
func patchCode(context *funcContext) { // {{{
	maxreg := 1
	if np := int(context.Proto.NumParameters); np > 1 {
		maxreg = np
	}
	// an immutable snapshot: chain-following must read pristine label
	// payloads, not offsets already written for earlier jumps
	code := append([]uint32(nil), context.Code.List()...)

	for pc := 0; pc < len(code); pc++ {
		inst := code[pc]
		switch opGetOpCode(inst) {
		case OP_JMP: // resolve the label id to an sJ offset
			label := opGetArgsJ(inst)
			count := 0 // avoiding infinite loops
			for count < 5 {
				tpc := context.GetLabelPc(label) + 1
				if tpc <= 0 || tpc >= len(code) || opGetOpCode(code[tpc]) != OP_JMP {
					break
				}
				next := opGetArgsJ(code[tpc])
				if next == label {
					break
				}
				label = next
				count++
			}
			distance := context.GetLabelPc(label) - pc
			if distance > opMaxArgsJ-opOffsetSJ || distance < -(opOffsetSJ+1) {
				raiseCompileError(context, context.Proto.LineDefined, "control structure too long")
			}
			context.Code.SetSJ(pc, distance)
		case OP_LOADKX, OP_NEWTABLE:
			pc++ // skip the EXTRAARG word (no register use)
			if reg := opGetArgA(inst); reg > maxreg {
				maxreg = reg
			}
		case OP_SETLIST:
			if opGetArgk(inst) != 0 {
				pc++ // EXTRAARG
			}
			if reg := opGetArgA(inst) + opGetArgvB(inst); reg > maxreg {
				maxreg = reg
			}
		case OP_CALL:
			if reg := opGetArgA(inst) + opGetArgC(inst) - 2; reg > maxreg {
				maxreg = reg
			}
		case OP_TAILCALL:
			if reg := opGetArgA(inst) + opGetArgB(inst) - 1; reg > maxreg {
				maxreg = reg
			}
		case OP_RETURN:
			if b := opGetArgB(inst); b != 0 {
				if reg := opGetArgA(inst) + b - 2; reg > maxreg {
					maxreg = reg
				}
			}
		case OP_VARARG:
			if c := opGetArgC(inst); c != 0 {
				if reg := opGetArgA(inst) + c - 2; reg > maxreg {
					maxreg = reg
				}
			}
		case OP_SELF:
			if reg := opGetArgA(inst) + 1; reg > maxreg {
				maxreg = reg
			}
		case OP_LOADNIL:
			if reg := opGetArgA(inst) + opGetArgB(inst); reg > maxreg {
				maxreg = reg
			}
		case OP_CONCAT:
			if reg := opGetArgA(inst) + opGetArgB(inst) - 1; reg > maxreg {
				maxreg = reg
			}
		case OP_FORPREP, OP_FORLOOP:
			if reg := opGetArgA(inst) + 2; reg > maxreg {
				maxreg = reg
			}
		case OP_TFORLOOP:
			if reg := opGetArgA(inst) + 3; reg > maxreg {
				maxreg = reg
			}
		case OP_TFORCALL:
			c := opGetArgC(inst)
			if c < 2 {
				c = 2 // the iterator call's state+control arguments
			}
			if reg := opGetArgA(inst) + 3 + c; reg > maxreg {
				maxreg = reg
			}
		case OP_SETTABUP, OP_SETUPVAL, OP_CLOSE, OP_TBC, OP_RETURN0,
			OP_RETURN1, OP_MMBIN, OP_MMBINI, OP_MMBINK, OP_EQ, OP_LT,
			OP_LE, OP_EQK, OP_EQI, OP_LTI, OP_LEI, OP_GTI, OP_GEI,
			OP_TEST, OP_TFORPREP, OP_EXTRAARG, OP_VARARGPREP:
			/* no register result */
		default:
			if reg := opGetArgA(inst); reg > maxreg {
				maxreg = reg
			}
		}
	}
	maxreg++
	if maxreg > maxRegisters {
		raiseCompileError(context, context.Proto.LineDefined, "register overflow(too many local variables)")
	}
	context.Proto.NumUsedRegisters = uint8(maxreg)
} // }}}

func Compile(chunk []ast.Stmt, name string) (proto *FunctionProto, err error) { // {{{
	defer func() {
		if rcv := recover(); rcv != nil {
			if _, ok := rcv.(*CompileError); ok {
				err = rcv.(error)
			} else {
				panic(rcv)
			}
		}
	}()
	err = nil
	parlist := &ast.ParList{HasVargs: true, Names: []string{}}
	funcexpr := &ast.FunctionExpr{ParList: parlist, Stmts: chunk}
	if len(chunk) > 0 {
		funcexpr.SetLastLine(sline(chunk[0]))
		funcexpr.SetLastLine(eline(chunk[len(chunk)-1]) + 1)
	}
	context := newFuncContext(name, nil)
	// D3: the main chunk declares _ENV as upvalue 0 — the loader seeds
	// the cell with the chunk's environment (load's env argument, the
	// thread env by default); nested functions capture it through the
	// normal OP_CLOSURE machinery.
	context.Upvalues.Register(envUpvalueName)
	compileFunctionExpr(context, funcexpr, ecnone(0))
	proto = context.Proto
	return
} // }}}
