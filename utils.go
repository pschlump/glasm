package lua

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unsafe"
)

func intMin(a, b int) int {
	if a < b {
		return a
	} else {
		return b
	}
}

func intMax(a, b int) int {
	if a > b {
		return a
	} else {
		return b
	}
}

func defaultFormat(v interface{}, f fmt.State, c rune) {
	buf := make([]string, 0, 10)
	buf = append(buf, "%")
	for i := 0; i < 128; i++ {
		if f.Flag(i) {
			buf = append(buf, string(rune(i)))
		}
	}

	if w, ok := f.Width(); ok {
		buf = append(buf, strconv.Itoa(w))
	}
	if p, ok := f.Precision(); ok {
		buf = append(buf, "."+strconv.Itoa(p))
	}
	buf = append(buf, string(c))
	format := strings.Join(buf, "")
	fmt.Fprintf(f, format, v)
}

type flagScanner struct {
	flag       byte
	start      string
	end        string
	buf        []byte
	str        string
	Length     int
	Pos        int
	HasFlag    bool
	ChangeFlag bool
}

func newFlagScanner(flag byte, start, end, str string) *flagScanner {
	return &flagScanner{flag, start, end, make([]byte, 0, len(str)), str, len(str), 0, false, false}
}

func (fs *flagScanner) AppendString(str string) { fs.buf = append(fs.buf, str...) }

func (fs *flagScanner) AppendChar(ch byte) { fs.buf = append(fs.buf, ch) }

func (fs *flagScanner) String() string { return string(fs.buf) }

func (fs *flagScanner) Next() (byte, bool) {
	c := byte('\000')
	fs.ChangeFlag = false
	if fs.Pos == fs.Length {
		if fs.HasFlag {
			fs.AppendString(fs.end)
		}
		return c, true
	} else {
		c = fs.str[fs.Pos]
		if c == fs.flag {
			if fs.Pos < (fs.Length-1) && fs.str[fs.Pos+1] == fs.flag {
				fs.HasFlag = false
				fs.AppendChar(fs.flag)
				fs.Pos += 2
				return fs.Next()
			} else if fs.Pos != fs.Length-1 {
				if fs.HasFlag {
					fs.AppendString(fs.end)
				}
				fs.AppendString(fs.start)
				fs.ChangeFlag = true
				fs.HasFlag = true
			}
		}
	}
	fs.Pos++
	return c, false
}

var cDateFlagToGo = map[byte]string{
	'a': "mon", 'A': "Monday", 'b': "Jan", 'B': "January", 'c': "02 Jan 06 15:04 MST", 'd': "02",
	'F': "2006-01-02", 'H': "15", 'I': "03", 'm': "01", 'M': "04", 'p': "PM", 'P': "pm", 'S': "05",
	'x': "15/04/05", 'X': "15:04:05", 'y': "06", 'Y': "2006", 'z': "-0700", 'Z': "MST"}

func strftime(t time.Time, cfmt string) string {
	sc := newFlagScanner('%', "", "", cfmt)
	for c, eos := sc.Next(); !eos; c, eos = sc.Next() {
		if !sc.ChangeFlag {
			if sc.HasFlag {
				if v, ok := cDateFlagToGo[c]; ok {
					sc.AppendString(t.Format(v))
				} else {
					switch c {
					case 'w':
						sc.AppendString(fmt.Sprint(int(t.Weekday())))
					default:
						sc.AppendChar('%')
						sc.AppendChar(c)
					}
				}
				sc.HasFlag = false
			} else {
				sc.AppendChar(c)
			}
		}
	}

	return sc.String()
}

func isInteger(v LNumber) bool {
	return float64(v) == float64(int64(v))
	//_, frac := math.Modf(float64(v))
	//return frac == 0.0
}

// isNumberValue reports whether v belongs to one of the two "number"
// subtypes (float or integer).
func isNumberValue(v LValue) bool {
	switch v.(type) {
	case LNumber, LInteger:
		return true
	default:
		return false
	}
}

// floatToIntExact converts a float to its exact integer value (stock
// luaV_flttointeger with F2Ieq): fails on NaN/±Inf and on any float with
// a fractional part or outside the int64 range. Out-of-range floats must
// be rejected BEFORE int64(f) — Go leaves that conversion
// implementation-defined, unlike C's truncation contract.
func floatToIntExact(f float64) (int64, bool) {
	if f != f || f >= 9223372036854775808.0 || f < -9223372036854775808.0 {
		return 0, false
	}
	i := int64(f)
	if float64(i) == f {
		return i, true
	}
	return 0, false
}

// intFitsFloat reports whether |i| <= 2^53: the range where float64(i)
// round-trips exactly, so mixed int/float comparisons can compare as
// floats (stock l_intfitsf).
func intFitsFloat(i int64) bool {
	return i <= 9007199254740992 && i >= -9007199254740992
}

// parseNumber: string → number with stock luaO_str2num semantics,
// preserving the syntactic subtype (U2/D1): integer syntax yields LInteger
// — plain hex accumulates in *unsigned* arithmetic and wraps (0xFFFFFFFFFFFFFFFF
// == -1), decimal overflow falls back to float — while every dot- or
// exponent-bearing form yields LNumber. Leading zeros stay DECIMAL ("0777"
// is 777, never octal — the old ParseInt(s, 0) read "0255" as 173; the
// fuzzer caught `-(0 .. 255)` printing -173 on interp vs -255 on wasm).
// Hex floats ("0x1.8p3", and dot-bearing hex without a p-exponent, which
// C's strtod accepts but Go's ParseFloat requires) and trailing-dot
// decimal forms ("3.", "3.e5") normalize to Go syntax first. A leading
// sign is accepted (string→number coercion; numeral literals never carry
// one). Underscores are not digits (Go's base-0 syntax is not Lua's). The
// parse package's numeralOK mirrors this acceptance for lex-time
// validation.
func parseNumber(number string) (LValue, error) {
	number = strings.Trim(number, " \t\n\v\f\r")
	if strings.IndexByte(number, '_') >= 0 {
		return LNumber(0), fmt.Errorf("invalid number: %s", number)
	}
	body := number
	sign := ""
	if len(body) > 0 && (body[0] == '-' || body[0] == '+') {
		sign, body = body[:1], body[1:]
	}
	isHex := len(body) > 1 && body[0] == '0' && (body[1] == 'x' || body[1] == 'X')
	if isHex {
		h := body[2:]
		if strings.IndexAny(h, ".pP") >= 0 {
			if v, err := strconv.ParseFloat(normalizeNumeral(number), LNumberBit); err == nil || errors.Is(err, strconv.ErrRange) {
				return LNumber(v), nil
			}
		} else {
			// plain hex integer: accumulate in unsigned arithmetic so
			// more than 64 bits wraps exactly like stock l_str2int
			// (0xFFFFFFFFFFFFFFFF parses as -1, not as a float)
			u, ok := uint64(0), true
			for i := 0; i < len(h); i++ {
				d := hexDigitValue(h[i])
				if d < 0 {
					ok = false
					break
				}
				u = u<<4 | uint64(d)
			}
			if ok && len(h) > 0 {
				v := int64(u)
				if sign == "-" {
					v = -v
				}
				return LInteger(v), nil
			}
			// fall through: "0x" alone, trailing garbage, ...
		}
	} else {
		if v, err := strconv.ParseInt(number, 10, LNumberBit); err == nil {
			return LInteger(v), nil
		}
		// decimal integer overflow (2^63 and beyond): float, per 5.4+
		if v, err := strconv.ParseFloat(normalizeNumeral(number), LNumberBit); err == nil || errors.Is(err, strconv.ErrRange) {
			return LNumber(v), nil
		}
	}
	return LNumber(0), fmt.Errorf("invalid number: %s", number)
}

func hexDigitValue(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	case 'A' <= c && c <= 'F':
		return int(c-'A') + 10
	}
	return -1
}

// normalizeNumeral adapts Lua numeral syntax to Go's ParseFloat: a digit
// after a trailing '.' ("3." → "3.0", "3.e5" → "3.0e5") and, for
// dot-bearing hex numerals without a p-exponent, an explicit "p0"
// ("0x8.8" → "0x8.8p0" — C strtod accepts the bare form, Go requires the
// exponent). parse/lexer.go carries the same helper for lex-time
// validation.
func normalizeNumeral(s string) string {
	b := []byte(s)
	for i := 0; i < len(b); i++ {
		if b[i] == '.' && (i+1 == len(b) || !isHexNumeralDigit(b[i+1])) {
			b = append(b, 0)
			copy(b[i+2:], b[i+1:])
			b[i+1] = '0'
		}
	}
	t := string(b)
	ht := t
	if len(ht) > 0 && (ht[0] == '-' || ht[0] == '+') {
		ht = ht[1:]
	}
	if len(ht) > 2 && ht[0] == '0' && (ht[1] == 'x' || ht[1] == 'X') && strings.IndexAny(ht, "pP") < 0 {
		t += "p0"
	}
	return t
}

func isHexNumeralDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

func popenArgs(arg string) (string, []string) {
	cmd := "/bin/sh"
	args := []string{"-c"}
	if LuaOS == "windows" {
		cmd = "C:\\Windows\\system32\\cmd.exe"
		args = []string{"/c"}
	}
	args = append(args, arg)
	return cmd, args
}

func isGoroutineSafe(lv LValue) bool {
	switch v := lv.(type) {
	case *LFunction, *LUserData, *LState:
		return false
	case *LTable:
		return v.Metatable == LNil
	default:
		return true
	}
}

func readBufioSize(reader *bufio.Reader, size int64) ([]byte, error, bool) {
	result := []byte{}
	read := int64(0)
	var err error
	var n int
	for read != size {
		buf := make([]byte, size-read)
		n, err = reader.Read(buf)
		if err != nil {
			break
		}
		read += int64(n)
		result = append(result, buf[:n]...)
	}
	e := err
	if e != nil && e == io.EOF {
		e = nil
	}

	return result, e, len(result) == 0 && err == io.EOF
}

func readBufioLine(reader *bufio.Reader) ([]byte, error, bool) {
	result := []byte{}
	var buf []byte
	var err error
	var isprefix bool = true
	for isprefix {
		buf, isprefix, err = reader.ReadLine()
		if err != nil {
			break
		}
		result = append(result, buf...)
	}
	e := err
	if e != nil && e == io.EOF {
		e = nil
	}

	return result, e, len(result) == 0 && err == io.EOF
}

func int2Fb(val int) int {
	e := 0
	x := val
	for x >= 16 {
		x = (x + 1) >> 1
		e++
	}
	if x < 8 {
		return x
	}
	return ((e + 1) << 3) | (x - 8)
}

func strCmp(s1, s2 string) int {
	len1 := len(s1)
	len2 := len(s2)
	for i := 0; ; i++ {
		c1 := -1
		if i < len1 {
			c1 = int(s1[i])
		}
		c2 := -1
		if i != len2 {
			c2 = int(s2[i])
		}
		switch {
		case c1 < c2:
			return -1
		case c1 > c2:
			return +1
		case c1 < 0:
			return 0
		}
	}
}

func unsafeFastStringToReadOnlyBytes(s string) (bs []byte) {
	sh := (*reflect.StringHeader)(unsafe.Pointer(&s))
	bh := (*reflect.SliceHeader)(unsafe.Pointer(&bs))
	bh.Data = sh.Data
	bh.Cap = sh.Len
	bh.Len = sh.Len
	return
}
