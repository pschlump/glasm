package lua

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"unsafe"
)

/* string.format — the stock 5.5.1 surface (U3f), ported from the
 * str_format section of runtime/lua55/src/lstrlib.c against the clua55
 * oracle (the build55.sh C99 target: lua_number2strx is the libc %a, so
 * hex floats carry full modifier support — precision, width and the
 * flag set — not the num2straux fallback).
 *
 * The conversion scanner is byte-faithful to the C: getformat spans
 * "-+#0 123456789." plus one character (the NUL when the span runs to
 * the end of the format string — the trailing lone '%'), checkformat
 * bounds every conversion to two flag digits and two width/precision
 * digits ("invalid conversion specification" past that, "invalid
 * format (too long)" past 21 span characters), and each verb keeps the
 * C's ordering of argument conversion against spec validation, which is
 * observable in the error texts.
 *
 * The libc spellings the Go stdlib does not produce are done here:
 * inf/nan ("inf", "-inf", "nan", "-nan"; the '0' flag does not
 * zero-pad them), '#' keeping the decimal point at precision zero,
 * %.Nf/%.Ne/%.Ng correct rounding (strconv — correctly rounded, like
 * the oracle libc), %a/%A with round-half-even mantissa truncation,
 * stripped-zero default precision ("0x1p+1", never "0x1.000..0p+1"),
 * an unpadded "p±d" exponent, and zero padding after the 0x. The %s
 * number path is luaO_tostringbuff (%.15g, re-read, %.17g, ".0" when
 * the result looks like an integer) — the interpreter's own tostring
 * stays Go-dialect; this is the format/tolstring-internal spelling.
 *
 * %p prints "(null)" for the non-reference types (numbers, booleans,
 * nil — lua_topointer is NULL there); strings get a content-derived
 * pseudo-address (equal content, equal output) and the reference types
 * get their Go pointer. Known divergence: stock's long strings (past
 * the intern boundary) are distinct objects, so two equal 300-byte
 * strings format differently there; Go strings are values and cannot
 * distinguish them (strings.lua:198). */

const (
	// frmtSpanChars is getformat's span set: the float flag family plus
	// the digits and the precision dot.
	frmtSpanChars = "-+#0 123456789."
	// frmtMaxFormat is MAX_FORMAT; the too-long check leaves room for
	// '%', '\0' and a length modifier.
	frmtMaxFormat = 32

	flagSetF = "-+#0 " // a, A, e, E, f, g, G
	flagSetI = "-+0 "  // d, i
	flagSetU = "-0"    // u
	flagSetX = "-#0"   // o, x, X
	flagSetC = "-"     // c, p, s
)

// convSpec is one parsed conversion: the flags that survive into the
// libc (plus the width/precision checkformat admits).
type convSpec struct {
	minus, plus, space, sharp, zero bool
	width                           int
	hasDot                          bool
	prec                            int
}

// frmtIsDigit and frmtIsAlpha are the C locale character classes.
func frmtIsDigit(c byte) bool { return c >= '0' && c <= '9' }

func frmtIsAlpha(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// frmtSkip2Digits ports get2digits: consume at most two digits.
func frmtSkip2Digits(s string) string {
	if len(s) > 0 && frmtIsDigit(s[0]) {
		s = s[1:]
		if len(s) > 0 && frmtIsDigit(s[0]) {
			s = s[1:]
		}
	}
	return s
}

// checkFormat ports checkformat: after the accepted flags, a width may
// not start with '0' (it is a flag), width and precision are at most
// two digits, and the last character must be a letter. On failure the
// whole conversion is echoed back.
func checkFormat(L *LState, form, flags string, precision bool) {
	spec := form[1:]
	for len(spec) > 0 && strings.IndexByte(flags, spec[0]) >= 0 {
		spec = spec[1:]
	}
	if len(spec) == 0 || spec[0] != '0' { // width cannot start with '0'
		spec = frmtSkip2Digits(spec)
		if len(spec) > 0 && spec[0] == '.' && precision {
			spec = frmtSkip2Digits(spec[1:])
		}
	}
	if len(spec) == 0 || !frmtIsAlpha(spec[0]) {
		L.RaiseError("invalid conversion specification: '%s'", form)
	}
}

// getFormat ports getformat: span flags/digits/dot from frmt[i:],
// include the following character (the conversion — or the terminating
// NUL when the span reaches the end). Returns the form ("%..."), the
// conversion character (0 at the end of the format string) and the
// index just past it.
func getFormat(L *LState, frmt string, i int) (string, byte, int) {
	j := i
	for j < len(frmt) && strings.IndexByte(frmtSpanChars, frmt[j]) >= 0 {
		j++
	}
	if j-i+1 >= frmtMaxFormat-10 {
		L.RaiseError("invalid format (too long)")
	}
	end := j + 1
	if end > len(frmt) {
		end = len(frmt)
	}
	var verb byte
	if j < len(frmt) {
		verb = frmt[j]
	}
	return "%" + frmt[i:end], verb, j + 1
}

// parseConvSpec reads flags, a two-digit width, and an optional dot plus
// two-digit precision out of a validated form. checkformat has already
// bounded the digits, so the caps below are defensive only.
func parseConvSpec(form string) convSpec {
	var sp convSpec
	i := 1
	for i < len(form) {
		switch form[i] {
		case '-':
			sp.minus = true
		case '+':
			sp.plus = true
		case ' ':
			sp.space = true
		case '#':
			sp.sharp = true
		case '0':
			sp.zero = true
		default:
			goto flagsDone
		}
		i++
	}
flagsDone:
	for i < len(form) && frmtIsDigit(form[i]) && sp.width < 100 {
		sp.width = sp.width*10 + int(form[i]-'0')
		i++
	}
	if i < len(form) && form[i] == '.' {
		sp.hasDot = true
		i++
		for i < len(form) && frmtIsDigit(form[i]) && sp.prec < 100 {
			sp.prec = sp.prec*10 + int(form[i]-'0')
			i++
		}
	}
	return sp
}

// frmtFinishWidth applies width padding: '-' left-justifies, else the
// '0' flag (zeroOK) pads with zeros after the sign/prefix, else spaces.
func frmtFinishWidth(prefix, body string, sp convSpec, zeroOK bool) string {
	s := prefix + body
	if sp.width <= len(s) {
		return s
	}
	pad := sp.width - len(s)
	switch {
	case sp.minus:
		return s + strings.Repeat(" ", pad)
	case sp.zero && zeroOK:
		return prefix + strings.Repeat("0", pad) + body
	default:
		return strings.Repeat(" ", pad) + s
	}
}

// frmtSpecialFloat renders inf/nan for the float conversions: the libc
// spellings (uppercase for A/E/G), '+'/' ' flags applied, the '0' flag
// ignored (the oracle pads with spaces), width honored.
func frmtSpecialFloat(sp convSpec, verb byte, f float64) string {
	var s string
	switch {
	case math.IsNaN(f):
		s = "nan"
		if math.Signbit(f) {
			s = "-" + s
		}
	case math.IsInf(f, 1):
		s = "inf"
	default: // -inf
		s = "-inf"
	}
	if verb == 'A' || verb == 'E' || verb == 'G' {
		s = strings.ToUpper(s)
	}
	if s[0] != '-' {
		if sp.plus {
			s = "+" + s
		} else if sp.space {
			s = " " + s
		}
	}
	return frmtFinishWidth("", s, sp, false)
}

// frmtFormatGFloat is the C %g algorithm: P = precision (0 means 1);
// with X the decimal exponent of the value rounded to P significant
// digits, X in [-4, P) selects style f with precision P-1-X, else style
// e with precision P-1; then, unless '#', trailing fraction zeros and a
// bare point go away.
func frmtFormatGFloat(f float64, prec int, sharp bool) string {
	p := prec
	if p == 0 {
		p = 1
	}
	es := strconv.FormatFloat(f, 'e', p-1, 64)
	x, _ := strconv.Atoi(es[strings.IndexByte(es, 'e')+1:])
	var body string
	if x < -4 || x >= p {
		body = es
	} else {
		body = strconv.FormatFloat(f, 'f', p-1-x, 64)
	}
	mant, exp := body, ""
	if i := strings.IndexByte(body, 'e'); i >= 0 {
		mant, exp = body[:i], body[i:]
	}
	if sharp {
		if !strings.Contains(mant, ".") {
			mant += "."
		}
	} else if strings.Contains(mant, ".") {
		mant = strings.TrimRight(mant, "0")
		if strings.HasSuffix(mant, ".") {
			mant = mant[:len(mant)-1]
		}
	}
	return mant + exp
}

// frmtFormatFloatConv renders %f/%e/%E/%g/%G. Rounding itself is
// strconv's (correctly rounded, like the oracle libc); this layer adds
// the sign flags, '#' keeping the point at precision zero, uppercase
// verbs and width padding.
func frmtFormatFloatConv(sp convSpec, verb byte, f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return frmtSpecialFloat(sp, verb, f)
	}
	prec := 6
	if sp.hasDot {
		prec = sp.prec
	}
	var body string
	switch verb {
	case 'f':
		body = strconv.FormatFloat(f, 'f', prec, 64)
	case 'e', 'E':
		body = strconv.FormatFloat(f, 'e', prec, 64)
	default: // 'g', 'G'
		body = frmtFormatGFloat(f, prec, sp.sharp)
	}
	neg := strings.HasPrefix(body, "-")
	body = strings.TrimPrefix(body, "-")
	if sp.sharp && prec == 0 {
		switch verb {
		case 'f':
			body += "."
		case 'e', 'E':
			i := strings.IndexByte(body, 'e')
			body = body[:i] + "." + body[i:]
		}
	}
	if verb == 'E' || verb == 'G' {
		body = strings.ToUpper(body)
	}
	return frmtFinishWidth(frmtSignPrefix(neg, sp), body, sp, true)
}

// frmtSignPrefix is the sign spelling: '-' wins, then '+', then ' '.
func frmtSignPrefix(neg bool, sp convSpec) string {
	switch {
	case neg:
		return "-"
	case sp.plus:
		return "+"
	case sp.space:
		return " "
	}
	return ""
}

// frmtFormatHexFloat renders %a/%A with the oracle libc's semantics:
// normalized "1.<13 hex digits>p±d" (subnormals normalized, zero is
// "0x0p+0"); an explicit precision truncates the fraction with
// round-half-even on the nibble boundary (a carry may lift the leading
// digit to 2 without touching the exponent); the default precision is
// the exact expansion with trailing zeros stripped (no point at all
// when nothing survives — but '#' keeps the point); the exponent is
// unpadded; '0' pads after the sign and "0x". 'A' uppercases
// everything.
func frmtFormatHexFloat(sp convSpec, verb byte, f float64) string {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return frmtSpecialFloat(sp, verb, f)
	}
	bits := math.Float64bits(f)
	expField := int(bits>>52) & 0x7ff
	mant := bits & (1<<52 - 1)
	var e int
	switch {
	case expField == 0 && mant == 0:
		e = 0 // zero prints as 0x0p+0
	case expField == 0: // subnormal: renormalize below 2^-1022
		e = -1022
		for mant < 1<<52 {
			mant <<= 1
			e--
		}
	default:
		e = expField - 1023
		mant |= 1 << 52
	}
	first := int(mant >> 52)
	frac := mant & (1<<52 - 1)
	np := 13
	if sp.hasDot {
		np = sp.prec
	}
	var fracDigits string
	if np >= 13 {
		fracDigits = fmt.Sprintf("%013x", frac)
		if np > 13 {
			fracDigits += strings.Repeat("0", np-13)
		}
	} else {
		shift := uint(52 - 4*np)
		kept := frac >> shift
		dropped := frac & (uint64(1)<<shift - 1)
		half := uint64(1) << (shift - 1)
		lastDigit := uint64(first)<<uint(4*np) | kept
		if dropped > half || (dropped == half && lastDigit&1 == 1) {
			if kept++; kept >= uint64(1)<<uint(4*np) {
				kept = 0
				first++
			}
		}
		if np == 0 {
			fracDigits = ""
		} else {
			fracDigits = fmt.Sprintf("%0*x", np, kept)
		}
	}
	if !sp.hasDot {
		fracDigits = strings.TrimRight(fracDigits, "0")
	}
	body := strconv.FormatInt(int64(first), 16)
	if len(fracDigits) > 0 || sp.sharp {
		body += "." + fracDigits
	}
	body += fmt.Sprintf("p%+d", e)
	prefix := "0x"
	if verb == 'A' {
		prefix = "0X"
		body = strings.ToUpper(body)
	}
	return frmtFinishWidth(frmtSignPrefix(bits>>63 != 0, sp)+prefix, body, sp, true)
}

// frmtFormatIntConv renders %c-excluded integer verbs: d/i signed, the
// rest unsigned over the two's-complement bits. Precision is the
// minimum digit count (0 with value 0 prints nothing), '#' prefixes 0x
// for nonzero x/X and forces a leading 0 for o, and '0' is ignored
// once a precision is given.
func frmtFormatIntConv(sp convSpec, verb byte, n int64) string {
	neg := verb == 'd' || verb == 'i'
	var mag uint64
	if neg {
		neg = n < 0
		if neg {
			mag = -uint64(n)
		} else {
			mag = uint64(n)
		}
	} else {
		mag = uint64(n)
	}
	base := 10
	switch verb {
	case 'o':
		base = 8
	case 'x', 'X':
		base = 16
	}
	digits := strconv.FormatUint(mag, base)
	prec := 1
	if sp.hasDot {
		prec = sp.prec
	}
	if prec == 0 && mag == 0 {
		digits = ""
	} else if len(digits) < prec {
		digits = strings.Repeat("0", prec-len(digits)) + digits
	}
	if verb == 'X' {
		digits = strings.ToUpper(digits)
	}
	prefix := ""
	if sp.sharp {
		switch verb {
		case 'o':
			if len(digits) == 0 || digits[0] != '0' {
				digits = "0" + digits
			}
		case 'x':
			if mag != 0 {
				prefix = "0x"
			}
		case 'X':
			if mag != 0 {
				prefix = "0X"
			}
		}
	}
	sign := ""
	if neg {
		sign = "-"
	} else if sp.plus {
		sign = "+"
	} else if sp.space {
		sign = " "
	}
	return frmtFinishWidth(sign+prefix, digits, sp, sp.zero && !sp.hasDot)
}

// numberToStringStock ports tostringbuffFloat (lobject.c): %.15g, read
// back, re-rendered %.17g when the round-trip loses the value; ".0"
// appended when the result is all of [-0123456789]. inf/nan get the
// libc spellings (a sign-bit NaN is "-nan"). The interpreter's own
// number tostring is unaffected — this is the string.format/tolstring
// spelling.
func numberToStringStock(f float64) string {
	if math.IsNaN(f) {
		if math.Signbit(f) {
			return "-nan"
		}
		return "nan"
	}
	if math.IsInf(f, 1) {
		return "inf"
	}
	if math.IsInf(f, -1) {
		return "-inf"
	}
	s := frmtFormatGFloat(f, 15, false)
	if v, err := strconv.ParseFloat(s, 64); err != nil || v != f {
		s = frmtFormatGFloat(f, 17, false)
	}
	if strings.Trim(s, "-0123456789") == "" {
		s += ".0"
	}
	return s
}

// frmtToLString ports luaL_tolstring (lauxlib.c): __tostring (its
// result must be a string or number — numbers render through the stock
// float spelling), then the type defaults — numbers via
// luaO_tostringbuff, "true"/"false"/"nil", and __name-or-typename plus
// a pseudo-address for the reference types.
func frmtToLString(L *LState, v LValue) string {
	if fn, ok := L.metaOp1(v, "__tostring").(*LFunction); ok {
		L.Push(fn)
		L.Push(v)
		L.Call(1, 1)
		res := L.reg.Pop()
		switch r := res.(type) {
		case LString:
			return string(r)
		case LInteger:
			return strconv.FormatInt(int64(r), 10)
		case LNumber:
			return numberToStringStock(float64(r))
		}
		L.RaiseError("'__tostring' must return a string")
	}
	switch t := v.(type) {
	case LString:
		return string(t)
	case LInteger:
		return strconv.FormatInt(int64(t), 10)
	case LNumber:
		return numberToStringStock(float64(t))
	case LBool:
		if bool(t) {
			return "true"
		}
		return "false"
	case *LNilType:
		return "nil"
	default:
		kind := v.Type().String()
		if name, ok := L.GetMetaField(v, "__name").(LString); ok {
			kind = string(name)
		}
		return kind + ": " + frmtPointerString(v)
	}
}

// frmtStringHash64 is FNV-1a: the content-derived pseudo-address for
// strings under %p (stock internalizes — equal content, one pointer).
func frmtStringHash64(s string) uint64 {
	h := uint64(14695981039346656037)
	for i := 0; i < len(s); i++ {
		h ^= uint64(s[i])
		h *= 1099511628211
	}
	return h
}

// frmtPointerString is lua_pointer2str for the reference types.
// frmtPointerString renders %p. Strings model stock's interning duality
// (ledger row: string identity is unobservable in the Go value model —
// this is the honest approximation): short strings (<= LUAI_MAXSHORTLEN
// 40) are internalized in stock, so content-derived pseudo-addresses
// match the interning observably (two equal short strings format
// equal); long strings are fresh allocations, so the backing-array
// pointer (distinct per creation, stable per value) models them.
func frmtPointerString(v LValue) string {
	switch t := v.(type) {
	case LString:
		if len(t) <= 40 {
			return "0x" + strconv.FormatUint(frmtStringHash64(string(t)), 16)
		}
		return fmt.Sprintf("%p", unsafe.StringData(string(t)))
	case *LTable:
		return fmt.Sprintf("%p", t)
	case *LFunction:
		return fmt.Sprintf("%p", t)
	case *LUserData:
		return fmt.Sprintf("%p", t)
	case *LState:
		return fmt.Sprintf("%p", t)
	case LChannel:
		return fmt.Sprintf("%p", chan LValue(t))
	}
	return "0x0" // not reached for reference types
}

// frmtAddQuoted ports addquoted: double quotes, backslash escapes for
// ", \ and newline, \ddd for the other control bytes (zero-padded only
// when the next byte is a digit, so reading the result back cannot
// merge into a longer escape), raw bytes elsewhere.
func frmtAddQuoted(b *strings.Builder, s string) {
	b.WriteByte('"')
	for k := 0; k < len(s); k++ {
		c := s[k]
		switch {
		case c == '"' || c == '\\' || c == '\n':
			b.WriteByte('\\')
			b.WriteByte(c)
		case c < 0x20 || c == 0x7f:
			next := byte(0) // the C reads the NUL terminator here
			if k+1 < len(s) {
				next = s[k+1]
			}
			if frmtIsDigit(next) {
				fmt.Fprintf(b, "\\%03d", c)
			} else {
				fmt.Fprintf(b, "\\%d", c)
			}
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}

// frmtQuoteFloat ports quotefloat: hex float (full precision, the
// default stripped form reads back exactly), inf as ±1e9999, NaN as
// (0/0).
func frmtQuoteFloat(f float64) string {
	switch {
	case math.IsInf(f, 1):
		return "1e9999"
	case math.IsInf(f, -1):
		return "-1e9999"
	case math.IsNaN(f):
		return "(0/0)"
	}
	return frmtFormatHexFloat(convSpec{}, 'a', f)
}

// frmtAddLiteral ports addliteral (the %q payload): strings quoted,
// integers decimal (mininteger in hex — the decimal form overflows),
// floats via quotefloat, booleans/nil via tolstring, everything else
// has no literal form.
func frmtAddLiteral(L *LState, b *strings.Builder, arg int) {
	switch v := L.Get(arg).(type) {
	case LString:
		frmtAddQuoted(b, string(v))
	case LInteger:
		n := int64(v)
		if n == math.MinInt64 {
			b.WriteString("0x" + strconv.FormatUint(uint64(n), 16))
		} else {
			b.WriteString(strconv.FormatInt(n, 10))
		}
	case LNumber:
		b.WriteString(frmtQuoteFloat(float64(v)))
	case LBool, *LNilType:
		b.WriteString(frmtToLString(L, v))
	default:
		L.argError(arg, "value has no literal form")
	}
}

// strFormat ports str_format (lstrlib.c).
func strFormat(L *LState) int {
	top := L.GetTop()
	arg := 1
	frmt := L.CheckString(1)
	var b strings.Builder
	i := 0
	for i < len(frmt) {
		c := frmt[i]
		i++
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		if i < len(frmt) && frmt[i] == '%' { // '%%'
			b.WriteByte('%')
			i++
			continue
		}
		// format item
		arg++
		if arg > top {
			L.argError(arg, "no value")
		}
		form, verb, next := getFormat(L, frmt, i)
		i = next
		switch verb {
		case 'c':
			checkFormat(L, form, flagSetC, false)
			n := checkInteger(L, arg)
			b.WriteString(frmtFinishWidth("", string([]byte{byte(n)}), parseConvSpec(form), false))
		case 'd', 'i', 'u', 'o', 'x', 'X':
			n := checkInteger(L, arg) // the C converts before validating
			flags := flagSetX
			switch verb {
			case 'd', 'i':
				flags = flagSetI
			case 'u':
				flags = flagSetU
			}
			checkFormat(L, form, flags, true)
			b.WriteString(frmtFormatIntConv(parseConvSpec(form), verb, n))
		case 'a', 'A':
			checkFormat(L, form, flagSetF, true)
			n := float64(L.CheckNumber(arg))
			b.WriteString(frmtFormatHexFloat(parseConvSpec(form), verb, n))
		case 'f', 'e', 'E', 'g', 'G':
			n := float64(L.CheckNumber(arg)) // the C converts before validating
			checkFormat(L, form, flagSetF, true)
			b.WriteString(frmtFormatFloatConv(parseConvSpec(form), verb, n))
		case 'p':
			v := L.Get(arg)
			checkFormat(L, form, flagSetC, false)
			sp := parseConvSpec(form)
			s := "(null)" // lua_topointer is NULL for the non-references
			switch v.(type) {
			case LString, *LTable, *LFunction, *LUserData, *LState, LChannel:
				s = frmtPointerString(v)
			}
			b.WriteString(frmtFinishWidth("", s, sp, false))
		case 'q':
			if len(form) > 2 { // modifiers?
				L.RaiseError("specifier '%s' cannot have modifiers", "%q")
			}
			frmtAddLiteral(L, &b, arg)
		case 's':
			s := frmtToLString(L, L.Get(arg))
			if form == "%s" { // no modifiers: keep the whole string
				b.WriteString(s)
			} else {
				if strings.IndexByte(s, 0) >= 0 {
					L.argError(arg, "string contains zeros")
				}
				checkFormat(L, form, flagSetC, true)
				sp := parseConvSpec(form)
				if !sp.hasDot && len(s) >= 100 {
					b.WriteString(s) // too long for width to matter
				} else {
					if sp.hasDot && len(s) > sp.prec {
						s = s[:sp.prec]
					}
					b.WriteString(frmtFinishWidth("", s, sp, false))
				}
			}
		default: // unknown conversions, including the trailing lone '%'
			L.RaiseError("invalid conversion '%s' to 'format'", form)
		}
	}
	L.Push(LString(b.String()))
	return 1
}

//
