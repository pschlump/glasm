package wasm

import (
	"encoding/binary"
	"math"
)

// Instruction emitters. Each method appends one instruction to the function
// body and returns the function for chaining. Operands follow the caller's
// view of the stack (no validation); misuse surfaces at instantiation time.
//
// Memory instructions take only an offset; alignment is fixed to the
// operation's natural alignment, which is what the backend always wants.

// Local appends one local of the given type and returns the function.
func (f *Function) Local(typ ValueType) *Function {
	return f.LocalN(typ, 1)
}

// LocalN appends count locals of the given type as one group.
func (f *Function) LocalN(typ ValueType, count uint32) *Function {
	f.locals = append(f.locals, localGroup{count, typ})
	return f
}

// LocalBase returns the local index at which declared (non-parameter)
// locals begin.
func (f *Function) LocalBase() uint32 {
	return uint32(len(f.m.types[f.typeIdx].params))
}

func (f *Function) op(code byte) *Function {
	f.body = append(f.body, code)
	return f
}

func (f *Function) opU32(code byte, v uint32) *Function {
	f.body = append(f.body, code)
	f.body = AppendU32(f.body, v)
	return f
}

func (f *Function) memop(code byte, align uint32, offset uint32) *Function {
	f.body = append(f.body, code)
	f.body = AppendU32(f.body, align)
	f.body = AppendU32(f.body, offset)
	return f
}

/* control */

func (f *Function) Block(bt BlockType) *Function { f.depth++; return f.op2(0x02, byte(bt)) }
func (f *Function) Loop(bt BlockType) *Function  { f.depth++; return f.op2(0x03, byte(bt)) }
func (f *Function) If(bt BlockType) *Function    { f.depth++; return f.op2(0x04, byte(bt)) }
func (f *Function) Else() *Function              { return f.op(0x05) }

// End closes the innermost open block. When no block is open it terminates
// the function body; a second terminating End panics.
func (f *Function) End() *Function {
	if f.depth == 0 {
		if f.terminated {
			panic("wasm: function already terminated by End")
		}
		f.terminated = true
		return f.op(0x0B)
	}
	f.depth--
	return f.op(0x0B)
}

func (f *Function) Br(label uint32) *Function   { return f.opU32(0x0C, label) }
func (f *Function) BrIf(label uint32) *Function { return f.opU32(0x0D, label) }
func (f *Function) Return() *Function           { return f.op(0x0F) }
func (f *Function) Call(fnIdx uint32) *Function { return f.opU32(0x10, fnIdx) }

// CallIndirect emits call_indirect on table 0 with the given type index.
// Stack order: function arguments, then the table index.
func (f *Function) CallIndirect(typeIdx uint32) *Function {
	f.body = append(f.body, 0x11)
	f.body = AppendU32(f.body, typeIdx)
	f.body = AppendU32(f.body, 0) // table 0
	return f
}

// BrTable emits br_table with the given label targets and default target.
func (f *Function) BrTable(labels []uint32, defaultLabel uint32) *Function {
	f.body = append(f.body, 0x0E)
	f.body = AppendU32(f.body, uint32(len(labels)))
	for _, l := range labels {
		f.body = AppendU32(f.body, l)
	}
	f.body = AppendU32(f.body, defaultLabel)
	return f
}

func (f *Function) Unreachable() *Function { return f.op(0x00) }
func (f *Function) Nop() *Function         { return f.op(0x01) }

/* parametric */

func (f *Function) Drop() *Function   { return f.op(0x1A) }
func (f *Function) Select() *Function { return f.op(0x1B) }

/* variables */

func (f *Function) LocalGet(i uint32) *Function  { return f.opU32(0x20, i) }
func (f *Function) LocalSet(i uint32) *Function  { return f.opU32(0x21, i) }
func (f *Function) LocalTee(i uint32) *Function  { return f.opU32(0x22, i) }
func (f *Function) GlobalGet(i uint32) *Function { return f.opU32(0x23, i) }
func (f *Function) GlobalSet(i uint32) *Function { return f.opU32(0x24, i) }

/* memory */

func (f *Function) I32Load(offset uint32) *Function   { return f.memop(0x28, 2, offset) }
func (f *Function) I64Load(offset uint32) *Function   { return f.memop(0x29, 3, offset) }
func (f *Function) F32Load(offset uint32) *Function   { return f.memop(0x2A, 2, offset) }
func (f *Function) F64Load(offset uint32) *Function   { return f.memop(0x2B, 3, offset) }
func (f *Function) I32Load8S(offset uint32) *Function { return f.memop(0x2C, 0, offset) }
func (f *Function) I32Load8U(offset uint32) *Function { return f.memop(0x2D, 0, offset) }
func (f *Function) I32Load16S(offset uint32) *Function {
	return f.memop(0x2E, 1, offset)
}
func (f *Function) I32Load16U(offset uint32) *Function {
	return f.memop(0x2F, 1, offset)
}
func (f *Function) I32Store(offset uint32) *Function  { return f.memop(0x36, 2, offset) }
func (f *Function) I64Store(offset uint32) *Function  { return f.memop(0x37, 3, offset) }
func (f *Function) F32Store(offset uint32) *Function  { return f.memop(0x38, 2, offset) }
func (f *Function) F64Store(offset uint32) *Function  { return f.memop(0x39, 3, offset) }
func (f *Function) I32Store8(offset uint32) *Function { return f.memop(0x3A, 0, offset) }
func (f *Function) I32Store16(offset uint32) *Function {
	return f.memop(0x3B, 1, offset)
}
func (f *Function) MemorySize() *Function { return f.op2(0x3F, 0x00) }
func (f *Function) MemoryGrow() *Function { return f.op2(0x40, 0x00) }

/* constants */

func (f *Function) I32Const(v int32) *Function {
	f.body = append(f.body, 0x41)
	f.body = AppendI32(f.body, v)
	return f
}

func (f *Function) I64Const(v int64) *Function {
	f.body = append(f.body, 0x42)
	f.body = AppendS64(f.body, v)
	return f
}

func (f *Function) F32Const(v float32) *Function {
	f.body = append(f.body, 0x43)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], math.Float32bits(v))
	f.body = append(f.body, b[:]...)
	return f
}

func (f *Function) F64Const(v float64) *Function {
	f.body = append(f.body, 0x44)
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(v))
	f.body = append(f.body, b[:]...)
	return f
}

/* i32 */

func (f *Function) I32Eqz() *Function { return f.op(0x45) }
func (f *Function) I32Eq() *Function  { return f.op(0x46) }
func (f *Function) I32Ne() *Function  { return f.op(0x47) }
func (f *Function) I32LtS() *Function { return f.op(0x48) }
func (f *Function) I32LtU() *Function { return f.op(0x49) }
func (f *Function) I32GtS() *Function { return f.op(0x4A) }
func (f *Function) I32GtU() *Function { return f.op(0x4B) }
func (f *Function) I32LeS() *Function { return f.op(0x4C) }
func (f *Function) I32LeU() *Function { return f.op(0x4D) }
func (f *Function) I32GeS() *Function { return f.op(0x4E) }
func (f *Function) I32GeU() *Function { return f.op(0x4F) }
func (f *Function) I32Add() *Function { return f.op(0x6A) }
func (f *Function) I32Sub() *Function { return f.op(0x6B) }
func (f *Function) I32Mul() *Function { return f.op(0x6C) }
func (f *Function) I32DivS() *Function {
	return f.op(0x6D)
}
func (f *Function) I32DivU() *Function { return f.op(0x6E) }
func (f *Function) I32RemS() *Function { return f.op(0x6F) }
func (f *Function) I32RemU() *Function { return f.op(0x70) }
func (f *Function) I32And() *Function  { return f.op(0x71) }
func (f *Function) I32Or() *Function   { return f.op(0x72) }
func (f *Function) I32Xor() *Function  { return f.op(0x73) }
func (f *Function) I32Shl() *Function  { return f.op(0x74) }
func (f *Function) I32ShrS() *Function { return f.op(0x75) }
func (f *Function) I32ShrU() *Function { return f.op(0x76) }

/* i64 */

func (f *Function) I64Eqz() *Function  { return f.op(0x50) }
func (f *Function) I64Eq() *Function   { return f.op(0x51) }
func (f *Function) I64Ne() *Function   { return f.op(0x52) }
func (f *Function) I64LtS() *Function  { return f.op(0x53) }
func (f *Function) I64GtS() *Function  { return f.op(0x55) }
func (f *Function) I64LeS() *Function  { return f.op(0x57) }
func (f *Function) I64GeS() *Function  { return f.op(0x59) }
func (f *Function) I64LtU() *Function  { return f.op(0x54) }
func (f *Function) I64LeU() *Function  { return f.op(0x58) }
func (f *Function) I64GeU() *Function  { return f.op(0x5A) }
func (f *Function) I64Add() *Function  { return f.op(0x7C) }
func (f *Function) I64Sub() *Function  { return f.op(0x7D) }
func (f *Function) I64Mul() *Function  { return f.op(0x7E) }
func (f *Function) I64DivS() *Function { return f.op(0x7F) }
func (f *Function) I64RemS() *Function { return f.op(0x81) }
func (f *Function) I64And() *Function  { return f.op(0x83) }
func (f *Function) I64Or() *Function   { return f.op(0x84) }
func (f *Function) I64Xor() *Function  { return f.op(0x85) }
func (f *Function) I64Shl() *Function  { return f.op(0x86) }
func (f *Function) I64ShrU() *Function { return f.op(0x88) }

/* f64 */

func (f *Function) F64Eq() *Function    { return f.op(0x61) }
func (f *Function) F64Ne() *Function    { return f.op(0x62) }
func (f *Function) F64Lt() *Function    { return f.op(0x63) }
func (f *Function) F64Gt() *Function    { return f.op(0x64) }
func (f *Function) F64Le() *Function    { return f.op(0x65) }
func (f *Function) F64Ge() *Function    { return f.op(0x66) }
func (f *Function) F64Abs() *Function   { return f.op(0x99) }
func (f *Function) F64Neg() *Function   { return f.op(0x9A) }
func (f *Function) F64Ceil() *Function  { return f.op(0x9B) }
func (f *Function) F64Floor() *Function { return f.op(0x9C) }
func (f *Function) F64Sqrt() *Function  { return f.op(0x9F) }
func (f *Function) F64Add() *Function   { return f.op(0xA0) }
func (f *Function) F64Sub() *Function   { return f.op(0xA1) }
func (f *Function) F64Mul() *Function   { return f.op(0xA2) }
func (f *Function) F64Div() *Function   { return f.op(0xA3) }
func (f *Function) F64Min() *Function   { return f.op(0xA4) }
func (f *Function) F64Max() *Function   { return f.op(0xA5) }

/* conversions */

func (f *Function) I32WrapI64() *Function     { return f.op(0xA7) }
func (f *Function) I32TruncF64S() *Function   { return f.op(0xAA) }
func (f *Function) I32TruncF64U() *Function   { return f.op(0xAB) }
func (f *Function) I64ExtendI32S() *Function  { return f.op(0xAC) }
func (f *Function) I64ExtendI32U() *Function  { return f.op(0xAD) }
func (f *Function) I64TruncF64S() *Function   { return f.op(0xB0) }
func (f *Function) F64ConvertI32S() *Function { return f.op(0xB7) }
func (f *Function) F64ConvertI32U() *Function { return f.op(0xB8) }
func (f *Function) F64ConvertI64S() *Function { return f.op(0xB9) }

func (f *Function) op2(code byte, imm byte) *Function {
	f.body = append(f.body, code, imm)
	return f
}
