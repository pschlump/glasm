package lua

import (
	"math"
)

/* math library — the stock 5.5.1 surface (U3b).
 *
 * Ported from runtime/lua55/src/lmathlib.c against the clua55 oracle:
 * the integer-exact family (abs/floor/ceil/fmod/modf return integers
 * when their inputs and results are integral — pushnumint), the subtype
 * family (tointeger/type/ult/maxinteger/mininteger, fail = false), and
 * xoshiro256** math.random with the harness determinism pin: the oracle
 * blob builds with -D'luai_makeseed()=0u' (build55.sh), so the library
 * open seed and the randomseed() no-arg seed are 0 on every engine.
 *
 * Removed per D8 (5.3 removals; stock compiles them only under
 * LUA_COMPAT_MATHLIB): pow log10 atan2 cosh sinh tanh — and the 5.1-era
 * mod alias. frexp/ldexp stay: 5.5.1 registers them again (the parent
 * plan's Appendix B removal list predates that; the oracle is the
 * authority — math.lua:688-697 exercises both). */

// toIntegerX ports lua_tointegerx (F2Ieq): integers pass, floats must
// have an exact integer value, numeric strings convert through the full
// numeral grammar first.
func toIntegerX(v LValue) (int64, bool) {
	switch n := v.(type) {
	case LInteger:
		return int64(n), true
	case LNumber:
		return floatToIntExact(float64(n))
	case LString:
		if num, err := parseNumber(string(n)); err == nil {
			switch c := num.(type) {
			case LInteger:
				return int64(c), true
			case LNumber:
				return floatToIntExact(float64(c))
			}
		}
	}
	return 0, false
}

// checkInteger ports luaL_checkinteger: toIntegerX or the stock error
// split — a number without an integer representation says so, anything
// else is a type error.
func checkInteger(L *LState, n int) int64 {
	if v, ok := toIntegerX(L.Get(n)); ok {
		return v
	}
	if isNumberValue(L.Get(n)) {
		L.ArgError(n, "number has no integer representation")
	}
	L.TypeError(n, LTNumber)
	return 0
}

// optInteger ports luaL_optinteger.
func optInteger(L *LState, n int, d int64) int64 {
	if v := L.Get(n); v == LNil {
		return d
	}
	return checkInteger(L, n)
}

// pushNumInt ports pushnumint: a float result lands as an integer when
// it fits, else stays a float.
func pushNumInt(d float64) LValue {
	if n, ok := floatToIntExact(d); ok {
		return LInteger(n)
	}
	return LNumber(d)
}

// rngState is the xoshiro256** state (stock RanState, lmathlib.c) over
// the 64-bit Rand64 path — word-width-agnostic math.
type rngState [4]uint64

func rotl64(x uint64, n uint) uint64 {
	return x<<n | x>>(64-n)
}

// next ports nextrand verbatim.
func (s *rngState) next() uint64 {
	state0 := s[0]
	state1 := s[1]
	state2 := s[2] ^ state0
	state3 := s[3] ^ state1
	res := rotl64(state1*5, 7) * 9
	s[0] = state0 ^ state3
	s[1] = state1 ^ state2
	s[2] = state2 ^ state1<<17
	s[3] = rotl64(state3, 45)
	return res
}

// randFloat ports I2d for double (FIGS = 53: the top 53 bits scaled into
// [0,1) with the two's-complement correction for negative sx).
func randFloat(x uint64) float64 {
	sx := int64(x >> 11)                                // shift64_FIG = 64 - 53
	res := float64(sx) * (0.5 / float64(uint64(1)<<52)) // scaleFIG = 2^-53
	if sx < 0 {
		res += 1.0
	}
	return res
}

// project ports project: spread n's bits into the smallest covering
// Mersenne number, then rejection-sample until the masked draw lands in
// [0,n].
func (s *rngState) project(ran uint64, n uint64) uint64 {
	lim := n
	for sh := uint(1); lim&(lim+1) != 0; sh *= 2 {
		lim |= lim >> sh
	}
	for {
		ran &= lim
		if ran <= n {
			return ran
		}
		ran = s.next()
	}
}

// setseed ports setseed (0xff in slot 1 avoids the zero state; 16 draws
// spread the seed).
func (s *rngState) setseed(n1, n2 uint64) {
	s[0] = n1
	s[1] = 0xff
	s[2] = n2
	s[3] = 0
	for i := 0; i < 16; i++ {
		s.next()
	}
}

func OpenMath(L *LState) int {
	mod := L.RegisterModule(MathLibName, mathFuncs).(*LTable)
	mod.RawSetString("pi", LNumber(math.Pi))
	mod.RawSetString("huge", LNumber(math.Inf(1)))
	mod.RawSetString("maxinteger", LInteger(math.MaxInt64))
	mod.RawSetString("mininteger", LInteger(math.MinInt64))
	// stock setrandfunc: setseed(luaL_makeseed(L), 0) — the blob pin
	// makes that seed 0 (build55.sh), which the interp mirrors so
	// unseeded draws are byte-exact against the oracle.
	L.G.randState.setseed(0, 0)
	L.Push(mod)
	return 1
}

var mathFuncs = map[string]LGFunction{
	"abs":        mathAbs,
	"acos":       mathAcos,
	"asin":       mathAsin,
	"atan":       mathAtan,
	"ceil":       mathCeil,
	"cos":        mathCos,
	"deg":        mathDeg,
	"exp":        mathExp,
	"tointeger":  mathToInteger,
	"floor":      mathFloor,
	"fmod":       mathFmod,
	"frexp":      mathFrexp,
	"ult":        mathUlt,
	"ldexp":      mathLdexp,
	"log":        mathLog,
	"max":        mathMax,
	"min":        mathMin,
	"modf":       mathModf,
	"rad":        mathRad,
	"sin":        mathSin,
	"sqrt":       mathSqrt,
	"tan":        mathTan,
	"type":       mathType,
	"random":     mathRandom,
	"randomseed": mathRandomseed,
}

func mathAbs(L *LState) int {
	if v, ok := L.Get(1).(LInteger); ok {
		n := int64(v)
		if n < 0 {
			n = int64(0 - uint64(n)) // abs(mininteger) == mininteger
		}
		L.Push(LInteger(n))
		return 1
	}
	L.Push(LNumber(math.Abs(float64(L.CheckNumber(1)))))
	return 1
}

func mathAcos(L *LState) int {
	L.Push(LNumber(math.Acos(float64(L.CheckNumber(1)))))
	return 1
}

func mathAsin(L *LState) int {
	L.Push(LNumber(math.Asin(float64(L.CheckNumber(1)))))
	return 1
}

// mathAtan is 5.3+ math.atan(y [, x]) — the old atan2 folded in.
func mathAtan(L *LState) int {
	y := float64(L.CheckNumber(1))
	x := 1.0
	if v := L.Get(2); v != LNil {
		x = float64(L.CheckNumber(2))
	}
	L.Push(LNumber(math.Atan2(y, x)))
	return 1
}

func mathCeil(L *LState) int {
	if v, ok := L.Get(1).(LInteger); ok {
		L.Push(v) // an integer is its own ceiling
		return 1
	}
	L.Push(pushNumInt(math.Ceil(float64(L.CheckNumber(1)))))
	return 1
}

func mathCos(L *LState) int {
	L.Push(LNumber(math.Cos(float64(L.CheckNumber(1)))))
	return 1
}

func mathDeg(L *LState) int {
	L.Push(LNumber(float64(L.CheckNumber(1)) * (180.0 / math.Pi)))
	return 1
}

func mathExp(L *LState) int {
	L.Push(LNumber(math.Exp(float64(L.CheckNumber(1)))))
	return 1
}

// mathToInteger is stock math_toint: lua_tointegerx semantics (numeric
// strings convert, integral floats convert) or fail. 5.5's fail is nil
// (luaL_pushfail; LUA_FAILISFALSE is not defined in the oracle build).
func mathToInteger(L *LState) int {
	if v, ok := toIntegerX(L.Get(1)); ok {
		L.Push(LInteger(v))
		return 1
	}
	L.CheckAny(1)
	L.Push(LNil)
	return 1
}

func mathFloor(L *LState) int {
	if v, ok := L.Get(1).(LInteger); ok {
		L.Push(v) // an integer is its own floor
		return 1
	}
	L.Push(pushNumInt(math.Floor(float64(L.CheckNumber(1)))))
	return 1
}

// mathFmod: int/int is C '%' (truncated, sign of the dividend) with the
// 0/-1 special cases; any float operand is float fmod.
func mathFmod(L *LState) int {
	if a, ok1 := L.Get(1).(LInteger); ok1 {
		if b, ok2 := L.Get(2).(LInteger); ok2 {
			d := int64(b)
			if d == 0 || d == -1 {
				if d == 0 {
					L.ArgError(2, "zero")
				}
				L.Push(LInteger(0)) // mininteger % -1 avoids the overflow
				return 1
			}
			L.Push(LInteger(int64(a) % d))
			return 1
		}
	}
	L.Push(LNumber(math.Mod(float64(L.CheckNumber(1)), float64(L.CheckNumber(2)))))
	return 1
}

func mathFrexp(L *LState) int {
	x := float64(L.CheckNumber(1))
	m, e := math.Frexp(x)
	L.Push(LNumber(m))
	L.Push(LInteger(e))
	return 2
}

// mathUlt: unsigned comparison.
func mathUlt(L *LState) int {
	a := checkInteger(L, 1)
	b := checkInteger(L, 2)
	L.Push(LBool(uint64(a) < uint64(b)))
	return 1
}

func mathLdexp(L *LState) int {
	x := float64(L.CheckNumber(1))
	ep := checkInteger(L, 2)
	L.Push(LNumber(math.Ldexp(x, int(ep))))
	return 1
}

// mathLog: natural log, or log in an explicit base (2 and 10 have their
// own paths like stock).
func mathLog(L *LState) int {
	x := float64(L.CheckNumber(1))
	var res float64
	if v := L.Get(2); v == LNil {
		res = math.Log(x)
	} else {
		base := float64(L.CheckNumber(2))
		switch base {
		case 2.0:
			res = math.Log2(x)
		case 10.0:
			res = math.Log10(x)
		default:
			res = math.Log(x) / math.Log(base)
		}
	}
	L.Push(LNumber(res))
	return 1
}

// mathMax/min walk lua_compare(LUA_OPLT) over the argument list —
// subtype-aware, first-index-wins on ties (stock's index arithmetic).
func mathMax(L *LState) int {
	n := L.GetTop()
	if n < 1 {
		L.ArgError(1, "value expected")
	}
	imax := 1
	for i := 2; i <= n; i++ {
		// Go context: the dispatch state is always mmSync (discarded)
		if v, _ := lessThan(L, L.Get(imax), L.Get(i)); v {
			imax = i
		}
	}
	L.Push(L.Get(imax))
	return 1
}

func mathMin(L *LState) int {
	n := L.GetTop()
	if n < 1 {
		L.ArgError(1, "value expected")
	}
	imin := 1
	for i := 2; i <= n; i++ {
		// Go context: the dispatch state is always mmSync (discarded)
		if v, _ := lessThan(L, L.Get(i), L.Get(imin)); v {
			imin = i
		}
	}
	L.Push(L.Get(imin))
	return 1
}

func mathModf(L *LState) int {
	if v, ok := L.Get(1).(LInteger); ok {
		L.Push(v) // an integer is its own integer part
		L.Push(LNumber(0))
		return 2
	}
	n := float64(L.CheckNumber(1))
	var ip float64 // integer part, rounded toward zero
	if n < 0 {
		ip = math.Ceil(n)
	} else {
		ip = math.Floor(n)
	}
	L.Push(pushNumInt(ip))
	if n == ip { // covers inf/-inf too
		L.Push(LNumber(0.0))
	} else {
		L.Push(LNumber(n - ip))
	}
	return 2
}

func mathRad(L *LState) int {
	L.Push(LNumber(float64(L.CheckNumber(1)) * (math.Pi / 180.0)))
	return 1
}

func mathSin(L *LState) int {
	L.Push(LNumber(math.Sin(float64(L.CheckNumber(1)))))
	return 1
}

func mathSqrt(L *LState) int {
	L.Push(LNumber(math.Sqrt(float64(L.CheckNumber(1)))))
	return 1
}

func mathTan(L *LState) int {
	L.Push(LNumber(math.Tan(float64(L.CheckNumber(1)))))
	return 1
}

// mathType: "integer" / "float" for numbers, fail (nil) for the rest.
func mathType(L *LState) int {
	switch L.Get(1).(type) {
	case LInteger:
		L.Push(LString("integer"))
	case LNumber:
		L.Push(LString("float"))
	default:
		L.CheckAny(1)
		L.Push(LNil)
	}
	return 1
}

// mathRandom ports math_random: no args → float in [0,1); a single 0 →
// a full 64-bit integer; bounds → low + project(draw, up-low).
func mathRandom(L *LState) int {
	state := &L.G.randState
	rv := state.next()
	var low, up int64
	switch L.GetTop() {
	case 0:
		L.Push(LNumber(randFloat(rv)))
		return 1
	case 1:
		low = 1
		up = checkInteger(L, 1)
		if up == 0 { // a single 0: full random integer
			L.Push(LInteger(int64(rv)))
			return 1
		}
	case 2:
		low = checkInteger(L, 1)
		up = checkInteger(L, 2)
	default:
		L.RaiseError("wrong number of arguments")
	}
	if low > up {
		L.ArgError(1, "interval is empty")
	}
	p := state.project(rv, uint64(up)-uint64(low))
	L.Push(LInteger(int64(p + uint64(low))))
	return 1
}

// mathRandomseed ports math_randomseed: (x[,y]) re-seeds and returns
// both seeds; the no-arg form derives n1 from makeseed (0 under the
// harness pin) and n2 from the current stream.
func mathRandomseed(L *LState) int {
	state := &L.G.randState
	var n1, n2 uint64
	if L.GetTop() == 0 {
		n1 = 0 // luai_makeseed() = 0u (build55.sh determinism pin)
		n2 = state.next()
	} else {
		n1 = uint64(checkInteger(L, 1))
		n2 = uint64(optInteger(L, 2, 0))
	}
	state.setseed(n1, n2)
	L.Push(LInteger(int64(n1)))
	L.Push(LInteger(int64(n2)))
	return 2
}

//
