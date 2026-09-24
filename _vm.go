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
		if jumpTable[int(inst>>26)](L, inst, baseframe) == 1 {
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
			if jumpTable[int(inst>>26)](L, inst, baseframe) == 1 {
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

	// +inline-call L.reg.CopyRange frame.ReturnBase L.reg.Top()-gfnret -1 wantret
	L.stack.Pop()
	L.currentFrame = L.stack.Last()
	return false
}

func threadRun(L *LState) {
	if L.stack.IsEmpty() {
		return
	}

	defer func() {
		if rcv := recover(); rcv != nil {
			var lv LValue
			if v, ok := rcv.(*ApiError); ok {
				lv = v.Object
			} else {
				lv = LString(fmt.Sprint(rcv))
			}
			if parent := L.Parent; parent != nil {
				if L.wrapped {
					L.Push(lv)
					parent.Panic(L)
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
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			v := reg.Get(lbase + B)
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_MOVEN
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			v := reg.Get(lbase + B)
			// +inline-call reg.Set lbase+A v
			code := cf.Fn.Proto.Code
			pc := cf.Pc
			for i := 0; i < C; i++ {
				inst = code[pc]
				pc++
				A = int(inst>>18) & 0xff //GETA
				B = int(inst & 0x1ff)    //GETB
				v := reg.Get(lbase + B)
				// +inline-call reg.Set lbase+A v
			}
			cf.Pc = pc
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADK
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst & 0x3ffff) //GETBX
			v := cf.Fn.Proto.Constants[Bx]
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADBOOL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			if B != 0 {
				// +inline-call reg.Set RA LTrue
			} else {
				// +inline-call reg.Set RA LFalse
			}
			if C != 0 {
				cf.Pc++
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LOADNIL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			for i := RA; i <= lbase+B; i++ {
				// +inline-call reg.Set i LNil
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETUPVAL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			v := cf.Fn.Upvalues[B].Value()
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETGLOBAL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst & 0x3ffff) //GETBX
			// D3: the environment is the frame function's _ENV upvalue
			// cell (SETUPVAL on _ENV changes global resolution for every
			// closure sharing that cell); EnvUpvalueIndex < 0 covers
			// functions compiled without globals
			env := cf.Fn.Env
			if idx := cf.Fn.Proto.EnvUpvalueIndex; idx >= 0 {
				env, _ = cf.Fn.Upvalues[idx].Value().(*LTable)
			}
			v := L.getFieldString(env, cf.Fn.Proto.stringConstants[Bx])
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETTABLE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			v := L.getField(reg.Get(lbase+B), L.rkValue(C))
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_GETTABLEKS
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			v := L.getFieldString(reg.Get(lbase+B), L.rkString(C))
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETGLOBAL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst & 0x3ffff) //GETBX
			// D3: see OP_GETGLOBAL — writes go through the _ENV cell
			env := cf.Fn.Env
			if idx := cf.Fn.Proto.EnvUpvalueIndex; idx >= 0 {
				env, _ = cf.Fn.Upvalues[idx].Value().(*LTable)
			}
			L.setFieldString(env, cf.Fn.Proto.stringConstants[Bx], reg.Get(RA))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETUPVAL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			cf.Fn.Upvalues[B].SetValue(reg.Get(RA))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETTABLE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			L.setField(reg.Get(RA), L.rkValue(B), L.rkValue(C))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETTABLEKS
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			L.setFieldString(reg.Get(RA), L.rkString(B), L.rkValue(C))
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_NEWTABLE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			v := newLTable(B, C)
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SELF
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			selfobj := reg.Get(lbase + B)
			v := L.getFieldString(selfobj, L.rkString(C))
			// +inline-call reg.Set RA v
			// +inline-call reg.Set RA+1 selfobj
			return 0
		},
		opArith, // OP_ADD
		opArith, // OP_SUB
		opArith, // OP_MUL
		opArith, // OP_DIV
		opArith, // OP_MOD
		opArith, // OP_POW
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_UNM
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			unaryv := L.rkValue(B)
			if nm, ok := unaryv.(LInteger); ok {
				// +inline-call reg.SetInteger RA -nm
			} else if nm, ok := unaryv.(LNumber); ok {
				// +inline-call reg.Set RA -nm
			} else {
				op := L.metaOp1(unaryv, "__unm")
				if op.Type() == LTFunction {
					reg.Push(op)
					reg.Push(unaryv)
					L.Call(1, 1)
					// +inline-call reg.Set RA reg.Pop()
				} else if num, ok1 := coerceStringNumber(unaryv); ok1 {
					if iv, ok2 := num.(LInteger); ok2 {
						// +inline-call reg.SetInteger RA -iv
					} else {
						fv := num.(LNumber)
						// +inline-call reg.Set RA -fv
					}
				} else {
					L.RaiseError("attempt to perform arithmetic on a %s value", unaryv.Type().String())
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_NOT
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
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
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			switch lv := L.rkValue(B).(type) {
			case LString:
				nv := LInteger(len(lv))
				// +inline-call reg.SetInteger RA nv
			default:
				op := L.metaOp1(lv, "__len")
				if op.Type() == LTFunction {
					reg.Push(op)
					reg.Push(lv)
					L.Call(1, 1)
					ret := reg.Pop()
					// +inline-call reg.Set RA ret
				} else if lv.Type() == LTTable {
					nv := LInteger(lv.(*LTable).Len())
					// +inline-call reg.SetInteger RA nv
				} else {
					L.RaiseError("attempt to get length of a %s value", lv.Type().String())
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CONCAT
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			RC := lbase + C
			RB := lbase + B
			v := stringConcat(L, RC-RB+1, RC)
			// +inline-call reg.Set RA v
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_JMP
			cf := L.currentFrame
			Sbx := int(inst&0x3ffff) - opMaxArgSbx //GETSBX
			cf.Pc += Sbx
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_EQ
			cf := L.currentFrame
			A := int(inst>>18) & 0xff //GETA
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			ret := equals(L, L.rkValue(B), L.rkValue(C), false)
			v := 1
			if ret {
				v = 0
			}
			if v == A {
				cf.Pc++
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LT
			cf := L.currentFrame
			A := int(inst>>18) & 0xff //GETA
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			ret := lessThan(L, L.rkValue(B), L.rkValue(C))
			v := 1
			if ret {
				v = 0
			}
			if v == A {
				cf.Pc++
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_LE
			cf := L.currentFrame
			A := int(inst>>18) & 0xff //GETA
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			lhs := L.rkValue(B)
			rhs := L.rkValue(C)
			ret := false

			if res, ok := numLE(lhs, rhs); ok {
				ret = res
			} else if isNumberValue(lhs) || isNumberValue(rhs) {
				L.RaiseError("attempt to compare %v with %v", lhs.Type().String(), rhs.Type().String())
			} else {
				if lhs.Type() != rhs.Type() {
					L.RaiseError("attempt to compare %v with %v", lhs.Type().String(), rhs.Type().String())
				}
				switch lhs.Type() {
				case LTString:
					ret = strCmp(string(lhs.(LString)), string(rhs.(LString))) <= 0
				default:
					switch objectRational(L, lhs, rhs, "__le") {
					case 1:
						ret = true
					case 0:
						ret = false
					default:
						ret = !objectRationalWithError(L, rhs, lhs, "__lt")
					}
				}
			}

			v := 1
			if ret {
				v = 0
			}
			if v == A {
				cf.Pc++
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TEST
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			C := int(inst>>9) & 0x1ff //GETC
			if LVAsBool(reg.Get(RA)) == (C == 0) {
				cf.Pc++
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TESTSET
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			if value := reg.Get(lbase + B); LVAsBool(value) != (C == 0) {
				// +inline-call reg.Set RA value
			} else {
				cf.Pc++
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CALL
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
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
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
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
				L.RaiseError("attempt to call a non-function object")
			}
			// +inline-call L.closeUpvalues lbase
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
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_RETURN
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			// +inline-call L.closeUpvalues lbase
			nret := B - 1
			if B == 0 {
				nret = reg.Top() - RA
			}
			n := cf.NRet
			if cf.NRet == MultRet {
				n = nret
			}

			if L.Parent != nil && L.stack.Sp() == 1 {
				// +inline-call copyReturnValues L reg.Top() RA n B
				switchToParentThread(L, n, false, true)
				return 1
			}
			islast := baseframe == L.stack.Pop() || L.stack.IsEmpty()
			// +inline-call copyReturnValues L cf.ReturnBase RA n B
			L.currentFrame = L.stack.Last()
			if islast || L.currentFrame == nil || L.currentFrame.Fn.IsG {
				return 1
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_FORLOOP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			if count, ok := reg.Get(RA).(LInteger); ok {
				// integer loop: RA=count, RA+1=step, RA+2=idx, RA+3=visible
				if count > 0 {
					nv := count - 1
					// +inline-call reg.SetInteger RA nv
					step := int64(reg.Get(RA + 1).(LInteger))
					idx := int64(reg.Get(RA+2).(LInteger)) + step
					iv := LInteger(idx)
					// +inline-call reg.SetInteger RA+2 iv
					// +inline-call reg.SetInteger RA+3 iv
					Sbx := int(inst&0x3ffff) - opMaxArgSbx //GETSBX
					cf.Pc += Sbx
				} else {
					// +inline-call reg.SetTop RA+1
				}
			} else if init, ok1 := arithNumber(reg.Get(RA)); ok1 {
				if limit, ok2 := arithNumber(reg.Get(RA + 1)); ok2 {
					if step, ok3 := arithNumber(reg.Get(RA + 2)); ok3 {
						init += step
						v := LNumber(init)
						// +inline-call reg.SetNumber RA v
						if (step > 0 && init <= limit) || (step <= 0 && init >= limit) {
							Sbx := int(inst&0x3ffff) - opMaxArgSbx //GETSBX
							cf.Pc += Sbx
							// +inline-call reg.SetNumber RA+3 v
						} else {
							// +inline-call reg.SetTop RA+1
						}
					}
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_FORPREP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			Sbx := int(inst&0x3ffff) - opMaxArgSbx //GETSBX
			initv := reg.Get(RA)
			stepv := reg.Get(RA + 2)
			initi, initIsInt := initv.(LInteger)
			stepi, stepIsInt := stepv.(LInteger)
			if initIsInt && stepIsInt {
				// integer loop (init and step are integer-Typed, per stock
				// forprep; the limit converts below). Layout: RA=count,
				// RA+1=step, RA+2=idx, RA+3=visible (the compiler's slots).
				if stepi == 0 {
					L.RaiseError("'for' step is zero")
				}
				limit, skip := forLimitInt(L, int64(initi), reg.Get(RA+1), int64(stepi))
				if skip {
					zv := LInteger(0)
					// +inline-call reg.SetInteger RA zv
				} else {
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
					count++ // this VM tests before each body pass (5.1 flow)
					if count == 0 {
						// (init,limit)=(minint,maxint), step 1: 2^64
						// iterations saturates the counter — the final
						// pass is lost (ledger row: U2b; stock's flow
						// counts iterations-1 and never wraps — U2c)
						count = 0xFFFFFFFFFFFFFFFF
					}
					cv := LInteger(int64(count))
					sv := LInteger(stepi)
					iv := LInteger(int64(uint64(initi) - uint64(stepi)))
					// +inline-call reg.SetInteger RA cv
					// +inline-call reg.SetInteger RA+1 sv
					// +inline-call reg.SetInteger RA+2 iv
				}
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
					// make sure all three cells hold floats (stock's
					// forprep rewrites the slots; string limits included)
					lv := LNumber(limitf)
					sv2 := LNumber(stepf)
					v := LNumber(initf - stepf)
					// +inline-call reg.SetNumber RA v
					// +inline-call reg.SetNumber RA+1 lv
					// +inline-call reg.SetNumber RA+2 sv2
				}
			}
			cf.Pc += Sbx
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_TFORLOOP
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			C := int(inst>>9) & 0x1ff //GETC
			nret := C
			// +inline-call reg.SetTop RA+3+2
			// +inline-call reg.Set RA+3+2 reg.Get(RA+2)
			// +inline-call reg.Set RA+3+1 reg.Get(RA+1)
			// +inline-call reg.Set RA+3 reg.Get(RA)
			L.callR(2, nret, RA+3)
			if value := reg.Get(RA + 3); value != LNil {
				// +inline-call reg.Set RA+2 value
				pc := cf.Fn.Proto.Code[cf.Pc]
				cf.Pc += int(pc&0x3ffff) - opMaxArgSbx
			}
			cf.Pc++
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_SETLIST
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff)    //GETB
			C := int(inst>>9) & 0x1ff //GETC
			if C == 0 {
				C = int(cf.Fn.Proto.Code[cf.Pc])
				cf.Pc++
			}
			offset := (C - 1) * FieldsPerFlush
			table := reg.Get(RA).(*LTable)
			nelem := B
			if B == 0 {
				nelem = reg.Top() - RA - 1
			}
			for i := 1; i <= nelem; i++ {
				table.RawSetInt(offset+i, reg.Get(RA+i))
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CLOSE
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			// +inline-call L.closeUpvalues RA
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_CLOSURE
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			Bx := int(inst & 0x3ffff) //GETBX
			proto := cf.Fn.Proto.FunctionPrototypes[Bx]
			closure := newLFunctionL(proto, cf.Fn.Env, int(proto.NumUpvalues))
			// +inline-call reg.Set RA closure
			for i := 0; i < int(proto.NumUpvalues); i++ {
				inst = cf.Fn.Proto.Code[cf.Pc]
				cf.Pc++
				B := opGetArgB(inst)
				switch opGetOpCode(inst) {
				case OP_MOVE:
					closure.Upvalues[i] = L.findUpvalue(lbase + B)
				case OP_GETUPVAL:
					closure.Upvalues[i] = cf.Fn.Upvalues[B]
				}
			}
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_VARARG
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			nparams := int(cf.Fn.Proto.NumParameters)
			nvarargs := cf.NArgs - nparams
			if nvarargs < 0 {
				nvarargs = 0
			}
			nwant := B - 1
			if B == 0 {
				nwant = nvarargs
			}
			// +inline-call reg.CopyRange RA cf.Base+nparams+1 cf.LocalBase nwant
			return 0
		},
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_NOP
			return 0
		},
		opArith, // OP_IDIV
		opArith, // OP_BAND
		opArith, // OP_BOR
		opArith, // OP_BXOR
		opArith, // OP_SHL
		opArith, // OP_SHR
		func(L *LState, inst uint32, baseframe *callFrame) int { //OP_BNOT
			reg := L.reg
			cf := L.currentFrame
			lbase := cf.LocalBase
			A := int(inst>>18) & 0xff //GETA
			RA := lbase + A
			B := int(inst & 0x1ff) //GETB
			unaryv := L.rkValue(B)
			if iv, ok := unaryv.(LInteger); ok {
				nv := ^iv
				// +inline-call reg.SetInteger RA nv
			} else if fv, ok := unaryv.(LNumber); ok {
				if iv, ok2 := floatToIntExact(float64(fv)); ok2 {
					nv := ^LInteger(iv)
					// +inline-call reg.SetInteger RA nv
				} else {
					L.RaiseError("number has no integer representation")
				}
			} else {
				op := L.metaOp1(unaryv, "__bnot")
				if op.Type() == LTFunction {
					reg.Push(op)
					reg.Push(unaryv)
					L.Call(1, 1)
					// +inline-call reg.Set RA reg.Pop()
				} else {
					// no string coercion: the string metatable has no
					// __bnot (stock stringmetamethods)
					L.RaiseError("attempt to perform bitwise operation on a %s value", unaryv.Type().String())
				}
			}
			return 0
		},
	}
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
	L.RaiseError("bad 'for' %s (number expected, got %s)", what, v.Type().String())
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
		L.RaiseError("bad 'for' limit (number expected, got %s)", lim.Type().String())
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

func opArith(L *LState, inst uint32, baseframe *callFrame) int { //OP_ADD..OP_SHR
	reg := L.reg
	cf := L.currentFrame
	lbase := cf.LocalBase
	A := int(inst>>18) & 0xff //GETA
	RA := lbase + A
	opcode := int(inst >> 26) //GETOPCODE
	B := int(inst & 0x1ff)    //GETB
	C := int(inst>>9) & 0x1ff //GETC
	lhs := L.rkValue(B)
	rhs := L.rkValue(C)
	if v, errText, res := arithCompute(opcode, lhs, rhs); res == arithDone {
		if iv, isInt := v.(LInteger); isInt {
			// +inline-call reg.SetInteger RA iv
		} else {
			fv := v.(LNumber)
			// +inline-call reg.SetNumber RA fv
		}
	} else if res == arithRaise {
		L.RaiseError("%s", errText)
	} else {
		v := objectArith(L, opcode, lhs, rhs)
		// +inline-call reg.Set RA v
	}
	return 0
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

func objectArith(L *LState, opcode int, lhs, rhs LValue) LValue {
	event := arithEvent(opcode)
	op := L.metaOp2(lhs, rhs, event)
	if _, ok := op.(*LFunction); ok {
		L.reg.Push(op)
		L.reg.Push(lhs)
		L.reg.Push(rhs)
		L.Call(2, 1)
		return L.reg.Pop()
	}
	// string→number coercion preserving the syntactic subtype, for the
	// ARITHMETIC ops only — stock's string metatable carries __add…
	// __unm but no bitwise metamethods (lstrlib.c stringmetamethods), so
	// "3" & 5 is a type error, never a coercion. 5.5 routes the
	// arithmetic coercion through the string metatable — U3 (row 58).
	if !isBitwiseOp(opcode) {
		if str, ok := lhs.(LString); ok {
			if lnum, err := parseNumber(string(str)); err == nil {
				lhs = lnum
			}
		}
		if str, ok := rhs.(LString); ok {
			if rnum, err := parseNumber(string(str)); err == nil {
				rhs = rnum
			}
		}
	}
	if v, errText, res := arithCompute(opcode, lhs, rhs); res == arithDone {
		return v
	} else if res == arithRaise {
		L.RaiseError("%s", errText)
	}
	// stock luaT_trybinTM error shapes (luaG_opinterror picks the
	// non-number operand; both non-numbers reports the first)
	what := "arithmetic"
	if isBitwiseOp(opcode) {
		what = "bitwise operation"
	}
	bad := lhs
	if isNumberValue(lhs) {
		bad = rhs
	}
	L.RaiseError("attempt to perform %s on a %s value", what, bad.Type().String())

	return LNil
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

func stringConcat(L *LState, total, last int) LValue {
	rhs := L.reg.Get(last)
	total--
	for i := last - 1; total > 0; {
		lhs := L.reg.Get(i)
		if !(LVCanConvToString(lhs) && LVCanConvToString(rhs)) {
			op := L.metaOp2(lhs, rhs, "__concat")
			if op.Type() == LTFunction {
				L.reg.Push(op)
				L.reg.Push(lhs)
				L.reg.Push(rhs)
				L.Call(2, 1)
				rhs = L.reg.Pop()
				total--
				i--
			} else {
				L.RaiseError("cannot perform concat operation between %v and %v", lhs.Type().String(), rhs.Type().String())
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

func lessThan(L *LState, lhs, rhs LValue) bool {
	// optimization for numbers
	if ret, ok := numLT(lhs, rhs); ok {
		return ret
	}
	if isNumberValue(lhs) || isNumberValue(rhs) {
		L.RaiseError("attempt to compare %v with %v", lhs.Type().String(), rhs.Type().String())
	}
	if lhs.Type() != rhs.Type() {
		L.RaiseError("attempt to compare %v with %v", lhs.Type().String(), rhs.Type().String())
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
	L.RaiseError("attempt to compare %v with %v", lhs.Type().String(), rhs.Type().String())
	return false
}

func objectRational(L *LState, lhs, rhs LValue, event string) int {
	m1 := L.metaOp1(lhs, event)
	m2 := L.metaOp1(rhs, event)
	if m1.Type() == LTFunction && m1 == m2 {
		L.reg.Push(m1)
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
