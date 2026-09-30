package lua

import (
	"fmt"
	"math"
	"strings"
)

func mainLoop(L *LState, baseframe *callFrame) {
	var inst uint32
	var cf *callFrame

	if L.stack.IsEmpty() {
		return
	}

	L.currentFrame = L.stack.Last()
	if L.currentFrame.Fn.IsG {
		callGFunction(L, false)
		return
	}

	for {
		cf = L.currentFrame
		inst = cf.Fn.Proto.Code[cf.Pc]
		cf.Pc++
		if jumpTable[int(inst&0x7f)](L, inst, baseframe) == 1 {
			return
		}
	}
}

func mainLoopWithContext(L *LState, baseframe *callFrame) {
	var inst uint32
	var cf *callFrame

	if L.stack.IsEmpty() {
		return
	}

	L.currentFrame = L.stack.Last()
	if L.currentFrame.Fn.IsG {
		callGFunction(L, false)
		return
	}

	for {
		cf = L.currentFrame
		inst = cf.Fn.Proto.Code[cf.Pc]
		cf.Pc++
		select {
		case <-L.ctx.Done():
			L.RaiseError("%s", L.ctx.Err().Error())
			return
		default:
			if jumpTable[int(inst&0x7f)](L, inst, baseframe) == 1 {
				return
			}
		}
	}
}

// regv is the first target register to copy the return values to.
// It can be reg.top, indicating that the copied values are going into new registers, or it can be below reg.top
// Indicating that the values should be within the existing registers.
// b is the available number of return values + 1.
// n is the desired number of return values.
// If n more than the available return values then the extra values are set to nil.
// When this function returns the top of the registry will be set to regv+n.
func copyReturnValues(L *LState, regv, start, n, b int) { // +inline-start
	if b == 1 {
		// +inline-call L.reg.FillNil  regv n
	} else {
		// +inline-call L.reg.CopyRange regv start -1 n
		if b > 1 && n > (b-1) {
			// +inline-call L.reg.FillNil  regv+b-1 n-(b-1)
		}
	}
} // +inline-end

func switchToParentThread(L *LState, nargs int, haserror bool, kill bool) {
	parent := L.Parent
	if parent == nil {
		L.RaiseError("can not yield from outside of a coroutine")
	}
	L.G.CurrentThread = parent
	L.Parent = nil
	if !L.wrapped {
		if haserror {
			parent.Push(LFalse)
		} else {
			parent.Push(LTrue)
		}
	}
	L.XMoveTo(parent, nargs)
	L.stack.Pop()
	offset := L.currentFrame.LocalBase - L.currentFrame.ReturnBase
	L.currentFrame = L.stack.Last()
	L.reg.SetTop(L.reg.Top() - offset) // remove 'yield' function(including tailcalled functions)
	if kill {
		L.kill()
	}
}

func callGFunction(L *LState, tailcall bool) bool {
	frame := L.currentFrame
	gfnret := frame.Fn.GFunction(L)
	if tailcall {
		L.currentFrame = L.RemoveCallerFrame()
	}

	if gfnret < 0 {
		switchToParentThread(L, L.GetTop(), false, false)
		return true
	}

	wantret := frame.NRet
	if wantret == MultRet {
		wantret = gfnret
	}

	if tailcall && L.Parent != nil && L.stack.Sp() == 1 {
		switchToParentThread(L, wantret, false, true)
		return true
	}

	if L.Parent != nil && L.stack.Sp() == 1 {
		// a G function as a coroutine's base frame (e.g. the gmatch
		// iterator resumed through coroutine.wrap — the Lua 5.3.2 bug
		// block in strings.lua): the last frame returning routes its
		// results back to the parent thread and finishes the thread,
		// exactly like doReturn's last-frame path (the base frame's
		// NRet is always MultRet, so the results are already staged
		// contiguously at the register top for XMoveTo)
		switchToParentThread(L, wantret, false, true)
		return true
	}

	// +inline-call L.reg.CopyRange frame.ReturnBase L.reg.Top()-gfnret -1 wantret
	L.stack.Pop()
	L.currentFrame = L.stack.Last()
	return false
}

// wrappedDeath finishes a WRAPPED thread that died with error lv (U4d):
// the error surfaces as a raise on the resumer, but the thread itself
// must still die — Dead set, CurrentThread handed back to the resumer —
// else a later resume of the corpse hits the running guard instead of
// the dead one (stock's unrecoverable-error arm sets L->status; official
// coroutine.lua's "bug in 5.4.1" block pins it).
func wrappedDeath(L, parent *LState, lv LValue) {
	L.deadErr = lv
	L.Push(lv)
	L.G.CurrentThread = parent
	L.Parent = nil
	L.kill()
	parent.Panic(L)
}

func threadRun(L *LState) {
	if L.stack.IsEmpty() {
		return
	}

	defer func() {
		if rcv := recover(); rcv != nil {
			if te, ok := rcv.(*closeSelfUnwind); ok {
				// U4d (stock luaD_throwbaselevel + luaE_resetthread): the
				// thread closed ITSELF — its closes already ran in coClose;
				// terminate at the base level with the close outcome (clean
				// or error), bypassing every intermediate recovery point
				if te.ok {
					L.SetTop(0)
					if parent := L.Parent; parent != nil {
						switchToParentThread(L, 0, false, true)
					} else {
						L.kill()
					}
				} else {
					if parent := L.Parent; parent != nil {
						if L.wrapped {
							wrappedDeath(L, parent, te.errobj)
						} else {
							L.deadErr = te.errobj
							L.SetTop(0)
							L.Push(te.errobj)
							switchToParentThread(L, 1, true, true)
						}
					} else {
						L.deadErr = te.errobj
						panic(rcv)
					}
				}
				return
			}
			var lv LValue
			if v, ok := rcv.(*ApiError); ok {
				lv = v.Object
			} else {
				lv = LString(fmt.Sprint(rcv))
			}
			// U4d (stock luaE_resetthread): the nil-object fix, then the
			// dying thread's upvalues and tbc close with the dying error
			// object (2-arg __close) before it propagates
			if lv == LNil {
				lv = LString("<no error object>")
			}
			L.closeUpvalues(0)
			if obj, failed := L.closeTBCProtected(0, lv); failed {
				lv = obj
			}
			L.deadErr = lv // coroutine.close's false+err source (U3h)
			if parent := L.Parent; parent != nil {
				if L.wrapped {
					wrappedDeath(L, parent, lv)
				} else {
					L.SetTop(0)
					L.Push(lv)
					switchToParentThread(L, 1, true, true)
				}
			} else {
				panic(rcv)
			}
		}
	}()
	L.mainLoop(L, nil)
}

type instFunc func(*LState, uint32, *callFrame) int

var jumpTable [opCodeMax + 1]instFunc

func init() {
	jumpTable = [opCodeMax + 1]instFunc{
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_MOVE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			v := reg.Get(lbase + B)
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADI
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			sBx := int(inst>>15&0x1ffff) - opMaxArgSbx //GETSBX
			iv := LInteger(sBx)
			// +inline-call reg.SetInteger RA iv
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADF
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			sBx := int(inst>>15&0x1ffff) - opMaxArgSbx //GETSBX
			fv := LNumber(float64(sBx))
			// +inline-call reg.SetNumber RA fv
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADK
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			v := cf.Fn.Proto.Constants[Bx]
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADKX
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			// the Ax index lives in the FOLLOWING EXTRAARG word (stock
			// lvm.c reads GETARG_Ax(*pc)); the compiler emits LOADKX with
			// a zeroed Bx field
			Ax := int(cf.Fn.Proto.Code[cf.Pc]>>7) & 0x1ffffff //GETAX (EXTRAARG)
			v := cf.Fn.Proto.Constants[Ax]
			cf.Pc++ // skip EXTRAARG
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADFALSE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			// +inline-call reg.Set RA LFalse
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LFALSESKIP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			// +inline-call reg.Set RA LFalse
			cf.Pc++
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADTRUE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			// +inline-call reg.Set RA LTrue
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADNIL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff  //GETA
			B := int(inst>>16) & 0xff //GETB
			for i := lbase + A; i <= lbase+A+B; i++ {
				// +inline-call reg.Set i LNil
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETUPVAL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			v := cf.Fn.Upvalues[B].Value()
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETUPVAL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			cf.Fn.Upvalues[B].SetValue(reg.Get(RA))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETTABUP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			t := cf.Fn.Upvalues[B].Value()
			k := cf.Fn.Proto.Constants[C]
			v := L.getFieldString(t, string(k.(LString)), upvalHint(cf, B))
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETTABLE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			v := L.getField(reg.Get(lbase+B), reg.Get(lbase+C), regHint(cf, B))
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETI
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			v := L.getField(reg.Get(lbase+B), LInteger(C), regHint(cf, B))
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETFIELD
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			v := L.getFieldString(reg.Get(lbase+B), string(cf.Fn.Proto.Constants[C].(LString)), regHint(cf, B))
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETTABUP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			B := int(inst>>16) & 0xff //GETB
			C := int(inst>>24) & 0xff //GETC
			k := int(inst>>15) & 1    //GETK
			t := cf.Fn.Upvalues[A].Value()
			key := string(cf.Fn.Proto.Constants[B].(LString))
			var v LValue
			if k != 0 {
				v = cf.Fn.Proto.Constants[C]
			} else {
				v = reg.Get(lbase + C)
			}
			L.setFieldString(t, key, v, upvalHint(cf, A))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETTABLE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			k := int(inst>>15) & 1    //GETK
			var v LValue
			if k != 0 {
				v = cf.Fn.Proto.Constants[C]
			} else {
				v = reg.Get(lbase + C)
			}
			L.setField(reg.Get(RA), reg.Get(lbase+B), v, regHint(cf, A))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETI
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			k := int(inst>>15) & 1    //GETK
			var v LValue
			if k != 0 {
				v = cf.Fn.Proto.Constants[C]
			} else {
				v = reg.Get(lbase + C)
			}
			L.setField(reg.Get(RA), LInteger(B), v, regHint(cf, A))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETFIELD
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			k := int(inst>>15) & 1    //GETK
			key := string(cf.Fn.Proto.Constants[B].(LString))
			var v LValue
			if k != 0 {
				v = cf.Fn.Proto.Constants[C]
			} else {
				v = reg.Get(lbase + C)
			}
			L.setFieldString(reg.Get(RA), key, v, regHint(cf, A))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_NEWTABLE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			vB := int(inst>>16) & 0x3f //GETVB
			vC := int(inst>>22) & 0x3ff //GETVC
			k := int(inst>>15) & 1     //GETK
			asize := vC
			if k != 0 {
				// the array-size high word lives in the FOLLOWING EXTRAARG
				// word (stock lvm.c: c += GETARG_Ax(*pc) * (MAXARG_vC+1))
				Ax := int(cf.Fn.Proto.Code[cf.Pc]>>7) & 0x1ffffff //GETAX (EXTRAARG)
				asize += Ax * (opMaxArgsvC + 1)
			}
			cf.Pc++ // skip EXTRAARG
			hsize := 0
			if vB > 0 {
				hsize = 1 << uint(vB-1)
			}
			v := newLTable(asize, hsize)
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SELF
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			selfobj := reg.Get(lbase + B)
			v := L.getFieldString(selfobj, string(cf.Fn.Proto.Constants[C].(LString)), regHint(cf, B))
			// +inline-call reg.Set RA v
			// +inline-call reg.Set RA+1 selfobj
			return 0
		},
		opArithI, // OP_ADDI
		opArithK, // OP_ADDK
		opArithK, // OP_SUBK
		opArithK, // OP_MULK
		opArithK, // OP_MODK
		opArithK, // OP_POWK
		opArithK, // OP_DIVK
		opArithK, // OP_IDIVK
		opBitwiseK, // OP_BANDK
		opBitwiseK, // OP_BORK
		opBitwiseK, // OP_BXORK
		opSHLI,     // OP_SHLI
		opSHRI,     // OP_SHRI
		opArith,    // OP_ADD
		opArith,    // OP_SUB
		opArith,    // OP_MUL
		opArith,    // OP_MOD
		opArith,    // OP_POW
		opArith,    // OP_DIV
		opArith,    // OP_IDIV
		opBitwise,  // OP_BAND
		opBitwise,  // OP_BOR
		opBitwise,  // OP_BXOR
		opBitwise,  // OP_SHL
		opBitwise,  // OP_SHR
		opMMBIN,    // OP_MMBIN
		opMMBINI,   // OP_MMBINI
		opMMBINK,   // OP_MMBINK
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_UNM
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			unaryv := reg.Get(lbase + B)
			if nm, ok := unaryv.(LInteger); ok {
				// +inline-call reg.SetInteger RA -nm
			} else if nm, ok := unaryv.(LNumber); ok {
				// +inline-call reg.SetNumber RA -nm
			} else {
				// U3c: the string metatable's __unm carries the coercion
				// (row 58) — no VM-level string fast path remains. U4d:
				// stock luaT_trybinTM(v1, v1) calls whatever metamethod
				// it found — a non-callable one surfaces as the call
				// error "metamethod 'unm'" (errors.lua's naming battery)
				op := L.metaOp1(unaryv, "__unm")
				if op != LNil {
					reg.Push(op)
					reg.Push(unaryv)
					reg.Push(unaryv)
					L.Call(2, 1)
					// +inline-call reg.Set RA reg.Pop()
				} else {
					// luaG_opinterror picks the (non-number) operand —
					// here and below, varinfo names its register (row 62)
					L.RaiseError("attempt to perform arithmetic on a %s value%s", L.objTypeName(unaryv), regHint(cf, B).resolve(L))
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_BNOT
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			unaryv := reg.Get(lbase + B)
			if iv, ok := unaryv.(LInteger); ok {
				nv := ^iv
				// +inline-call reg.SetInteger RA nv
			} else if fv, ok := unaryv.(LNumber); ok {
				if iv, ok2 := floatToIntExact(float64(fv)); ok2 {
					nv := ^LInteger(iv)
					// +inline-call reg.SetInteger RA nv
				} else {
					// luaG_tointerror over the operand (row 62)
					L.RaiseError("number%s has no integer representation", regHint(cf, B).resolve(L))
				}
			} else {
				// U4d: stock luaT_trybinTM(v1, v1) — call whatever
				// metamethod was found (see OP_UNM)
				op := L.metaOp1(unaryv, "__bnot")
				if op != LNil {
					reg.Push(op)
					reg.Push(unaryv)
					reg.Push(unaryv)
					L.Call(2, 1)
					// +inline-call reg.Set RA reg.Pop()
				} else {
					// no string coercion: the string metatable has no
					// __bnot (stock stringmetamethods); luaG_opinterror
					// names the operand's register (row 62)
					L.RaiseError("attempt to perform bitwise operation on a %s value%s", L.objTypeName(unaryv), regHint(cf, B).resolve(L))
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_NOT
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			if LVIsFalse(reg.Get(lbase + B)) {
				// +inline-call reg.Set RA LTrue
			} else {
				// +inline-call reg.Set RA LFalse
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LEN
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			lv := reg.Get(lbase + B)
			switch lv.(type) {
			case LString:
				nv := LInteger(len(lv.(LString)))
				// +inline-call reg.SetInteger RA nv
			default:
				// U4d: stock luaV_objlen calls the FIRST metamethod found
				// with the object twice (luaT_callTMres(tm, rb, rb)) — a
				// non-callable one surfaces as "metamethod 'len'"
				op := L.metaOp1(lv, "__len")
				if op != LNil {
					reg.Push(op)
					reg.Push(lv)
					reg.Push(lv)
					L.Call(2, 1)
					ret := reg.Pop()
					// +inline-call reg.Set RA ret
				} else if lv.Type() == LTTable {
					nv := LInteger(lv.(*LTable).Len())
					// +inline-call reg.SetInteger RA nv
				} else {
					// luaG_typeerror(rb, "get length of") + varinfo (row 62)
					L.RaiseError("attempt to get length of a %s value%s", L.objTypeName(lv), regHint(cf, B).resolve(L))
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CONCAT
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff  //GETA
			B := int(inst>>16) & 0xff //GETB
			RA := lbase + A
			v := stringConcat(L, B, RA+B-1, cf)
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CLOSE
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			// +inline-call L.closeUpvalues RA
			// U4: upvalues first, then to-be-closed (stock luaF_close);
			// a __close error propagates as a normal error
			L.closeTBC(RA)
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TBC
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			L.registerTBC(RA, A+1)
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_JMP
			cf := L.currentFrame
			sJ := int(inst>>7&0x1ffffff) - opOffsetSJ //GETSJ
			cf.Pc += sJ
			return 0
		},
		opEQTest,  // OP_EQ
		opLTTest,  // OP_LT
		opLETest,  // OP_LE
		opEQKTest, // OP_EQK
		opEQITest, // OP_EQI
		opOrderI,  // OP_LTI
		opOrderI,  // OP_LEI
		opOrderI,  // OP_GTI
		opOrderI,  // OP_GEI
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TEST
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			k := int(inst>>15) & 1   //GETK
			cond := !LVIsFalse(reg.Get(lbase + A))
			if cond != (k == 1) {
				cf.Pc++
			} else {
				ni := cf.Fn.Proto.Code[cf.Pc]
				cf.Pc += int(ni>>7&0x1ffffff) - opOffsetSJ + 1
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TESTSET
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			k := int(inst>>15) & 1    //GETK
			if value := reg.Get(lbase + B); LVIsFalse(value) == (k == 1) {
				cf.Pc++
			} else {
				// +inline-call reg.Set RA value
				ni := cf.Fn.Proto.Code[cf.Pc]
				cf.Pc += int(ni>>7&0x1ffffff) - opOffsetSJ + 1
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CALL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff  //GETB
			C := int(inst>>24) & 0xff //GETC
			nargs := B - 1
			if B == 0 {
				nargs = reg.Top() - (RA + 1)
			}
			lv := reg.Get(RA)
			nret := C - 1
			var callable *LFunction
			var meta bool
			if fn, ok := lv.(*LFunction); ok {
				callable = fn
				meta = false
			} else {
				callable, meta = L.metaCall(lv)
			}
			// +inline-call L.pushCallFrame callFrame{Fn:callable,Pc:0,Base:RA,LocalBase:RA+1,ReturnBase:RA,NArgs:nargs,NRet:nret,Parent:cf,TailCall:0} lv meta
			if callable.IsG && callGFunction(L, false) {
				return 1
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TAILCALL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			B := int(inst>>16) & 0xff //GETB
			nargs := B - 1
			if B == 0 {
				nargs = reg.Top() - (RA + 1)
			}
			lv := reg.Get(RA)
			var callable *LFunction
			var meta bool
			if fn, ok := lv.(*LFunction); ok {
				callable = fn
				meta = false
			} else {
				callable, meta = L.metaCall(lv)
			}
			if callable == nil {
				// U2f re-pin: luaG_typeerror names the callee's type;
				// U3 row 62: luaG_callerror's call-site name or varinfo
				L.raiseCallError(lv)
			}
			// +inline-call L.closeUpvalues lbase
			// U4: close this frame's <close> variables before the frame
			// is reused (stock TAILCALL k / CLOSEKTOP; the compiler
			// suppresses the promotion inside a tbc scope, this is the
			// safety net)
			L.closeTBC(lbase)
			if callable.IsG {
				luaframe := cf
				L.pushCallFrame(callFrame{
					Fn:         callable,
					Pc:         0,
					Base:       RA,
					LocalBase:  RA + 1,
					ReturnBase: cf.ReturnBase,
					NArgs:      nargs,
					NRet:       cf.NRet,
					Parent:     cf,
					TailCall:   0,
				}, lv, meta)
				if callGFunction(L, true) {
					return 1
				}
				if L.currentFrame == nil || L.currentFrame.Fn.IsG || luaframe == baseframe {
					return 1
				}
			} else {
				base := cf.Base
				cf.Fn = callable
				cf.Pc = 0
				cf.Base = RA
				cf.LocalBase = RA + 1
				cf.ReturnBase = cf.ReturnBase
				cf.NArgs = nargs
				cf.NRet = cf.NRet
				cf.TailCall++
				lbase := cf.LocalBase
				if meta {
					cf.NArgs++
					L.reg.Insert(lv, cf.LocalBase)
				}
				// +inline-call L.initCallFrame cf
				// +inline-call L.reg.CopyRange base RA -1 reg.Top()-RA-1
				cf.Base = base
				cf.LocalBase = base + (cf.LocalBase - lbase + 1)
			}
			return 0
		},
		opReturn,  // OP_RETURN
		opReturn0, // OP_RETURN0
		opReturn1, // OP_RETURN1
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_FORLOOP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			if count, ok := reg.Get(RA).(LInteger); ok {
				// integer loop: RA=count, RA+1=step, RA+2=control
				if count > 0 {
					nv := count - 1
					// +inline-call reg.SetInteger RA nv
					step := int64(reg.Get(RA+1).(LInteger))
					idx := int64(reg.Get(RA+2).(LInteger)) + step
					iv := LInteger(idx)
					// +inline-call reg.SetInteger RA+2 iv
					cf.Pc -= Bx
				}
			} else if stepv, ok1 := arithNumber(reg.Get(RA + 1)); ok1 {
				if limit, ok2 := arithNumber(reg.Get(RA)); ok2 {
					idx, ok3 := arithNumber(reg.Get(RA + 2))
					if !ok3 {
						return 0
					}
					idx += stepv
					if (stepv > 0 && idx <= limit) || (stepv <= 0 && idx >= limit) {
						v := LNumber(idx)
						// +inline-call reg.SetNumber RA+2 v
						cf.Pc -= Bx
					}
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_FORPREP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			initv := reg.Get(RA)
			stepv := reg.Get(RA + 2)
			initi, initIsInt := initv.(LInteger)
			stepi, stepIsInt := stepv.(LInteger)
			if initIsInt && stepIsInt {
				// integer loop (init and step integer-Typed, per stock
				// forprep; the limit converts below). Layout after
				// preparation: RA=count, RA+1=step, RA+2=control.
				if stepi == 0 {
					L.RaiseError("'for' step is zero")
				}
				limit, skip := forLimitInt(L, int64(initi), reg.Get(RA+1), int64(stepi))
				if skip {
					cf.Pc += Bx + 1
					return 0
				}
				var count uint64
				if stepi > 0 {
					count = uint64(limit) - uint64(initi)
					if stepi != 1 {
						count /= uint64(stepi)
					}
				} else {
					count = uint64(initi) - uint64(limit)
					// 'step+1' avoids negating minint
					count /= uint64(-(stepi + 1)) + 1
				}
				cv := LInteger(int64(count))
				sv := LInteger(stepi)
				iv := LInteger(int64(initi))
				// +inline-call reg.SetInteger RA cv
				// +inline-call reg.SetInteger RA+1 sv
				// +inline-call reg.SetInteger RA+2 iv
			} else {
				// float loop: coerce all three (strings included, per
				// stock's tonumber), checking limit/step/init in order
				limitf, ok1 := forNumber(L, reg.Get(RA+1), "limit")
				stepf, ok2 := forNumber(L, stepv, "step")
				initf, ok3 := forNumber(L, initv, "initial value")
				if ok1 && ok2 && ok3 {
					if stepf == 0 {
						L.RaiseError("'for' step is zero")
					}
					if (stepf > 0 && limitf < initf) || (stepf < 0 && initf < limitf) {
						cf.Pc += Bx + 1
						return 0
					}
					// make sure all three cells hold floats (stock's
					// forprep rewrites the slots; string limits included)
					lv := LNumber(limitf)
					sv2 := LNumber(stepf)
					v := LNumber(initf)
					// +inline-call reg.SetNumber RA lv
					// +inline-call reg.SetNumber RA+1 sv2
					// +inline-call reg.SetNumber RA+2 v
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TFORPREP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			// swap the control (RA+3) and closing (RA+2) variables; the
			// closing value's to-be-closed registration is U5 work (the
			// compiler passes only nil closings until then)
			tmp := reg.Get(RA + 3)
			// +inline-call reg.Set RA+3 reg.Get(RA+2)
			// +inline-call reg.Set RA+2 tmp
			cf.Pc += Bx
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TFORCALL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			C := int(inst>>24) & 0xff //GETC
			// +inline-call reg.SetTop RA+5
			// +inline-call reg.Set RA+5 reg.Get(RA+3)
			// +inline-call reg.Set RA+4 reg.Get(RA+1)
			// +inline-call reg.Set RA+3 reg.Get(RA)
			L.callR(2, C, RA+3)
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TFORLOOP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			if value := reg.Get(RA + 3); value != LNil {
				cf.Pc -= Bx
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETLIST
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			vB := int(inst>>16) & 0x3f   //GETVB
			vC := int(inst>>22) & 0x3ff  //GETVC
			k := int(inst>>15) & 1      //GETK
			last := vC
			if k != 0 {
				// the base-index high word lives in the FOLLOWING EXTRAARG
				// word (stock lvm.c: last += GETARG_Ax(*pc) * (MAXARG_vC+1))
				Ax := int(cf.Fn.Proto.Code[cf.Pc]>>7) & 0x1ffffff //GETAX (EXTRAARG)
				last += Ax * (opMaxArgsvC + 1)
				cf.Pc++
			}
			nelem := vB
			if vB == 0 {
				nelem = reg.Top() - RA - 1
			}
			last += nelem
			table := reg.Get(RA).(*LTable)
			for i := 1; i <= nelem; i++ {
				table.RawSetInt(last-nelem+i, reg.Get(RA+i))
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CLOSURE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			proto := cf.Fn.Proto.FunctionPrototypes[Bx]
			closure := newLFunctionL(proto, cf.Fn.Env, len(proto.Upvalues))
			// +inline-call reg.Set RA closure
			for i := 0; i < len(proto.Upvalues); i++ {
				uvd := &proto.Upvalues[i]
				if uvd.InStack {
					closure.Upvalues[i] = L.findUpvalue(lbase + int(uvd.Idx))
				} else {
					closure.Upvalues[i] = cf.Fn.Upvalues[uvd.Idx]
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_VARARG
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			C := int(inst>>24) & 0xff //GETC
			nparams := int(cf.Fn.Proto.NumParameters)
			nvarargs := cf.NArgs - nparams
			if nvarargs < 0 {
				nvarargs = 0
			}
			nwant := C - 1
			if C == 0 {
				nwant = nvarargs
			}
			// +inline-call reg.CopyRange RA cf.Base+nparams+1 cf.LocalBase nwant
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETVARG
			// 5.5 vararg parameters-as-tables; the parser does not
			// produce this opcode yet (U5)
			L.RaiseError("OP_GETVARG not supported until U5")
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_ERRNNIL
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>7) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst>>15) & 0x1ffff //GETBX
			if v := L.reg.Get(RA); v != LNil {
				name := "?"
				if Bx > 0 && Bx-1 < len(cf.Fn.Proto.Constants) {
					if s, ok := cf.Fn.Proto.Constants[Bx-1].(LString); ok {
						name = string(s)
					}
				}
				L.RaiseError("attempt to assign to global '%s' (global is not declared)", name)
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_VARARGPREP
			// thin marker: the frame shuffle happens at call time (the
			// 5.4+ vararg model is U5; ledger row)
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_EXTRAARG
			// consumed by the preceding instruction; never dispatched
			return 0
		},
	}
}


// condJump executes the jump instruction that follows a test/compare
// (docondjump): the jump is taken when cond == k, skipped otherwise.
func condJump(cf *callFrame, cond bool, k int) {
	if cond != (k == 1) {
		cf.Pc++
	} else {
		ni := cf.Fn.Proto.Code[cf.Pc]
		cf.Pc += int(ni>>7&0x1ffffff) - opOffsetSJ + 1
	}
}

// leValues is the OP_LE comparison core (shared with the immediate
// order family's LEI/GEI metamethod path).
func leValues(L *LState, lhs, rhs LValue) bool {
	if res, ok := numLE(lhs, rhs); ok {
		return res
	}
	if isNumberValue(lhs) || isNumberValue(rhs) {
		orderError(L, lhs, rhs)
	}
	if lhs.Type() != rhs.Type() {
		orderError(L, lhs, rhs)
	}
	switch lhs.Type() {
	case LTString:
		return strCmp(string(lhs.(LString)), string(rhs.(LString))) <= 0
	default:
		switch objectRational(L, lhs, rhs, "__le") {
		case 1:
			return true
		case 0:
			return false
		default:
			return !objectRationalWithError(L, rhs, lhs, "__lt")
		}
	}
}

// tmToArithOp maps an MMBIN C operand (a tag-method number) back to its
// arith opcode (the reverse of arithTM's ADD..SHR range).
func tmToArithOp(tm int) int {
	return tm - tmAdd + OP_ADD
}

// opArith: the register-form arith/bitwise family (OP_ADD..OP_SHR). The
// numeric fast path writes the result and skips the MMBIN slot; the
// erroring numeric ops raise here (same text and position the MMBIN
// would produce); non-numeric operands fall into the MMBIN slot.
func opArith(L *LState, inst uint32, baseframe *callFrame) int { //OP_ADD..OP_SHR
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	opcode := int(inst & 0x7f) //GETOPCODE
	B := int(inst>>16) & 0xff  //GETB
	C := int(inst>>24) & 0xff  //GETC
	lhs := reg.Get(lbase + B)
	rhs := reg.Get(lbase + C)
	if v, errText, res := arithCompute(opcode, lhs, rhs); res == arithDone {
		if iv, isInt := v.(LInteger); isInt {
			// +inline-call reg.SetInteger RA iv
		} else {
			fv := v.(LNumber)
			// +inline-call reg.SetNumber RA fv
		}
		cf.Pc++
	} else if res == arithRaise {
		raiseArithError(L, errText, lhs, rhs, B, C)
	}
	return 0
}

// opBitwise: the register-form bitwise ops share opArith's body (the
// subtype dispatch lives in arithCompute).
func opBitwise(L *LState, inst uint32, baseframe *callFrame) int { //OP_BAND..OP_SHR
	return opArith(L, inst, baseframe)
}

// opArithI: OP_ADDI — R[A] := R[B] + sC (int wrap / float promote).
func opArithI(L *LState, inst uint32, baseframe *callFrame) int { //OP_ADDI
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	B := int(inst>>16) & 0xff  //GETB
	C := int(inst>>24) & 0xff //GETC
	imm := int64(sC2int(C))
	lhs := reg.Get(lbase + B)
	if iv, ok := lhs.(LInteger); ok {
		rv := iv + LInteger(imm)
		// +inline-call reg.SetInteger RA rv
		cf.Pc++
	} else if fv, ok := lhs.(LNumber); ok {
		rv := fv + LNumber(float64(imm))
		// +inline-call reg.SetNumber RA rv
		cf.Pc++
	}
	return 0
}

// opArithK: the ADDK..IDIVK family — the constant rides the pool.
func opArithK(L *LState, inst uint32, baseframe *callFrame) int { //OP_ADDK..OP_IDIVK
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	opcode := int(inst&0x7f) - OP_ADDK + OP_ADD //GETOPCODE
	B := int(inst>>16) & 0xff                   //GETB
	C := int(inst>>24) & 0xff                   //GETC
	lhs := reg.Get(lbase + B)
	rhs := cf.Fn.Proto.Constants[C]
	if v, errText, res := arithCompute(opcode, lhs, rhs); res == arithDone {
		if iv, isInt := v.(LInteger); isInt {
			// +inline-call reg.SetInteger RA iv
		} else {
			fv := v.(LNumber)
			// +inline-call reg.SetNumber RA fv
		}
		cf.Pc++
	} else if res == arithRaise {
		// the constant operand carries no register (stock instack)
		raiseArithError(L, errText, lhs, rhs, B, -1)
	}
	return 0
}

// opBitwiseK: BANDK/BORK/BXORK — the constant is an integer in the pool.
func opBitwiseK(L *LState, inst uint32, baseframe *callFrame) int { //OP_BANDK..OP_BXORK
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	opcode := int(inst&0x7f) - OP_BANDK + OP_BAND //GETOPCODE
	B := int(inst>>16) & 0xff                     //GETB
	C := int(inst>>24) & 0xff                     //GETC
	lhs := reg.Get(lbase + B)
	i2 := int64(cf.Fn.Proto.Constants[C].(LInteger))
	if i1, ok := toIntBitwise(lhs); ok {
		rv := LInteger(bitwiseInt(opcode, i1, i2))
		// +inline-call reg.SetInteger RA rv
		cf.Pc++
	}
	return 0
}

// opSHLI: R[A] := sC << R[B]; opSHRI: R[A] := R[B] >> sC.
func opSHLI(L *LState, inst uint32, baseframe *callFrame) int { //OP_SHLI
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	B := int(inst>>16) & 0xff  //GETB
	C := int(inst>>24) & 0xff //GETC
	imm := int64(sC2int(C))
	if ib, ok := toIntBitwise(reg.Get(lbase + B)); ok {
		rv := LInteger(shiftInt(imm, ib))
		// +inline-call reg.SetInteger RA rv
		cf.Pc++
	}
	return 0
}

func opSHRI(L *LState, inst uint32, baseframe *callFrame) int { //OP_SHRI
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	B := int(inst>>16) & 0xff  //GETB
	C := int(inst>>24) & 0xff //GETC
	imm := int64(sC2int(C))
	if ib, ok := toIntBitwise(reg.Get(lbase + B)); ok {
		rv := LInteger(shiftInt(ib, -imm))
		// +inline-call reg.SetInteger RA rv
		cf.Pc++
	}
	return 0
}

// The MMBIN family: the arith fast path fell through — call the
// metamethod over the original operands (stock luaT_trybinTM et al.,
// with the fork's 5.3-style VM coercion pending U3, row 58).
func opMMBIN(L *LState, inst uint32, baseframe *callFrame) int { //OP_MMBIN
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	C := int(inst>>24) & 0xff //GETC
	pi := cf.Fn.Proto.Code[cf.Pc-2]
	dest := lbase + opGetArgA(pi)
	p1 := reg.Get(lbase + A)
	p2 := reg.Get(lbase + B)
	v := objectArith(L, tmToArithOp(C), p1, p2, A, B)
	// +inline-call reg.Set dest v
	return 0
}

func opMMBINI(L *LState, inst uint32, baseframe *callFrame) int { //OP_MMBINI
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	C := int(inst>>24) & 0xff //GETC
	k := int(inst>>15) & 1    //GETK
	pi := cf.Fn.Proto.Code[cf.Pc-2]
	dest := lbase + opGetArgA(pi)
	p1 := reg.Get(lbase + A)
	p2 := LValue(LInteger(int64(sC2int(B))))
	r1, r2 := A, -1 // the immediate carries no register (stock instack)
	if k != 0 { // flipped: the immediate was the first operand
		p1, p2 = p2, p1
		r1, r2 = r2, r1
	}
	v := objectArith(L, tmToArithOp(C), p1, p2, r1, r2)
	// +inline-call reg.Set dest v
	return 0
}

func opMMBINK(L *LState, inst uint32, baseframe *callFrame) int { //OP_MMBINK
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	C := int(inst>>24) & 0xff //GETC
	k := int(inst>>15) & 1    //GETK
	pi := cf.Fn.Proto.Code[cf.Pc-2]
	dest := lbase + opGetArgA(pi)
	p1 := reg.Get(lbase + A)
	p2 := cf.Fn.Proto.Constants[B]
	r1, r2 := A, -1 // the constant carries no register (stock instack)
	if k != 0 { // flipped: the constant was the first operand
		p1, p2 = p2, p1
		r1, r2 = r2, r1
	}
	v := objectArith(L, tmToArithOp(C), p1, p2, r1, r2)
	// +inline-call reg.Set dest v
	return 0
}

func opEQTest(L *LState, inst uint32, baseframe *callFrame) int { //OP_EQ
	cf := L.currentFrame
	reg := L.reg
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	k := int(inst>>15) & 1    //GETK
	ret := equals(L, reg.Get(lbase+A), reg.Get(lbase+B), false)
	condJump(cf, ret, k)
	return 0
}

func opLTTest(L *LState, inst uint32, baseframe *callFrame) int { //OP_LT
	cf := L.currentFrame
	reg := L.reg
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	k := int(inst>>15) & 1    //GETK
	ret := lessThan(L, reg.Get(lbase+A), reg.Get(lbase+B))
	condJump(cf, ret, k)
	return 0
}

func opLETest(L *LState, inst uint32, baseframe *callFrame) int { //OP_LE
	cf := L.currentFrame
	reg := L.reg
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	k := int(inst>>15) & 1    //GETK
	ret := leValues(L, reg.Get(lbase+A), reg.Get(lbase+B))
	condJump(cf, ret, k)
	return 0
}

// opEQK: raw equality against a constant (stock luaV_rawequalobj —
// basic types carry no __eq here).
func opEQKTest(L *LState, inst uint32, baseframe *callFrame) int { //OP_EQK
	cf := L.currentFrame
	reg := L.reg
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	k := int(inst>>15) & 1    //GETK
	ret := equals(L, reg.Get(lbase+A), cf.Fn.Proto.Constants[B], true)
	condJump(cf, ret, k)
	return 0
}

// opEQI: numeric equality against an sC immediate; other types are
// never equal to a number.
func opEQITest(L *LState, inst uint32, baseframe *callFrame) int { //OP_EQI
	cf := L.currentFrame
	reg := L.reg
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	k := int(inst>>15) & 1    //GETK
	im := int64(sC2int(B))
	var ret bool
	switch v := reg.Get(lbase + A).(type) {
	case LInteger:
		ret = int64(v) == im
	case LNumber:
		ret = float64(v) == float64(im)
	}
	condJump(cf, ret, k)
	return 0
}

// opOrderI: LTI/LEI/GTI/GEI — integer fast path, float compare, else
// the order metamethod over the register value and an aux immediate
// (C marks a float-sourced immediate; GTI/GEI dispatch flipped, stock
// luaT_callorderiTM).
func opOrderI(L *LState, inst uint32, baseframe *callFrame) int { //OP_LTI..OP_GEI
	cf := L.currentFrame
	reg := L.reg
	lbase := cf.LocalBase
	op := int(inst & 0x7f) //GETOPCODE
	A := int(inst>>7) & 0xff //GETA
	B := int(inst>>16) & 0xff //GETB
	C := int(inst>>24) & 0xff //GETC
	k := int(inst>>15) & 1    //GETK
	ra := reg.Get(lbase + A)
	im := int64(sC2int(B))
	var cond bool
	switch v := ra.(type) {
	case LInteger:
		i := int64(v)
		switch op {
		case OP_LTI:
			cond = i < im
		case OP_LEI:
			cond = i <= im
		case OP_GTI:
			cond = i > im
		case OP_GEI:
			cond = i >= im
		}
	case LNumber:
		f := float64(v)
		fim := float64(im)
		switch op {
		case OP_LTI:
			cond = f < fim
		case OP_LEI:
			cond = f <= fim
		case OP_GTI:
			cond = f > fim
		case OP_GEI:
			cond = f >= fim
		}
	default:
		var aux LValue
		if C != 0 {
			aux = LNumber(float64(im))
		} else {
			aux = LInteger(im)
		}
		if op == OP_GTI { // (a > imm) <=> __lt(imm, a)
			cond = lessThan(L, aux, ra)
		} else if op == OP_GEI { // (a >= imm) <=> __le(imm, a)
			cond = leValues(L, aux, ra)
		} else if op == OP_LTI {
			cond = lessThan(L, ra, aux)
		} else {
			cond = leValues(L, ra, aux)
		}
	}
	condJump(cf, cond, k)
	return 0
}

// doReturn is the RETURN/RETURN0/RETURN1 core (the old OP_RETURN body).
func doReturn(L *LState, cf *callFrame, baseframe *callFrame, RA, nres, b int) int {
	reg := L.reg
	lbase := cf.LocalBase
	// +inline-call L.closeUpvalues lbase
	// U4: the frame's to-be-closed variables close at return too (stock
	// RETURN k / luaF_close(base, CLOSEKTOP) — unconditional here: the
	// list is empty when the function declared no <close>)
	L.closeTBC(lbase)
	n := cf.NRet
	if cf.NRet == MultRet {
		n = nres
	}

	if L.Parent != nil && L.stack.Sp() == 1 {
		// +inline-call copyReturnValues L reg.Top() RA n b
		switchToParentThread(L, n, false, true)
		return 1
	}
	islast := baseframe == L.stack.Pop() || L.stack.IsEmpty()
	// +inline-call copyReturnValues L cf.ReturnBase RA n b
	L.currentFrame = L.stack.Last()
	if islast || L.currentFrame == nil || L.currentFrame.Fn.IsG {
		return 1
	}
	return 0
}

func opReturn(L *LState, inst uint32, baseframe *callFrame) int { //OP_RETURN
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	RA := lbase + A
	B := int(inst>>16) & 0xff //GETB
	nret := B - 1
	if B == 0 {
		nret = reg.Top() - RA
	}
	return doReturn(L, cf, baseframe, RA, nret, B)
}

func opReturn0(L *LState, inst uint32, baseframe *callFrame) int { //OP_RETURN0
	cf := L.currentFrame
	return doReturn(L, cf, baseframe, cf.LocalBase, 0, 1)
}

func opReturn1(L *LState, inst uint32, baseframe *callFrame) int { //OP_RETURN1
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>7) & 0xff //GETA
	return doReturn(L, cf, baseframe, lbase+A, 1, 2)
}

// coerceStringNumber converts a numeric string to its number value with
// the syntactic subtype preserved (parseNumber); anything else fails.
func coerceStringNumber(v LValue) (LValue, bool) {
	if str, ok := v.(LString); ok {
		if num, err := parseNumber(string(str)); err == nil {
			return num, true
		}
	}
	return nil, false
}

// forNumber coerces one numeric-for operand for the float flavor
// (tonumber: numbers pass, numeric strings convert); raises stock's
// "bad 'for'" error otherwise.
func forNumber(L *LState, v LValue, what string) (LNumber, bool) {
	if n, ok := arithNumber(v); ok {
		return n, true
	}
	if num, ok := coerceStringNumber(v); ok {
		if n, ok2 := arithNumber(num); ok2 {
			return n, true
		}
	}
	L.RaiseError("bad 'for' %s (number expected, got %s)", what, L.objTypeName(v))
	return 0, false
}

// forLimitInt ports stock forLimit: the integer loop's limit — exact
// conversion per step direction (floor ascending, ceil descending), a
// float beyond int64 clamps into range, and a limit smaller/larger than
// every integer skips the loop outright.
func forLimitInt(L *LState, init int64, lim LValue, step int64) (int64, bool) {
	var p int64
	if iv, ok := lim.(LInteger); ok {
		p = int64(iv)
	} else if fv, ok := arithNumber(lim); ok {
		var ok2 bool
		if step < 0 {
			p, ok2 = floatToCeilInt(float64(fv))
		} else {
			p, ok2 = floatToFloorInt(float64(fv))
		}
		if !ok2 {
			if float64(fv) > 0 { // too large
				if step < 0 {
					return 0, true
				}
				p = math.MaxInt64
			} else { // less than min integer
				if step > 0 {
					return 0, true
				}
				p = math.MinInt64
			}
		}
	} else if num, ok := coerceStringNumber(lim); ok {
		return forLimitInt(L, init, num, step)
	} else {
		L.RaiseError("bad 'for' limit (number expected, got %s)", L.objTypeName(lim))
	}
	if step > 0 {
		return p, init > p
	}
	return p, init < p
}

// arithNumber returns the float view of either number subtype (the
// tonumberns macro: no string coercion).
func arithNumber(v LValue) (LNumber, bool) {
	switch n := v.(type) {
	case LNumber:
		return n, true
	case LInteger:
		return LNumber(float64(n)), true
	}
	return 0, false
}

func luaModulo(lhs, rhs LNumber) LNumber {
	flhs := float64(lhs)
	frhs := float64(rhs)
	v := math.Mod(flhs, frhs)
	if frhs > 0 && v < 0 || frhs < 0 && v > 0 {
		v += frhs
	}
	return LNumber(v)
}

// objectArith is the MMBIN-slot slow path (luaT_trybinTM); reg1/reg2
// are the operands' frame-relative registers for the row-62 varinfo
// hints (-1 for the MMBINI/MMBINK aux values, which stock's instack
// cannot see either).
func objectArith(L *LState, opcode int, lhs, rhs LValue, reg1, reg2 int) LValue {
	event := arithEvent(opcode)
	if op := L.metaOp2(lhs, rhs, event); op != LNil {
		// covers user metamethods AND the string metatable's arith
		// metamethods (U3c, row 58) — string coercion lives there, not
		// in the VM. U4d: stock luaT_trybinTM calls whatever metamethod
		// it found — a non-callable one surfaces as the call error
		// "metamethod '<ev>'" (errors.lua's call-naming battery), so the
		// old *LFunction guard is gone
		L.reg.Push(op)
		L.reg.Push(lhs)
		L.reg.Push(rhs)
		L.Call(2, 1)
		return L.reg.Pop()
	}
	if v, errText, res := arithCompute(opcode, lhs, rhs); res == arithDone {
		return v
	} else if res == arithRaise {
		raiseArithError(L, errText, lhs, rhs, reg1, reg2)
	}
	// stock luaT_trybinTM failure: luaG_opinterror picks the non-number
	// operand (both non-numbers reports the first) — varinfo names it
	what := "arithmetic"
	if isBitwiseOp(opcode) {
		what = "bitwise operation"
	}
	bad, badreg := lhs, reg1
	if isNumberValue(lhs) {
		bad, badreg = rhs, reg2
	}
	L.RaiseError("attempt to perform %s on a %s value%s", what, L.objTypeName(bad), frameRegVarinfo(L, badreg))

	return LNil
}

// raiseArithError: the numeric arithRaise texts. luaG_tointerror
// ("number has no integer representation") appends the varinfo of the
// operand without an exact integer value (p1 unless it converts); the
// divide-by-zero / 'n%%0' texts are bare luaG_runerror raises.
func raiseArithError(L *LState, errText string, lhs, rhs LValue, reg1, reg2 int) {
	if errText == "number has no integer representation" {
		badreg := reg2
		if _, ok := toIntBitwise(lhs); !ok {
			badreg = reg1
		}
		L.RaiseError("number%s has no integer representation", frameRegVarinfo(L, badreg))
	}
	L.RaiseError("%s", errText)
}

// arithEvent maps an arith opcode to its metamethod name.
func arithEvent(opcode int) string {
	switch opcode {
	case OP_ADD:
		return "__add"
	case OP_SUB:
		return "__sub"
	case OP_MUL:
		return "__mul"
	case OP_DIV:
		return "__div"
	case OP_MOD:
		return "__mod"
	case OP_POW:
		return "__pow"
	case OP_IDIV:
		return "__idiv"
	case OP_BAND:
		return "__band"
	case OP_BOR:
		return "__bor"
	case OP_BXOR:
		return "__bxor"
	case OP_SHL:
		return "__shl"
	case OP_SHR:
		return "__shr"
	}
	return "__arith"
}

func isBitwiseOp(opcode int) bool {
	switch opcode {
	case OP_BAND, OP_BOR, OP_BXOR, OP_SHL, OP_SHR:
		return true
	}
	return false
}

type arithOutcome int

const (
	arithDone  arithOutcome = iota // numeric result staged in v
	arithRaise                     // numeric operands, erroring op (text in err)
	arithSlow                      // non-numeric pair: metamethods / coercion
)

// arithCompute is the numeric core of every binary arith/bitwise opcode —
// the executable-spec twin of stock's op_arith/op_arithf/op_bitwise macros.
// int/int keeps the integer subtype with 5.3 wrap-around semantics; any
// float operand promotes to float ('/' and '^' are always float); bitwise
// ops convert through luaV_tointegerns F2Ieq (strings are NOT accepted
// here — they take the slow path).
func arithCompute(opcode int, lhs, rhs LValue) (v LValue, err string, res arithOutcome) {
	li, liOK := lhs.(LInteger)
	ri, riOK := rhs.(LInteger)
	lf, lfOK := lhs.(LNumber)
	rf, rfOK := rhs.(LNumber)
	lNum := liOK || lfOK
	rNum := riOK || rfOK
	switch opcode {
	case OP_UNM: // luaO_arith's unary case (both slots hold the operand)
		if lNum && rNum {
			if liOK {
				return LInteger(-int64(li)), "", arithDone
			}
			return LNumber(-float64(lf)), "", arithDone
		}
	case OP_DIV, OP_POW: // float-only ops (int operands promote)
		if lNum && rNum {
			return LNumber(floatArith(opcode, asFloat(li, lf, liOK), asFloat(ri, rf, riOK))), "", arithDone
		}
	case OP_ADD, OP_SUB, OP_MUL, OP_MOD, OP_IDIV:
		if liOK && riOK {
			if iv, ok := intArith(opcode, int64(li), int64(ri)); ok {
				return LInteger(iv), "", arithDone
			}
			return nil, intArithError(opcode), arithRaise
		}
		if lNum && rNum {
			return LNumber(floatArith(opcode, asFloat(li, lf, liOK), asFloat(ri, rf, riOK))), "", arithDone
		}
	case OP_BAND, OP_BOR, OP_BXOR, OP_SHL, OP_SHR:
		if lNum && rNum {
			i1, ok1 := toIntBitwise(lhs)
			i2, ok2 := toIntBitwise(rhs)
			if !ok1 || !ok2 {
				return nil, "number has no integer representation", arithRaise
			}
			return LInteger(bitwiseInt(opcode, i1, i2)), "", arithDone
		}
	}
	return nil, "", arithSlow
}

func asFloat(iv LInteger, fv LNumber, isInt bool) float64 {
	if isInt {
		return float64(iv)
	}
	return float64(fv)
}

// intArithError gives stock's message for the failing integer ops
// (luaV_idiv / luaV_mod in 5.5.1 — note idiv's text differs from 5.4).
func intArithError(opcode int) string {
	if opcode == OP_MOD {
		return "attempt to perform 'n%0'"
	}
	return "attempt to divide by zero"
}

// intArith: integer ops with 5.3 semantics — +,-,* wrap; // and % follow
// the floor with the -1/0 special cases (ports of luaV_idiv/luaV_mod).
// ok=false only for the erroring divisor cases.
func intArith(op int, m, n int64) (int64, bool) {
	switch op {
	case OP_ADD:
		return m + n, true
	case OP_SUB:
		return m - n, true
	case OP_MUL:
		return m * n, true
	case OP_MOD:
		if uint64(n)+1 <= 1 { // special cases: -1 or 0
			if n == 0 {
				return 0, false
			}
			return 0, true // m % -1 == 0, avoids minint overflow
		}
		r := m % n
		if r != 0 && (r^n) < 0 { // 'm/n' would be non-integer negative?
			r += n
		}
		return r, true
	case OP_IDIV:
		if uint64(n)+1 <= 1 {
			if n == 0 {
				return 0, false
			}
			return 0 - m, true // n == -1, avoids minint overflow
		}
		q := m / n
		if (m^n) < 0 && m%n != 0 { // negative non-integer quotient?
			q--
		}
		return q, true
	}
	panic("should not reach here")
}

// floatArith: float paths. MOD follows the fmod-with-sign-fix luai_nummod;
// IDIV is floor(a/b) (luai_numidiv); POW squares exactly when the exponent
// is 2, like luai_numpow.
func floatArith(op int, m, n float64) float64 {
	switch op {
	case OP_ADD:
		return m + n
	case OP_SUB:
		return m - n
	case OP_MUL:
		return m * n
	case OP_DIV:
		return m / n
	case OP_MOD:
		return float64(luaModulo(LNumber(m), LNumber(n)))
	case OP_IDIV:
		return math.Floor(m / n)
	case OP_POW:
		if n == 2 {
			return m * m
		}
		return math.Pow(m, n)
	}
	panic("should not reach here")
}

// toIntBitwise is luaV_tointegerns with F2Ieq: integers pass, floats must
// have an exact integer value (strings and everything else fail).
func toIntBitwise(v LValue) (int64, bool) {
	switch n := v.(type) {
	case LInteger:
		return int64(n), true
	case LNumber:
		i, ok := floatToIntExact(float64(n))
		return i, ok
	}
	return 0, false
}

// shiftInt is stock luaV_shiftl: a negative count shifts the other way,
// and any count outside (-64,64) yields zero. Right shifts are LOGICAL —
// stock's intop casts through lua_Unsigned before shifting.
func shiftInt(x, y int64) int64 {
	if y < 0 {
		if y <= -64 {
			return 0
		}
		return int64(uint64(x) >> uint64(-y))
	}
	if y >= 64 {
		return 0
	}
	return x << y
}

func bitwiseInt(op int, x, y int64) int64 {
	switch op {
	case OP_BAND:
		return x & y
	case OP_BOR:
		return x | y
	case OP_BXOR:
		return x ^ y
	case OP_SHL:
		return shiftInt(x, y)
	case OP_SHR:
		return shiftInt(x, -y)
	}
	panic("should not reach here")
}

// stringConcat is the OP_CONCAT core (luaV_concat); cf names the
// frame for the row-62 varinfo hint (nil from host-API Concat, where
// stock's varinfo sees a C frame and yields no name).
func stringConcat(L *LState, total, last int, cf *callFrame) LValue {
	rhs := L.reg.Get(last)
	total--
	for i := last - 1; total > 0; {
		lhs := L.reg.Get(i)
		if !(LVCanConvToString(lhs) && LVCanConvToString(rhs)) {
			// U4d: stock luaT_tryconcatTM calls whatever metamethod was
			// found — a non-callable one surfaces as the call error
			// "metamethod 'concat'"
			op := L.metaOp2(lhs, rhs, "__concat")
			if op != LNil {
				L.reg.Push(op)
				L.reg.Push(lhs)
				L.reg.Push(rhs)
				L.Call(2, 1)
				rhs = L.reg.Pop()
				total--
				i--
			} else {
				// U2f re-pin: luaG_concaterror — if the first operand is
				// string/number-convertible the second is the offender,
				// else the first is (U3 row 62: varinfo names it)
				bad, badreg := lhs, i
				if LVCanConvToString(lhs) {
					bad, badreg = rhs, last
				}
				hint := ""
				if cf != nil {
					hint = protoVarinfo(L, cf, badreg-cf.LocalBase)
				}
				L.RaiseError("attempt to concatenate a %s value%s", L.objTypeName(bad), hint)
				return LNil
			}
		} else {
			buf := make([]string, total+1)
			buf[total] = LVAsString(rhs)
			for total > 0 {
				lhs = L.reg.Get(i)
				if !LVCanConvToString(lhs) {
					break
				}
				buf[total-1] = LVAsString(lhs)
				i--
				total--
			}
			rhs = LString(strings.Join(buf, ""))
		}
	}
	return rhs
}

// The four mixed int/float order comparisons below are ports of stock
// 5.5 lvm.c LTintfloat/LEintfloat/LTfloatint/LEfloatint: compare as floats
// when the integer survives the round trip (|i| <= 2^53, l_intfitsf),
// otherwise through the exact floor/ceil of the float operand.

func ltIntFloat(i int64, f float64) bool {
	if intFitsFloat(i) {
		return float64(i) < f
	}
	if fi, ok := floatToCeilInt(f); ok { // i < f <=> i < ceil(f)
		return i < fi
	}
	return f > 0 // 'f' is greater than all integers, or NaN
}

func leIntFloat(i int64, f float64) bool {
	if intFitsFloat(i) {
		return float64(i) <= f
	}
	if fi, ok := floatToFloorInt(f); ok { // i <= f <=> i <= floor(f)
		return i <= fi
	}
	return f > 0
}

func ltFloatInt(f float64, i int64) bool {
	if intFitsFloat(i) {
		return f < float64(i)
	}
	if fi, ok := floatToFloorInt(f); ok { // f < i <=> floor(f) < i
		return fi < i
	}
	return f < 0 // 'f' is less than all integers, or NaN
}

func leFloatInt(f float64, i int64) bool {
	if intFitsFloat(i) {
		return f <= float64(i)
	}
	if fi, ok := floatToCeilInt(f); ok { // f <= i <=> ceil(f) <= i
		return fi <= i
	}
	return f < 0
}

// floatToFloorInt / floatToCeilInt are luaV_flttointeger with F2Ifloor /
// F2Iceil: they fail on NaN/±Inf and on values whose floored/ceiled value
// falls outside int64; anything else converts.
func floatToFloorInt(f float64) (int64, bool) {
	if f != f || f >= 9223372036854775808.0 || f < -9223372036854775808.0 {
		return 0, false
	}
	return int64(math.Floor(f)), true
}

func floatToCeilInt(f float64) (int64, bool) {
	if f != f || f >= 9223372036854775808.0 || f < -9223372036854775808.0 {
		return 0, false
	}
	return int64(math.Ceil(f)), true
}

// numLT/numLE return (result, true) when both operands are numbers —
// either subtype — and (false, false) for any other pair.
func numLT(lhs, rhs LValue) (bool, bool) {
	li, lok := lhs.(LInteger)
	ri, rok := rhs.(LInteger)
	lf, lokf := lhs.(LNumber)
	rf, rokf := rhs.(LNumber)
	switch {
	case lok && rok:
		return li < ri, true
	case lok && rokf:
		return ltIntFloat(int64(li), float64(rf)), true
	case lokf && rok:
		return ltFloatInt(float64(lf), int64(ri)), true
	case lokf && rokf:
		return lf < rf, true
	}
	return false, false
}

func numLE(lhs, rhs LValue) (bool, bool) {
	li, lok := lhs.(LInteger)
	ri, rok := rhs.(LInteger)
	lf, lokf := lhs.(LNumber)
	rf, rokf := rhs.(LNumber)
	switch {
	case lok && rok:
		return li <= ri, true
	case lok && rokf:
		return leIntFloat(int64(li), float64(rf)), true
	case lokf && rok:
		return leFloatInt(float64(lf), int64(ri)), true
	case lokf && rokf:
		return lf <= rf, true
	}
	return false, false
}

// equalsNumber compares two numbers — either subtype — for exact equality
// (stock luaV_rawequalobj's int/float case: 2 == 2.0).
func equalsNumber(lhs, rhs LValue) bool {
	li, lok := lhs.(LInteger)
	ri, rok := rhs.(LInteger)
	if lok && rok {
		return li == ri
	}
	if lok {
		return eqIntFloat(int64(li), float64(rhs.(LNumber)))
	}
	if rok {
		return eqIntFloat(int64(ri), float64(lhs.(LNumber)))
	}
	return lhs.(LNumber) == rhs.(LNumber)
}

func eqIntFloat(i int64, f float64) bool {
	if intFitsFloat(i) {
		return f == float64(i)
	}
	fi, ok := floatToIntExact(f)
	return ok && fi == i
}

// orderError is luaG_ordererror (U2f re-pin): same types name them
// twice, mixed types name each once. luaG_runerror semantics (U3b):
// position from the CURRENT frame — the running Lua frame for OP_LT-
// family compares, and NO position when the compare comes from a G
// function via lua_compare (math.min("2", 1) raises bare).
func orderError(L *LState, lhs, rhs LValue) {
	t1, t2 := L.objTypeName(lhs), L.objTypeName(rhs)
	if t1 == t2 {
		L.raiseErrorAt(0, "attempt to compare two %s values", t1)
	} else {
		L.raiseErrorAt(0, "attempt to compare %s with %s", t1, t2)
	}
}

func lessThan(L *LState, lhs, rhs LValue) bool {
	// optimization for numbers
	if ret, ok := numLT(lhs, rhs); ok {
		return ret
	}
	if isNumberValue(lhs) || isNumberValue(rhs) {
		orderError(L, lhs, rhs)
	}
	if lhs.Type() != rhs.Type() {
		orderError(L, lhs, rhs)
		return false
	}
	ret := false
	switch lhs.Type() {
	case LTString:
		ret = strCmp(string(lhs.(LString)), string(rhs.(LString))) < 0
	default:
		ret = objectRationalWithError(L, lhs, rhs, "__lt")
	}
	return ret
}

func equals(L *LState, lhs, rhs LValue, raw bool) bool {
	lt := lhs.Type()
	if lt != rhs.Type() {
		// the two number subtypes compare numerically across types
		if isNumberValue(lhs) && isNumberValue(rhs) {
			return equalsNumber(lhs, rhs)
		}
		return false
	}

	ret := false
	switch lt {
	case LTNil:
		ret = true
	case LTNumber, LTInteger:
		ret = equalsNumber(lhs, rhs)
	case LTBool:
		ret = bool(lhs.(LBool)) == bool(rhs.(LBool))
	case LTString:
		ret = string(lhs.(LString)) == string(rhs.(LString))
	case LTUserData, LTTable:
		if lhs == rhs {
			ret = true
		} else if !raw {
			switch objectRational(L, lhs, rhs, "__eq") {
			case 1:
				ret = true
			default:
				ret = false
			}
		}
	default:
		ret = lhs == rhs
	}
	return ret
}

func objectRationalWithError(L *LState, lhs, rhs LValue, event string) bool {
	switch objectRational(L, lhs, rhs, event) {
	case 1:
		return true
	case 0:
		return false
	}
	orderError(L, lhs, rhs)
	return false
}

// objectRational is the metamethod comparison core (stock callbinTM's
// first-found rule, shared by luaT_callorderTM and luaV_equalobj's __eq
// arm): the FIRST operand's metamethod, else the second's — called
// whatever it is (a non-callable one surfaces as the call error
// "metamethod '<ev>'", U4d) — with the boolean result mapped to 1/0;
// -1 when neither operand has the event. The old fork guard required
// the SAME function on both operands (a gopherlua artifact — stock 5.1
// and 5.5 both call the first one found).
func objectRational(L *LState, lhs, rhs LValue, event string) int {
	if op := L.metaOp2(lhs, rhs, event); op != LNil {
		L.reg.Push(op)
		L.reg.Push(lhs)
		L.reg.Push(rhs)
		L.Call(2, 1)
		if LVAsBool(L.reg.Pop()) {
			return 1
		}
		return 0
	}
	return -1
}
