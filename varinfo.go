package lua

import (
	"fmt"
)

/*
  U3 (ledger row 62): stock 5.5.1's debug-name "varinfo" — the
  " (local 'x')" / " (upvalue 'u')" / " (constant 'k')" / " (global 'g')"
  / " (field 'f')" / " (method 'm')" suffixes the VM's type errors carry
  (ldebug.c). This file ports the symbolic-execution machinery:
  findsetreg (which instruction last set the register), basicgetobjname
  / getobjname (what name that instruction implies), and the varinfo
  entry points the raise sites call. The interpreter is the executable
  spec: raise sites thread the faulty operand's frame-relative register
  in; immediates and pool constants are not in a register (-1) — stock's
  instack finds nothing for them either, so they carry no hint.

  The kind set is exactly stock's: "local", "upvalue", "constant",
  "global", "field" (including the GETI "integer index" alias), and
  "method". Compare errors (luaG_ordererror) and the 'for' errors
  (luaG_forerror) carry NO varinfo in stock — neither gets one here.
*/

// varinfo kind strings (ldebug.c strlocal/strupval + getobjname's
// literals; formatvarinfo renders them as " (%s '%s')").
const (
	varinfoLocal    = "local"
	varinfoUpvalue  = "upvalue"
	varinfoConstant = "constant"
	varinfoGlobal   = "global"
	varinfoField    = "field"
	varinfoMethod   = "method"
)

// protoVarinfo appends stock's varinfo hint for frame-relative register
// reg of the given frame/pc, e.g. " (local 'x')" — "" when the operand
// has no name. cf.Pc has already advanced past the about-to-execute
// instruction (currentpc = cf.Pc - 1, stock's pcRel convention).
func protoVarinfo(L *LState, cf *callFrame, reg int) string {
	if cf == nil || cf.Fn == nil || cf.Fn.IsG || cf.Fn.Proto == nil || reg < 0 {
		return ""
	}
	p := cf.Fn.Proto
	pc := cf.Pc - 1
	if pc < 0 || pc >= len(p.Code) {
		return ""
	}
	kind, name := getObjName(p, pc, reg)
	return formatVarInfo(kind, name)
}

// frameRegVarinfo is protoVarinfo over the current frame (stock's
// varinfo reads L->ci); reg < 0 marks an operand that is not in a
// register (immediates, pool constants).
func frameRegVarinfo(L *LState, reg int) string {
	if reg < 0 {
		return ""
	}
	return protoVarinfo(L, L.currentFrame, reg)
}

// upvalueVarinfo covers the one operand stock's varinfo resolves
// outside a register: the GETTABUP/SETTABUP table, an upvalue cell
// (varinfo's getupvalname arm).
func upvalueVarinfo(cf *callFrame, uv int) string {
	if cf == nil || cf.Fn == nil || cf.Fn.IsG || cf.Fn.Proto == nil {
		return ""
	}
	return formatVarInfo(varinfoUpvalue, upvalName(cf.Fn.Proto, uv))
}

// formatVarInfo — stock formatvarinfo: " (%s '%s')", "" without a kind.
func formatVarInfo(kind, name string) string {
	if kind == "" {
		return ""
	}
	return fmt.Sprintf(" (%s '%s')", kind, name)
}

// varinfoHint is the lazy carrier for the raise-site hints: resolving one
// runs findSetReg's symbolic walk over the proto's code — O(codelen) — so
// the field ops receive the (frame, register) pair and resolve only on
// the error path, never per table access (the eager form cost the
// interpreter a 3x slowdown; the zero value resolves to "").
type varinfoHint struct {
	cf   *callFrame
	reg  int  // frame-relative register of the object operand; <0 = none
	uv   int  // upvalue slot of the object (GETTABUP/SETTABUP's table)
	isUp bool // the upvalue flavor
}

func regHint(cf *callFrame, reg int) varinfoHint  { return varinfoHint{cf: cf, reg: reg} }
func upvalHint(cf *callFrame, uv int) varinfoHint { return varinfoHint{cf: cf, uv: uv, isUp: true} }

// resolve computes the hint text — error paths only.
func (h varinfoHint) resolve(L *LState) string {
	if h.cf == nil {
		return ""
	}
	if h.isUp {
		return upvalueVarinfo(h.cf, h.uv)
	}
	if h.reg < 0 {
		return ""
	}
	return protoVarinfo(L, h.cf, h.reg)
}

/* {{{ symbolic execution (findsetreg and friends) */

// filterpc (stock): code before jmptarget is conditional (inside a
// jump) — its register writes cannot be attributed.
func filterpc(pc, jmptarget int) int {
	if pc < jmptarget {
		return -1
	}
	return pc
}

// opIsMMMode — stock testMMMode (luaP_opmodes bit 7): only the MMBIN
// family. findsetreg steps back over the MMBIN slot because it is the
// about-to-execute instruction when a metamethod path fails.
func opIsMMMode(op int) bool {
	return op == OP_MMBIN || op == OP_MMBINI || op == OP_MMBINK
}

// opSetsRegA — stock testAMode over opProps (the fork's mirror of
// luaP_opmodes' A bit).
func opSetsRegA(op int) bool {
	return op >= 0 && op <= opCodeMax && opProps[op].SetRegA
}

// findSetReg (stock findsetreg): the last instruction before lastpc
// that set register reg — LOADNIL's A..A+B range, TFORCALL/CALL/
// TAILCALL's everything-above-base effects, JMP jmptarget tracking, and
// the default A-mode sets. -1 when no attributable setter is found.
func findSetReg(p *FunctionProto, lastpc, reg int) int {
	setreg := -1
	jmptarget := 0
	if lastpc >= 0 && lastpc < len(p.Code) && opIsMMMode(opGetOpCode(p.Code[lastpc])) {
		lastpc-- // previous instruction was not actually executed
	}
	if lastpc > len(p.Code) {
		lastpc = len(p.Code)
	}
	for pc := 0; pc < lastpc; pc++ {
		inst := p.Code[pc]
		op := opGetOpCode(inst)
		a := opGetArgA(inst)
		var change bool
		switch op {
		case OP_LOADNIL: // set registers from 'a' to 'a+b'
			b := opGetArgB(inst)
			change = a <= reg && reg <= a+b
		case OP_TFORCALL: // affects all regs above its base
			change = reg >= a+2
		case OP_CALL, OP_TAILCALL: // affect all registers above base
			change = reg >= a
		case OP_JMP: // changes 'jmptarget', not registers
			dest := pc + 1 + opGetArgsJ(inst)
			if dest <= lastpc && dest > jmptarget {
				jmptarget = dest
			}
			change = false
		default: // any instruction that sets A
			change = opSetsRegA(op) && reg == a
		}
		if change {
			setreg = filterpc(pc, jmptarget)
		}
	}
	return setreg
}

// localNameAt — stock luaF_getlocalname over DbgLocals: the name of the
// localnum'ts (1-based) active local at pc. The fork records StartPc =
// the first active pc and EndPc = the LAST active pc, so stock's
// half-open [startpc, endpc) is the closed [StartPc, EndPc] here.
func localNameAt(p *FunctionProto, localnum, pc int) (string, bool) {
	for i := 0; i < len(p.DbgLocals); i++ {
		lv := p.DbgLocals[i]
		if lv.StartPc > pc {
			break
		}
		if pc <= lv.EndPc {
			localnum--
			if localnum == 0 {
				return lv.Name, true
			}
		}
	}
	return "", false
}

// LocalNameAt is localNameAt exported for the wasm backend's OP_TBC
// lowering (U4f): the non-closable raise names the variable, and the
// backend precomputes the interp's DbgLocals answer at compile time.
func LocalNameAt(p *FunctionProto, localnum, pc int) (string, bool) {
	return localNameAt(p, localnum, pc)
}

// upvalName — stock upvalname ("?" for unnamed captures).
func upvalName(p *FunctionProto, uv int) string {
	if uv >= 0 && uv < len(p.Upvalues) && p.Upvalues[uv].Name != "" {
		return p.Upvalues[uv].Name
	}
	return "?"
}

// knameAt — stock kname: only STRING constants are nameable ("?" with
// no kind otherwise).
func knameAt(p *FunctionProto, index int) (kind, name string) {
	if index >= 0 && index < len(p.Constants) {
		if s, ok := p.Constants[index].(LString); ok {
			return varinfoConstant, string(s)
		}
	}
	return "", "?"
}

// basicGetObjName — stock basicgetobjname: the local name for the
// register, else the name implied by the instruction findsetreg last
// set it with (MOVE / GETUPVAL / LOADK / LOADKX). newpc carries
// findsetreg's result back (the C *ppc out-parameter).
func basicGetObjName(p *FunctionProto, ppc, reg int) (kind, name string, newpc int) {
	pc := ppc
	if nm, ok := localNameAt(p, reg+1, pc); ok {
		return varinfoLocal, nm, pc
	}
	pc = findSetReg(p, pc, reg)
	if pc != -1 && pc < len(p.Code) {
		inst := p.Code[pc]
		switch opGetOpCode(inst) {
		case OP_MOVE: // move from 'b' to 'a'
			if b := opGetArgB(inst); b < opGetArgA(inst) {
				return basicGetObjName(p, pc, b)
			}
		case OP_GETUPVAL:
			return varinfoUpvalue, upvalName(p, opGetArgB(inst)), pc
		case OP_LOADK:
			k, nm := knameAt(p, opGetArgBx(inst))
			return k, nm, pc
		case OP_LOADKX: // the Ax index lives in the following EXTRAARG
			if pc+1 < len(p.Code) {
				k, nm := knameAt(p, opGetArgAx(p.Code[pc+1]))
				return k, nm, pc
			}
		}
	}
	return "", "", pc
}

// rnameAt — stock rname: a name for the key register 'c'; only a
// constant name counts, "?" otherwise.
func rnameAt(p *FunctionProto, pc, c int) string {
	kind, name, _ := basicGetObjName(p, pc, c)
	if kind != varinfoConstant {
		name = "?"
	}
	return name
}

// isEnvKind — stock isEnv: "global" when the indexed table is the _ENV
// variable, "field" otherwise.
func isEnvKind(p *FunctionProto, pc int, inst uint32, isup bool) string {
	t := opGetArgB(inst)
	name := ""
	if isup {
		name = upvalName(p, t)
	} else {
		what, nm, _ := basicGetObjName(p, pc, t)
		// only a local or an upvalue can be the variable _ENV
		if what == varinfoLocal || what == varinfoUpvalue {
			name = nm
		}
	}
	if name == envUpvalueName {
		return varinfoGlobal
	}
	return varinfoField
}

// getObjName — stock getobjname: basicgetobjname extended with the
// table-access instructions (GETTABUP / GETTABLE / GETVARG / GETI /
// GETFIELD / SELF).
func getObjName(p *FunctionProto, lastpc, reg int) (kind, name string) {
	kind, name, lastpc = basicGetObjName(p, lastpc, reg)
	if kind != "" {
		return kind, name
	}
	if lastpc != -1 && lastpc < len(p.Code) {
		inst := p.Code[lastpc]
		switch opGetOpCode(inst) {
		case OP_GETTABUP:
			_, name = knameAt(p, opGetArgC(inst))
			return isEnvKind(p, lastpc, inst, true), name
		case OP_GETTABLE, OP_GETVARG:
			name = rnameAt(p, lastpc, opGetArgC(inst))
			return isEnvKind(p, lastpc, inst, false), name
		case OP_GETI:
			return varinfoField, "integer index"
		case OP_GETFIELD:
			_, name = knameAt(p, opGetArgC(inst))
			return isEnvKind(p, lastpc, inst, false), name
		case OP_SELF:
			_, name = knameAt(p, opGetArgC(inst))
			return varinfoMethod, name
		}
	}
	return "", ""
}

/* }}} */

/* {{{ call-site names (funcnamefromcode / luaG_callerror) */

// tmEventName maps a tag-method number (the MMBIN family's C operand,
// opcode.go's tm* order) to its event name — stock reads tmname[tm] and
// skips the two leading underscores.
func tmEventName(tm int) string {
	switch tm {
	case tmIndex:
		return "index"
	case tmNewindex:
		return "newindex"
	case tmGC:
		return "gc"
	case tmMode:
		return "mode"
	case tmLen:
		return "len"
	case tmEq:
		return "eq"
	case tmAdd:
		return "add"
	case tmSub:
		return "sub"
	case tmMul:
		return "mul"
	case tmMod:
		return "mod"
	case tmPow:
		return "pow"
	case tmDiv:
		return "div"
	case tmIdiv:
		return "idiv"
	case tmBand:
		return "band"
	case tmBor:
		return "bor"
	case tmBxor:
		return "bxor"
	case tmShl:
		return "shl"
	case tmShr:
		return "shr"
	case tmUnm:
		return "unm"
	case tmBnot:
		return "bnot"
	case tmLt:
		return "lt"
	case tmLe:
		return "le"
	case tmConcat:
		return "concat"
	case tmCall:
		return "call"
	case tmClose:
		return "close"
	}
	return "?"
}

// funcNameFromCode — stock funcnamefromcode: a name for the callee of
// the instruction at pc. OP_CALL/OP_TAILCALL resolve the function
// register through getobjname; OP_TFORCALL is the "for iterator"; the
// metamethod-calling opcodes name the event itself.
func funcNameFromCode(p *FunctionProto, pc int) (kind, name string) {
	if pc < 0 || pc >= len(p.Code) {
		return "", ""
	}
	inst := p.Code[pc]
	switch opGetOpCode(inst) {
	case OP_CALL, OP_TAILCALL:
		return getObjName(p, pc, opGetArgA(inst))
	case OP_TFORCALL:
		return "for iterator", "for iterator"
	case OP_SELF, OP_GETTABUP, OP_GETTABLE, OP_GETI, OP_GETFIELD:
		return "metamethod", "index"
	case OP_SETTABUP, OP_SETTABLE, OP_SETI, OP_SETFIELD:
		return "metamethod", "newindex"
	case OP_MMBIN, OP_MMBINI, OP_MMBINK:
		return "metamethod", tmEventName(opGetArgC(inst))
	case OP_UNM:
		return "metamethod", "unm"
	case OP_BNOT:
		return "metamethod", "bnot"
	case OP_LEN:
		return "metamethod", "len"
	case OP_CONCAT:
		return "metamethod", "concat"
	case OP_EQ:
		return "metamethod", "eq"
	case OP_LT, OP_LTI, OP_GTI:
		return "metamethod", "lt"
	case OP_LE, OP_LEI, OP_GEI:
		return "metamethod", "le"
	case OP_CLOSE, OP_RETURN:
		return "metamethod", "close"
	}
	return "", ""
}

// raiseCallError — stock luaG_callerror: name the callee from the
// calling code (funcnamefromcall) when the caller is a Lua frame; a
// caller that yields no name falls back to varinfo of the callee value
// (stock's instack scan of the frame's registers). The raise is
// luaG_runerror's position rule (U5c): the CURRENT frame — a C/G caller
// (pcall, xpcall) yields NO position, so pcall(42) answers a bare
// "attempt to call a number value".
func (ls *LState) raiseCallError(fn LValue) {
	extra := ""
	if cf := ls.currentFrame; cf != nil && cf.Fn != nil && !cf.Fn.IsG && cf.Fn.Proto != nil {
		kind, name := funcNameFromCode(cf.Fn.Proto, cf.Pc-1)
		if kind == "" {
			for reg := cf.LocalBase; reg < ls.reg.Top(); reg++ {
				if ls.reg.Get(reg) == fn {
					kind, name = getObjName(cf.Fn.Proto, cf.Pc-1, reg-cf.LocalBase)
					break
				}
			}
		}
		extra = formatVarInfo(kind, name)
	}
	ls.raiseErrorAt(0, "attempt to call a %s value%s", ls.objTypeName(fn), extra)
}

/* }}} */
