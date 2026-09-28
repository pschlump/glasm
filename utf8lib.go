package lua

/* utf8 library — the stock 5.5.1 surface (U3e), ported from
 * runtime/lua55/src/lutf8lib.c. The decoder accepts structurally valid
 * sequences of up to 6 bytes (lax ceiling 0x7FFFFFFF), rejects overlong
 * forms via limits[], and — unless a lax argument is given — rejects
 * surrogates and values above MAXUNICODE. utf8.offset has 5.5's
 * two-value return (start byte and last continuation byte).
 *
 * Where the C reads the NUL terminator just past the string, the Go
 * bounds-checks and reports "not a continuation byte" (iscont is false
 * at/past the end) — same observable behavior. */

const (
	utf8MaxUnicode = 0x10FFFF
	utf8MaxUTF     = 0x7FFFFFFF
	utf8MsgInvalid = "invalid UTF-8 code"
)

// utf8IsCont reports a continuation byte (0b10xxxxxx); out of range is
// never a continuation byte (the C reads the NUL terminator there).
func utf8IsCont(s string, i int) bool {
	return i >= 0 && i < len(s) && s[i]&0xC0 == 0x80
}

// utf8Posrelat ports u_posrelat: negative positions count back from the
// end (len+pos+1); a too-negative position clamps to 0.
func utf8Posrelat(pos int64, len int64) int64 {
	if pos >= 0 {
		return pos
	}
	if uint64(-pos) > uint64(len) {
		return 0
	}
	return len + pos + 1
}

// utf8Decode ports utf8_decode: decodes one sequence at s[i:], returning
// the value and the index just past the sequence, or ok=false.
func utf8Decode(s string, i int, strict bool) (uint32, int, bool) {
	var limits = [6]uint32{^uint32(0), 0x80, 0x800, 0x10000, 0x200000, 0x4000000}
	c := uint32(s[i])
	res := uint32(0)
	if c < 0x80 {
		res = c
	} else if c >= 0xfe {
		return 0, 0, false // needs six or more continuation bytes
	} else {
		count := 0
		for ; c&0x40 != 0; c <<= 1 {
			count++
			if count >= len(s)-i || !utf8IsCont(s, i+count) {
				return 0, 0, false
			}
			cc := uint32(s[i+count])
			res = res<<6 | cc&0x3F
		}
		res |= (c & 0x7F) << uint(count*5)
		if res > utf8MaxUTF || res < limits[count] {
			return 0, 0, false // overlong or out of range
		}
		i += count
	}
	if strict && (res > utf8MaxUnicode || (res >= 0xD800 && res <= 0xDFFF)) {
		return 0, 0, false
	}
	return res, i + 1, true
}

// utf8Escape ports luaO_utf8esc: encode a code point (≤ MAXUTF) as UTF-8
// bytes. utf8.char allows surrogates (only the MAXUTF range check).
func utf8Escape(code uint32) []byte {
	if code < 0x80 {
		return []byte{byte(code)}
	}
	var buff [8]byte
	n := 1
	mfb := uint32(0x3f)
	for {
		buff[len(buff)-n] = byte(0x80 | code&0x3f)
		n++
		code >>= 6
		mfb >>= 1
		if code <= mfb {
			break
		}
	}
	buff[len(buff)-n] = byte((^mfb << 1) | code)
	return buff[len(buff)-n:]
}

func OpenUTF8(L *LState) int {
	mod := L.RegisterModule(UTF8LibName, utf8Funcs).(*LTable)
	mod.RawSetString("charpattern", LString("[\x00-\x7F\xC2-\xFD][\x80-\xBF]*"))
	L.Push(mod)
	return 1
}

var utf8Funcs = map[string]LGFunction{
	"offset":    utf8Offset,
	"codepoint": utf8Codepoint,
	"char":      utf8Char,
	"len":       utf8Len,
	"codes":     utf8Codes,
}

// utf8Len ports utflen: the number of characters starting in [i,j], or
// nil + the offending byte position when the interval is malformed.
func utf8Len(L *LState) int {
	s := L.CheckString(1)
	length := int64(len(s))
	posi := utf8Posrelat(optInteger(L, 2, 1), length)
	posj := utf8Posrelat(optInteger(L, 3, -1), length)
	lax := L.ToBool(4)
	if !(posi >= 1 && posi-1 <= length) {
		L.ArgError(2, "initial position out of bounds")
	}
	posi--
	if !(posj-1 < length) {
		L.ArgError(3, "final position out of bounds")
	}
	posj--
	n := int64(0)
	for posi <= posj {
		_, next, ok := utf8Decode(s, int(posi), !lax)
		if !ok {
			L.Push(LNil)
			L.Push(LInteger(posi + 1))
			return 2
		}
		posi = int64(next)
		n++
	}
	L.Push(LInteger(n))
	return 1
}

// utf8Codepoint ports codepoint: code points for all characters that
// start in [i,j] (j defaults to i).
func utf8Codepoint(L *LState) int {
	s := L.CheckString(1)
	length := int64(len(s))
	posi := utf8Posrelat(optInteger(L, 2, 1), length)
	pose := utf8Posrelat(optInteger(L, 3, posi), length)
	lax := L.ToBool(4)
	if posi < 1 {
		L.ArgError(2, "out of bounds")
	}
	if pose > length {
		L.ArgError(3, "out of bounds")
	}
	if posi > pose {
		return 0 // empty interval
	}
	if pose-posi >= 1<<31-1 {
		L.RaiseError("string slice too long")
	}
	i := int(posi - 1)
	end := int(pose)
	n := 0
	for i < end {
		code, next, ok := utf8Decode(s, i, !lax)
		if !ok {
			L.RaiseError("%s", utf8MsgInvalid)
		}
		L.Push(LInteger(int64(code)))
		n++
		i = next
	}
	return n
}

func utf8Char(L *LState) int {
	top := L.GetTop()
	buf := make([]byte, 0, top)
	for i := 1; i <= top; i++ {
		code := uint64(checkInteger(L, i))
		if code > utf8MaxUTF {
			L.ArgError(i, "value out of range")
		}
		buf = append(buf, utf8Escape(uint32(code))...)
	}
	L.Push(LString(buf))
	return 1
}

// utf8Offset ports byteoffset: the byte positions where the n-th
// character counting from i starts and (5.5) ends; n == 0 means the
// character containing i; nil when the character cannot be found.
func utf8Offset(L *LState) int {
	s := L.CheckString(1)
	length := int64(len(s))
	n := checkInteger(L, 2)
	defposi := int64(1)
	if n < 0 {
		defposi = length + 1
	}
	posi := utf8Posrelat(optInteger(L, 3, defposi), length)
	if !(posi >= 1 && posi-1 <= length) {
		L.ArgError(3, "position out of bounds")
	}
	posi--
	if n == 0 {
		// find the beginning of the current byte sequence
		for posi > 0 && utf8IsCont(s, int(posi)) {
			posi--
		}
	} else {
		if utf8IsCont(s, int(posi)) {
			L.RaiseError("initial position is a continuation byte")
		}
		if n < 0 {
			for n < 0 && posi > 0 {
				posi--
				for posi > 0 && utf8IsCont(s, int(posi)) {
					posi--
				}
				n++
			}
		} else {
			n--
			for n > 0 && posi < length {
				posi++
				for utf8IsCont(s, int(posi)) {
					posi++
				}
				n--
			}
		}
	}
	if n != 0 {
		L.Push(LNil)
		return 1
	}
	L.Push(LInteger(posi + 1))
	// the C reads s[posi] where posi == len reads the NUL sentinel —
	// same observable behavior as a single-byte (non-continuation) 0
	at := byte(0)
	if posi < length {
		at = s[posi]
	}
	if at&0x80 != 0 { // multi-byte character?
		if utf8IsCont(s, int(posi)) {
			L.RaiseError("initial position is a continuation byte")
		}
		for utf8IsCont(s, int(posi)+1) {
			posi++ // skip to the last continuation byte
		}
	}
	L.Push(LInteger(posi + 1))
	return 2
}

func utf8IterAux(L *LState, strict bool) int {
	s := L.CheckString(1)
	length := uint64(len(s))
	n := uint64(L.ToInt64(2))
	if n < length {
		for utf8IsCont(s, int(n)) {
			n++
		}
	}
	if n >= length { // also handles negative n (wraps huge)
		return 0
	}
	code, next, ok := utf8Decode(s, int(n), strict)
	if !ok || utf8IsCont(s, next) {
		L.RaiseError("%s", utf8MsgInvalid)
	}
	L.Push(LInteger(int64(n + 1)))
	L.Push(LInteger(int64(code)))
	return 2
}

func utf8IterStrict(L *LState) int { return utf8IterAux(L, true) }
func utf8IterLax(L *LState) int    { return utf8IterAux(L, false) }

// utf8Codes ports iter_codes: iterator + the string + 0 for the
// generic for; the string must not start mid-sequence.
func utf8Codes(L *LState) int {
	lax := L.ToBool(2)
	s := L.CheckString(1)
	if utf8IsCont(s, 0) {
		L.ArgError(1, utf8MsgInvalid)
	}
	if lax {
		L.Push(L.NewFunction(utf8IterLax))
	} else {
		L.Push(L.NewFunction(utf8IterStrict))
	}
	L.Push(LString(s))
	L.Push(LInteger(0))
	return 3
}

//
