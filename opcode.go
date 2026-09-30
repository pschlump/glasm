package lua

import (
	"fmt"
)

/*
  U2c (upgrade plan D2): the stock Lua 5.5.1 instruction set and encoding,
  verified line-by-line against the vendored runtime/lua55/src/lopcodes.h.

          instruction = 32bit(fixed length), opcode in the first 7 bits

          3 3 2 2 2 2 2 2 2 2 2 2 1 1 1 1 1 1 1 1 1 1 0 0 0 0 0 0 0 0 0 0
          1 0 9 8 7 6 5 4 3 2 1 0 9 8 7 6 5 4 3 2 1 0 9 8 7 6 5 4 3 2 1 0
  iABC          C(8)     |      B(8)     |k|     A(8)      |   Op(7)     |
  ivABC         vC(10)     |     vB(6)   |k|     A(8)      |   Op(7)     |
  iABx                Bx(17)               |     A(8)      |   Op(7)     |
  iAsBx              sBx (signed)(17)      |     A(8)      |   Op(7)     |
  iAx                           Ax(25)                     |   Op(7)     |
  isJ                           sJ (signed)(25)            |   Op(7)     |

  Signed arguments are excess-K encoded (value = written - OFFSET).
*/

const opInvalidInstruction = ^uint32(0)

const opSizeOp = 7
const opSizeA = 8
const opSizeB = 8
const opSizeC = 8
const opSizevB = 6
const opSizevC = 10
const opSizeBx = opSizeC + opSizeB + 1 // 17
const opSizeAx = opSizeBx + opSizeA    // 25
const opSizesJ = opSizeBx + opSizeA    // 25

const opPosOp = 0
const opPosA = opPosOp + opSizeOp // 7
const opPosK = opPosA + opSizeA   // 15
const opPosB = opPosK + 1         // 16
const opPosC = opPosB + opSizeB   // 24

const opMaxArgsA = (1 << opSizeA) - 1
const opMaxArgsB = (1 << opSizeB) - 1
const opMaxArgsC = (1 << opSizeC) - 1
const opMaxArgsvB = (1 << opSizevB) - 1
const opMaxArgsvC = (1 << opSizevC) - 1
const opMaxArgBx = (1 << opSizeBx) - 1
const opMaxArgAx = (1 << opSizeAx) - 1
const opMaxArgsJ = (1 << opSizesJ) - 1

const opMaxArgSbx = opMaxArgBx >> 1 // OFFSET_sBx
const opOffsetSJ = opMaxArgsJ >> 1  // OFFSET_sJ

// opMaxIndexK: largest constant index addressable by a K-operand
// (8-bit B/C fields).
const opMaxIndexK = opMaxArgsC

// int2sC / sC2int: the sC immediate encoding (lopcodes.h).
const opOffsetSC = opMaxArgsC >> 1

func int2sC(i int) int { return i + opOffsetSC }
func sC2int(i int) int { return i - opOffsetSC }
func fitsC(i int64) bool {
	return uint64(i)+opOffsetSC <= uint64(opMaxArgsC)
}

func fitsBx(i int64) bool {
	return -int64(opMaxArgSbx) <= i && i <= int64(opMaxArgBx)-int64(opMaxArgSbx)
}

const (
	OP_MOVE       int = iota /*      A B     R[A] := R[B]                                        */
	OP_LOADI                 /*      A sBx   R[A] := sBx                                        */
	OP_LOADF                 /*      A sBx   R[A] := (float)sBx                                 */
	OP_LOADK                 /*      A Bx    R[A] := K[Bx]                                      */
	OP_LOADKX                /*      A       R[A] := K[extra arg]                               */
	OP_LOADFALSE             /*      A       R[A] := false                                      */
	OP_LFALSESKIP            /*      A       R[A] := false; pc++                                */
	OP_LOADTRUE              /*      A       R[A] := true                                       */
	OP_LOADNIL               /*      A B     R[A], R[A+1], ..., R[A+B] := nil                   */
	OP_GETUPVAL              /*      A B     R[A] := UpValue[B]                                 */
	OP_SETUPVAL              /*      A B     UpValue[B] := R[A]                                 */

	OP_GETTABUP /*      A B C   R[A] := UpValue[B][K[C]:string]                       */
	OP_GETTABLE /*      A B C   R[A] := R[B][R[C]]                                     */
	OP_GETI     /*      A B C   R[A] := R[B][C]                                       */
	OP_GETFIELD /*      A B C   R[A] := R[B][K[C]:string]                             */

	OP_SETTABUP /*      A B C   UpValue[A][K[B]:string] := RK(C)                      */
	OP_SETTABLE /*      A B C   R[A][R[B]] := RK(C)                                   */
	OP_SETI     /*      A B C   R[A][B] := RK(C)                                      */
	OP_SETFIELD /*      A B C   R[A][K[B]:string] := RK(C)                            */

	OP_NEWTABLE /*      A vB vC k  R[A] := {}                                          */

	OP_SELF /*      A B C   R[A+1] := R[B]; R[A] := R[B][K[C]:string]                  */

	OP_ADDI /*      A B sC   R[A] := R[B] + sC                                         */

	OP_ADDK  /*      A B C   R[A] := R[B] + K[C]:number                                */
	OP_SUBK  /*      A B C   R[A] := R[B] - K[C]:number                                */
	OP_MULK  /*      A B C   R[A] := R[B] * K[C]:number                                */
	OP_MODK  /*      A B C   R[A] := R[B] % K[C]:number                                */
	OP_POWK  /*      A B C   R[A] := R[B] ^ K[C]:number                                */
	OP_DIVK  /*      A B C   R[A] := R[B] / K[C]:number                                */
	OP_IDIVK /*      A B C   R[A] := R[B] // K[C]:number                               */

	OP_BANDK /*      A B C   R[A] := R[B] & K[C]:integer                               */
	OP_BORK  /*      A B C   R[A] := R[B] | K[C]:integer                               */
	OP_BXORK /*      A B C   R[A] := R[B] ~ K[C]:integer                               */

	OP_SHLI /*      A B sC   R[A] := sC << R[B]                                        */
	OP_SHRI /*      A B sC   R[A] := R[B] >> sC                                        */

	OP_ADD  /*      A B C   R[A] := R[B] + R[C]                                        */
	OP_SUB  /*      A B C   R[A] := R[B] - R[C]                                        */
	OP_MUL  /*      A B C   R[A] := R[B] * R[C]                                        */
	OP_MOD  /*      A B C   R[A] := R[B] % R[C]                                        */
	OP_POW  /*      A B C   R[A] := R[B] ^ R[C]                                        */
	OP_DIV  /*      A B C   R[A] := R[B] / R[C]                                        */
	OP_IDIV /*      A B C   R[A] := R[B] // R[C]                                       */

	OP_BAND /*      A B C   R[A] := R[B] & R[C]                                       */
	OP_BOR  /*      A B C   R[A] := R[B] | R[C]                                       */
	OP_BXOR /*      A B C   R[A] := R[B] ~ R[C]                                       */
	OP_SHL  /*      A B C   R[A] := R[B] << R[C]                                      */
	OP_SHR  /*      A B C   R[A] := R[B] >> R[C]                                      */

	OP_MMBIN  /*      A B C   call C metamethod over R[A] and R[B]                     */
	OP_MMBINI /*      A sB C k  call C metamethod over R[A] and sB                     */
	OP_MMBINK /*      A B C k   call C metamethod over R[A] and K[B]                   */

	OP_UNM  /*      A B     R[A] := -R[B]                                             */
	OP_BNOT /*      A B     R[A] := ~R[B]                                             */
	OP_NOT  /*      A B     R[A] := not R[B]                                          */
	OP_LEN  /*      A B     R[A] := #R[B]                                             */

	OP_CONCAT /*      A B     R[A] := R[A].. ... ..R[A + B - 1]                        */

	OP_CLOSE /*      A       close all upvalues >= R[A]                               */
	OP_TBC   /*      A       mark variable A "to be closed"                           */
	OP_JMP   /*      sJ      pc += sJ                                                */
	OP_EQ    /*      A B k   if ((R[A] == R[B]) ~= k) then pc++                       */
	OP_LT    /*      A B k   if ((R[A] <  R[B]) ~= k) then pc++                       */
	OP_LE    /*      A B k   if ((R[A] <= R[B]) ~= k) then pc++                       */

	OP_EQK /*      A B k   if ((R[A] == K[B]) ~= k) then pc++                          */
	OP_EQI /*      A sB k  if ((R[A] == sB) ~= k) then pc++                            */
	OP_LTI /*      A sB k  if ((R[A] < sB) ~= k) then pc++                             */
	OP_LEI /*      A sB k  if ((R[A] <= sB) ~= k) then pc++                            */
	OP_GTI /*      A sB k  if ((R[A] > sB) ~= k) then pc++                             */
	OP_GEI /*      A sB k  if ((R[A] >= sB) ~= k) then pc++                            */

	OP_TEST    /*      A k     if (not R[A] == k) then pc++                            */
	OP_TESTSET /*      A B k   if (not R[B] == k) then pc++ else R[A] := R[B]          */

	OP_CALL     /*      A B C   R[A], ... ,R[A+C-2] := R[A](R[A+1], ... ,R[A+B-1])     */
	OP_TAILCALL /*      A B C k  return R[A](R[A+1], ... ,R[A+B-1])                    */

	OP_RETURN  /*      A B C k  return R[A], ... ,R[A+B-2]                             */
	OP_RETURN0 /*              return                                                 */
	OP_RETURN1 /*      A       return R[A]                                            */

	OP_FORLOOP /*      A Bx    update counters; if loop continues then pc-=Bx         */
	OP_FORPREP /*      A Bx    <check values and prepare counters>;
	        if not to run then pc+=Bx+1                                 */

	OP_TFORPREP /*      A Bx    create upvalue for R[A + 3]; pc+=Bx                   */
	OP_TFORCALL /*      A C     R[A+4], ... ,R[A+3+C] := R[A](R[A+1], R[A+2])          */
	OP_TFORLOOP /*      A Bx    if R[A+2] ~= nil then { R[A]=R[A+2]; pc -= Bx }       */

	OP_SETLIST /*      A vB vC k  R[A][vC+i] := R[A+i], 1 <= i <= vB                  */

	OP_CLOSURE /*      A Bx    R[A] := closure(KPROTO[Bx])                             */

	OP_VARARG /*      A B C k R[A], ..., R[A+C-2] = varargs                           */

	OP_GETVARG /*      A B C   R[A] := R[B][R[C]], R[B] is vararg parameter           */

	OP_ERRNNIL /*      A Bx    raise error if R[A] ~= nil (K[Bx - 1] is global name)  */

	OP_VARARGPREP /*              (adjust varargs)                                    */

	OP_EXTRAARG /*      Ax      extra (larger) argument for previous opcode           */
)
const opCodeMax = OP_EXTRAARG

// Tag-method numbers for the MMBIN family's C operand (ltm.h TMS order;
// the arith/bitwise events line up with OP_ADD..OP_SHR).
const (
	tmIndex = iota
	tmNewindex
	tmGC
	tmMode
	tmLen
	tmEq
	tmAdd
	tmSub
	tmMul
	tmMod
	tmPow
	tmDiv
	tmIdiv
	tmBand
	tmBor
	tmBxor
	tmShl
	tmShr
	tmUnm
	tmBnot
	tmLt
	tmLe
	tmConcat
	tmCall
	tmClose
)

// arithTM maps OP_ADD..OP_SHR to its tag-method number (binopr2TM).
func arithTM(op int) int {
	if op >= OP_ADD && op <= OP_SHR {
		return op - OP_ADD + tmAdd
	}
	switch op {
	case OP_UNM:
		return tmUnm
	case OP_BNOT:
		return tmBnot
	}
	return -1
}

type opMode int

const (
	opModeABC opMode = iota
	opModevABC
	opModeABx
	opModeAsBx
	opModeAx
	opModeSJ
)

type opArgMode int

const (
	opArgModeN opArgMode = iota
	opArgModeU
	opArgModeR
	opArgModeK
	opArgModeI // sB/sC immediate
)

type opProp struct {
	Name     string
	IsTest   bool
	SetRegA  bool
	ModeArgB opArgMode
	ModeArgC opArgMode
	Mode     opMode
}

var opProps = []opProp{
	/* 0*/ opProp{"MOVE", false, true, opArgModeR, opArgModeN, opModeABC},
	/* 1*/ opProp{"LOADI", false, true, opArgModeI, opArgModeN, opModeAsBx},
	/* 2*/ opProp{"LOADF", false, true, opArgModeI, opArgModeN, opModeAsBx},
	/* 3*/ opProp{"LOADK", false, true, opArgModeK, opArgModeN, opModeABx},
	/* 4*/ opProp{"LOADKX", false, true, opArgModeN, opArgModeN, opModeABx},
	/* 5*/ opProp{"LOADFALSE", false, true, opArgModeN, opArgModeN, opModeABC},
	/* 6*/ opProp{"LFALSESKIP", false, true, opArgModeN, opArgModeN, opModeABC},
	/* 7*/ opProp{"LOADTRUE", false, true, opArgModeN, opArgModeN, opModeABC},
	/* 8*/ opProp{"LOADNIL", false, true, opArgModeU, opArgModeN, opModeABC},
	/* 9*/ opProp{"GETUPVAL", false, true, opArgModeU, opArgModeN, opModeABC},
	/*10*/ opProp{"SETUPVAL", false, false, opArgModeU, opArgModeN, opModeABC},
	/*11*/ opProp{"GETTABUP", false, true, opArgModeU, opArgModeK, opModeABC},
	/*12*/ opProp{"GETTABLE", false, true, opArgModeR, opArgModeR, opModeABC},
	/*13*/ opProp{"GETI", false, true, opArgModeR, opArgModeU, opModeABC},
	/*14*/ opProp{"GETFIELD", false, true, opArgModeR, opArgModeK, opModeABC},
	/*15*/ opProp{"SETTABUP", false, false, opArgModeK, opArgModeK, opModeABC},
	/*16*/ opProp{"SETTABLE", false, false, opArgModeR, opArgModeK, opModeABC},
	/*17*/ opProp{"SETI", false, false, opArgModeU, opArgModeK, opModeABC},
	/*18*/ opProp{"SETFIELD", false, false, opArgModeK, opArgModeK, opModeABC},
	/*19*/ opProp{"NEWTABLE", false, true, opArgModeU, opArgModeU, opModevABC},
	/*20*/ opProp{"SELF", false, true, opArgModeR, opArgModeK, opModeABC},
	/*21*/ opProp{"ADDI", false, true, opArgModeR, opArgModeI, opModeABC},
	/*22*/ opProp{"ADDK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*23*/ opProp{"SUBK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*24*/ opProp{"MULK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*25*/ opProp{"MODK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*26*/ opProp{"POWK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*27*/ opProp{"DIVK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*28*/ opProp{"IDIVK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*29*/ opProp{"BANDK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*30*/ opProp{"BORK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*31*/ opProp{"BXORK", false, true, opArgModeR, opArgModeK, opModeABC},
	/*32*/ opProp{"SHLI", false, true, opArgModeR, opArgModeI, opModeABC},
	/*33*/ opProp{"SHRI", false, true, opArgModeR, opArgModeI, opModeABC},
	/*34*/ opProp{"ADD", false, true, opArgModeR, opArgModeR, opModeABC},
	/*35*/ opProp{"SUB", false, true, opArgModeR, opArgModeR, opModeABC},
	/*36*/ opProp{"MUL", false, true, opArgModeR, opArgModeR, opModeABC},
	/*37*/ opProp{"MOD", false, true, opArgModeR, opArgModeR, opModeABC},
	/*38*/ opProp{"POW", false, true, opArgModeR, opArgModeR, opModeABC},
	/*39*/ opProp{"DIV", false, true, opArgModeR, opArgModeR, opModeABC},
	/*40*/ opProp{"IDIV", false, true, opArgModeR, opArgModeR, opModeABC},
	/*41*/ opProp{"BAND", false, true, opArgModeR, opArgModeR, opModeABC},
	/*42*/ opProp{"BOR", false, true, opArgModeR, opArgModeR, opModeABC},
	/*43*/ opProp{"BXOR", false, true, opArgModeR, opArgModeR, opModeABC},
	/*44*/ opProp{"SHL", false, true, opArgModeR, opArgModeR, opModeABC},
	/*45*/ opProp{"SHR", false, true, opArgModeR, opArgModeR, opModeABC},
	/*46*/ opProp{"MMBIN", false, false, opArgModeR, opArgModeU, opModeABC},
	/*47*/ opProp{"MMBINI", false, false, opArgModeI, opArgModeU, opModeABC},
	/*48*/ opProp{"MMBINK", false, false, opArgModeK, opArgModeU, opModeABC},
	/*49*/ opProp{"UNM", false, true, opArgModeR, opArgModeN, opModeABC},
	/*50*/ opProp{"BNOT", false, true, opArgModeR, opArgModeN, opModeABC},
	/*51*/ opProp{"NOT", false, true, opArgModeR, opArgModeN, opModeABC},
	/*52*/ opProp{"LEN", false, true, opArgModeR, opArgModeN, opModeABC},
	/*53*/ opProp{"CONCAT", false, true, opArgModeR, opArgModeN, opModeABC},
	/*54*/ opProp{"CLOSE", false, false, opArgModeN, opArgModeN, opModeABC},
	/*55*/ opProp{"TBC", false, false, opArgModeN, opArgModeN, opModeABC},
	/*56*/ opProp{"JMP", false, false, opArgModeN, opArgModeN, opModeSJ},
	/*57*/ opProp{"EQ", true, false, opArgModeR, opArgModeR, opModeABC},
	/*58*/ opProp{"LT", true, false, opArgModeR, opArgModeR, opModeABC},
	/*59*/ opProp{"LE", true, false, opArgModeR, opArgModeR, opModeABC},
	/*60*/ opProp{"EQK", true, false, opArgModeK, opArgModeN, opModeABC},
	/*61*/ opProp{"EQI", true, false, opArgModeI, opArgModeN, opModeABC},
	/*62*/ opProp{"LTI", true, false, opArgModeI, opArgModeN, opModeABC},
	/*63*/ opProp{"LEI", true, false, opArgModeI, opArgModeN, opModeABC},
	/*64*/ opProp{"GTI", true, false, opArgModeI, opArgModeN, opModeABC},
	/*65*/ opProp{"GEI", true, false, opArgModeI, opArgModeN, opModeABC},
	/*66*/ opProp{"TEST", true, false, opArgModeR, opArgModeN, opModeABC},
	/*67*/ opProp{"TESTSET", true, true, opArgModeR, opArgModeN, opModeABC},
	/*68*/ opProp{"CALL", false, true, opArgModeU, opArgModeU, opModeABC},
	/*69*/ opProp{"TAILCALL", false, true, opArgModeU, opArgModeU, opModeABC},
	/*70*/ opProp{"RETURN", false, false, opArgModeU, opArgModeN, opModeABC},
	/*71*/ opProp{"RETURN0", false, false, opArgModeN, opArgModeN, opModeABC},
	/*72*/ opProp{"RETURN1", false, false, opArgModeU, opArgModeN, opModeABC},
	/*73*/ opProp{"FORLOOP", false, true, opArgModeN, opArgModeN, opModeABx},
	/*74*/ opProp{"FORPREP", false, true, opArgModeN, opArgModeN, opModeABx},
	/*75*/ opProp{"TFORPREP", false, false, opArgModeN, opArgModeN, opModeABx},
	/*76*/ opProp{"TFORCALL", false, false, opArgModeN, opArgModeU, opModeABC},
	/*77*/ opProp{"TFORLOOP", false, true, opArgModeN, opArgModeN, opModeABx},
	/*78*/ opProp{"SETLIST", false, false, opArgModeU, opArgModeU, opModevABC},
	/*79*/ opProp{"CLOSURE", false, true, opArgModeU, opArgModeN, opModeABx},
	/*80*/ opProp{"VARARG", false, true, opArgModeU, opArgModeN, opModeABC},
	/*81*/ opProp{"GETVARG", false, true, opArgModeR, opArgModeR, opModeABC},
	/*82*/ opProp{"ERRNNIL", false, false, opArgModeU, opArgModeN, opModeABx},
	/*83*/ opProp{"VARARGPREP", false, false, opArgModeN, opArgModeN, opModeABC},
	/*84*/ opProp{"EXTRAARG", false, false, opArgModeN, opArgModeN, opModeAx},
}

/* field accessors (lopcodes.h GETARG_x / SETARG_x / CREATE_x) {{{ */

func opGetOpCode(inst uint32) int {
	return int(inst & 0x7f)
}

func opSetOpCode(inst *uint32, opcode int) {
	*inst = (*inst &^ 0x7f) | uint32(opcode&0x7f)<<opPosOp
}

func opGetArgA(inst uint32) int {
	return int(inst>>opPosA) & 0xff
}

func opSetArgA(inst *uint32, arg int) {
	*inst = (*inst &^ (0xff << opPosA)) | uint32(arg&0xff)<<opPosA
}

func opGetArgB(inst uint32) int {
	return int(inst>>opPosB) & 0xff
}

func opSetArgB(inst *uint32, arg int) {
	*inst = (*inst &^ (0xff << opPosB)) | uint32(arg&0xff)<<opPosB
}

func opGetArgC(inst uint32) int {
	return int(inst>>opPosC) & 0xff
}

func opSetArgC(inst *uint32, arg int) {
	*inst = (*inst &^ (0xff << opPosC)) | uint32(arg&0xff)<<opPosC
}

func opGetArgvB(inst uint32) int {
	return int(inst>>opPosB) & opMaxArgsvB
}

func opSetArgvB(inst *uint32, arg int) {
	*inst = (*inst &^ (opMaxArgsvB << opPosB)) | uint32(arg&opMaxArgsvB)<<opPosB
}

func opGetArgvC(inst uint32) int {
	return int(inst>>22) & opMaxArgsvC
}

func opSetArgvC(inst *uint32, arg int) {
	*inst = (*inst &^ (opMaxArgsvC << 22)) | uint32(arg&opMaxArgsvC)<<22
}

func opGetArgk(inst uint32) int {
	return int(inst>>opPosK) & 1
}

func opSetArgk(inst *uint32, arg int) {
	*inst = (*inst &^ (1 << opPosK)) | uint32(arg&1)<<opPosK
}

func opGetArgBx(inst uint32) int {
	return int(inst>>opPosK) & opMaxArgBx
}

func opSetArgBx(inst *uint32, arg int) {
	*inst = (*inst &^ (opMaxArgBx << opPosK)) | uint32(arg&opMaxArgBx)<<opPosK
}

func opGetArgSbx(inst uint32) int {
	return opGetArgBx(inst) - opMaxArgSbx
}

func opSetArgSbx(inst *uint32, arg int) {
	opSetArgBx(inst, arg+opMaxArgSbx)
}

func opGetArgAx(inst uint32) int {
	return int(inst>>opPosA) & opMaxArgAx
}

func opSetArgAx(inst *uint32, arg int) {
	*inst = (*inst &^ (opMaxArgAx << opPosA)) | uint32(arg&opMaxArgAx)<<opPosA
}

func opGetArgsJ(inst uint32) int {
	return int(inst>>opPosA)&opMaxArgsJ - opOffsetSJ
}

func opSetArgsJ(inst *uint32, arg int) {
	*inst = (*inst &^ (opMaxArgsJ << opPosA)) | uint32(arg+opOffsetSJ)&opMaxArgsJ<<opPosA
}

func opCreateABC(op int, a int, b int, c int) uint32 {
	var inst uint32 = 0
	opSetOpCode(&inst, op)
	opSetArgA(&inst, a)
	opSetArgB(&inst, b)
	opSetArgC(&inst, c)
	return inst
}

func opCreateABCk(op int, a int, b int, c int, k int) uint32 {
	inst := opCreateABC(op, a, b, c)
	opSetArgk(&inst, k)
	return inst
}

func opCreatevABCk(op int, a int, b int, c int, k int) uint32 {
	var inst uint32 = 0
	opSetOpCode(&inst, op)
	opSetArgA(&inst, a)
	opSetArgvB(&inst, b)
	opSetArgvC(&inst, c)
	opSetArgk(&inst, k)
	return inst
}

func opCreateABx(op int, a int, bx int) uint32 {
	var inst uint32 = 0
	opSetOpCode(&inst, op)
	opSetArgA(&inst, a)
	opSetArgBx(&inst, bx)
	return inst
}

func opCreateAsBx(op int, a int, sbx int) uint32 {
	var inst uint32 = 0
	opSetOpCode(&inst, op)
	opSetArgA(&inst, a)
	opSetArgSbx(&inst, sbx)
	return inst
}

func opCreateAx(op int, ax int) uint32 {
	var inst uint32 = 0
	opSetOpCode(&inst, op)
	opSetArgAx(&inst, ax)
	return inst
}

func opCreateSJ(op int, sj int) uint32 {
	var inst uint32 = 0
	opSetOpCode(&inst, op)
	opSetArgsJ(&inst, sj)
	return inst
}

/* }}} */

/*
Compile-time K-operand marker: registers are 0..255; a K-operand index is
carried through the compiler as kMark|kidx (the 5.5 encoding has no RK bit —
the k flag lives in the instruction itself — so this convention is purely
internal to compile.go).
*/
const kMark = 1 << 16

func opIsK(value int) bool   { return value&kMark != 0 }
func opIndexK(value int) int { return value &^ kMark }
func opRkAsk(value int) int  { return value | kMark }
func opIndexOrReg(v int) int { return v &^ kMark }

/* exported accessors: the wasm backend decodes protos through these
(U2-implementation-plan §4 U2c — luawasm stops re-implementing the decode). */

func OpGetOpCode(inst uint32) int            { return opGetOpCode(inst) }
func OpGetArgA(inst uint32) int              { return opGetArgA(inst) }
func OpGetArgB(inst uint32) int              { return opGetArgB(inst) }
func OpGetArgC(inst uint32) int              { return opGetArgC(inst) }
func OpGetArgvB(inst uint32) int             { return opGetArgvB(inst) }
func OpGetArgvC(inst uint32) int             { return opGetArgvC(inst) }
func OpGetArgk(inst uint32) int              { return opGetArgk(inst) }
func OpGetArgBx(inst uint32) int             { return opGetArgBx(inst) }
func OpGetArgSbx(inst uint32) int            { return opGetArgSbx(inst) }
func OpGetArgAx(inst uint32) int             { return opGetArgAx(inst) }
func OpGetArgsJ(inst uint32) int             { return opGetArgsJ(inst) }
func OpCreateABCk(op, a, b, c, k int) uint32 { return opCreateABCk(op, a, b, c, k) }
func OpCreatevABCk(op, a, b, c, k int) uint32 {
	return opCreatevABCk(op, a, b, c, k)
}
func OpCreateABx(op, a, bx int) uint32   { return opCreateABx(op, a, bx) }
func OpCreateAsBx(op, a, sbx int) uint32 { return opCreateAsBx(op, a, sbx) }
func OpCreateAx(op, ax int) uint32       { return opCreateAx(op, ax) }
func OpCreateSJ(op, sj int) uint32       { return opCreateSJ(op, sj) }
func OpCodeMax() int                     { return opCodeMax }

func opToString(inst uint32) string {
	op := opGetOpCode(inst)
	if op > opCodeMax {
		return ""
	}
	prop := &(opProps[op])

	a := opGetArgA(inst)
	b := opGetArgB(inst)
	c := opGetArgC(inst)
	bx := opGetArgBx(inst)
	sbx := opGetArgSbx(inst)
	sj := opGetArgsJ(inst)
	k := opGetArgk(inst)

	buf := ""
	switch prop.Mode {
	case opModeABC:
		buf = fmt.Sprintf("%s      |  %d, %d, %d", prop.Name, a, b, c)
	case opModevABC:
		buf = fmt.Sprintf("%s      |  %d, %d, %d", prop.Name, a, opGetArgvB(inst), opGetArgvC(inst))
	case opModeABx:
		buf = fmt.Sprintf("%s      |  %d, %d", prop.Name, a, bx)
	case opModeAsBx:
		buf = fmt.Sprintf("%s      |  %d, %d", prop.Name, a, sbx)
	case opModeAx:
		buf = fmt.Sprintf("%s      |  %d", prop.Name, opGetArgAx(inst))
	case opModeSJ:
		buf = fmt.Sprintf("%s      |  %d", prop.Name, sj)
	}
	if opGetArgk(inst) != 0 {
		buf += fmt.Sprintf("; k=%v", k)
	}

	switch op {
	case OP_MOVE:
		buf += fmt.Sprintf("; R(%v) := R(%v)", a, b)
	case OP_LOADI:
		buf += fmt.Sprintf("; R(%v) := %v", a, sbx)
	case OP_LOADF:
		buf += fmt.Sprintf("; R(%v) := %v.0", a, sbx)
	case OP_LOADK:
		buf += fmt.Sprintf("; R(%v) := Kst(%v)", a, bx)
	case OP_LOADKX:
		buf += fmt.Sprintf("; R(%v) := Kst(%v)", a, opGetArgAx(inst))
	case OP_LOADFALSE:
		buf += fmt.Sprintf("; R(%v) := false", a)
	case OP_LFALSESKIP:
		buf += fmt.Sprintf("; R(%v) := false; pc++", a)
	case OP_LOADTRUE:
		buf += fmt.Sprintf("; R(%v) := true", a)
	case OP_LOADNIL:
		buf += fmt.Sprintf("; R(%v) := ... := R(%v) := nil", a, a+b)
	case OP_GETUPVAL:
		buf += fmt.Sprintf("; R(%v) := UpValue[%v]", a, b)
	case OP_SETUPVAL:
		buf += fmt.Sprintf("; UpValue[%v] := R(%v)", b, a)
	case OP_GETTABUP:
		buf += fmt.Sprintf("; R(%v) := UpValue[%v][Kst(%v):string]", a, b, c)
	case OP_GETTABLE:
		buf += fmt.Sprintf("; R(%v) := R(%v)[R(%v)]", a, b, c)
	case OP_GETI:
		buf += fmt.Sprintf("; R(%v) := R(%v)[%v]", a, b, c)
	case OP_GETFIELD:
		buf += fmt.Sprintf("; R(%v) := R(%v)[Kst(%v):string]", a, b, c)
	case OP_SETTABUP:
		buf += fmt.Sprintf("; UpValue[%v][Kst(%v):string] := RK(%v)", a, b, c)
	case OP_SETTABLE:
		buf += fmt.Sprintf("; R(%v)[R(%v)] := RK(%v)", a, b, c)
	case OP_SETI:
		buf += fmt.Sprintf("; R(%v)[%v] := RK(%v)", a, b, c)
	case OP_SETFIELD:
		buf += fmt.Sprintf("; R(%v)[Kst(%v):string] := RK(%v)", a, b, c)
	case OP_NEWTABLE:
		buf += fmt.Sprintf("; R(%v) := {} (size = vB,vC)", a)
	case OP_SELF:
		buf += fmt.Sprintf("; R(%v+1) := R(%v); R(%v) := R(%v)[Kst(%v):string]", a, b, a, b, c)
	case OP_ADDI:
		buf += fmt.Sprintf("; R(%v) := R(%v) + %v", a, b, sC2int(c))
	case OP_ADDK, OP_SUBK, OP_MULK, OP_MODK, OP_POWK, OP_DIVK, OP_IDIVK,
		OP_BANDK, OP_BORK, OP_BXORK:
		buf += fmt.Sprintf("; R(%v) := R(%v) op Kst(%v):number", a, b, c)
	case OP_SHLI:
		buf += fmt.Sprintf("; R(%v) := %v << R(%v)", a, sC2int(c), b)
	case OP_SHRI:
		buf += fmt.Sprintf("; R(%v) := R(%v) >> %v", a, b, sC2int(c))
	case OP_ADD, OP_SUB, OP_MUL, OP_MOD, OP_POW, OP_DIV, OP_IDIV,
		OP_BAND, OP_BOR, OP_BXOR, OP_SHL, OP_SHR:
		buf += fmt.Sprintf("; R(%v) := R(%v) op R(%v)", a, b, c)
	case OP_MMBIN:
		buf += fmt.Sprintf("; call TM(%v) over R(%v) and R(%v)", c, a, b)
	case OP_MMBINI:
		buf += fmt.Sprintf("; call TM(%v) over R(%v) and %v", c, a, sC2int(b))
	case OP_MMBINK:
		buf += fmt.Sprintf("; call TM(%v) over R(%v) and Kst(%v)", c, a, b)
	case OP_UNM:
		buf += fmt.Sprintf("; R(%v) := -R(%v)", a, b)
	case OP_BNOT:
		buf += fmt.Sprintf("; R(%v) := ~R(%v)", a, b)
	case OP_NOT:
		buf += fmt.Sprintf("; R(%v) := not R(%v)", a, b)
	case OP_LEN:
		buf += fmt.Sprintf("; R(%v) := length of R(%v)", a, b)
	case OP_CONCAT:
		buf += fmt.Sprintf("; R(%v) := R(%v).. ... ..R(%v)", a, a, a+b-1)
	case OP_CLOSE:
		buf += fmt.Sprintf("; close all upvalues up to (>=) R(%v)", a)
	case OP_TBC:
		buf += fmt.Sprintf("; mark variable R(%v) to be closed", a)
	case OP_JMP:
		buf += fmt.Sprintf("; pc+=%v", sj)
	case OP_EQ, OP_LT, OP_LE:
		buf += fmt.Sprintf("; if ((R(%v) op R(%v)) ~= %v) then pc++", a, b, k)
	case OP_EQK:
		buf += fmt.Sprintf("; if ((R(%v) == Kst(%v)) ~= %v) then pc++", a, b, k)
	case OP_EQI, OP_LTI, OP_LEI, OP_GTI, OP_GEI:
		buf += fmt.Sprintf("; if ((R(%v) op %v) ~= %v) then pc++", a, sC2int(b), k)
	case OP_TEST:
		buf += fmt.Sprintf("; if (not R(%v) == %v) then pc++", a, k)
	case OP_TESTSET:
		buf += fmt.Sprintf("; if (not R(%v) == %v) then pc++ else R(%v) := R(%v)", b, k, a, b)
	case OP_CALL:
		buf += fmt.Sprintf("; R(%v) ... R(%v+%v-2) := R(%v)(R(%v+1) ... R(%v+%v-1))", a, a, c, a, a, a, b)
	case OP_TAILCALL:
		buf += fmt.Sprintf("; return R(%v)(R(%v+1) ... R(%v+%v-1))", a, a, a, b)
	case OP_RETURN:
		buf += fmt.Sprintf("; return R(%v) ... R(%v+%v-2)", a, a, b)
	case OP_RETURN0:
		buf += "; return"
	case OP_RETURN1:
		buf += fmt.Sprintf("; return R(%v)", a)
	case OP_FORLOOP:
		buf += fmt.Sprintf("; update counters; if loop continues then pc-=%v", bx)
	case OP_FORPREP:
		buf += fmt.Sprintf("; prepare counters; if not to run then pc+=%v+1", bx)
	case OP_TFORPREP:
		buf += fmt.Sprintf("; create upvalue for R(%v+3); pc+=%v", a, bx)
	case OP_TFORCALL:
		buf += fmt.Sprintf("; R(%v+4), ... ,R(%v+3+%v) := R(%v)(R(%v+1), R(%v+2))", a, a, c, a, a, a)
	case OP_TFORLOOP:
		buf += fmt.Sprintf("; if R(%v+3) ~= nil then pc-=%v", a, bx)
	case OP_SETLIST:
		buf += fmt.Sprintf("; R(%v)[%v+i] := R(%v+i), 1 <= i <= %v", a, opGetArgvC(inst), a, opGetArgvB(inst))
	case OP_CLOSURE:
		buf += fmt.Sprintf("; R(%v) := closure(KPROTO[%v])", a, bx)
	case OP_VARARG:
		buf += fmt.Sprintf("; R(%v), ..., R(%v+%v-2) = varargs", a, a, c)
	case OP_GETVARG:
		buf += fmt.Sprintf("; R(%v) := R(%v)[R(%v)] (vararg parameter)", a, b, c)
	case OP_ERRNNIL:
		buf += fmt.Sprintf("; raise error if R(%v) ~= nil", a)
	case OP_VARARGPREP:
		/* nothing to do */
	case OP_EXTRAARG:
		buf += fmt.Sprintf("; extra arg %v", opGetArgAx(inst))
	}
	return buf
}
