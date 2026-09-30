package lua

import (
	"bytes"
	"encoding/binary"
	"math"
)

/* string.pack / string.unpack / string.packsize — the stock 5.5.1 port (U3g).
 *
 * Ported from runtime/lua55/src/lstrlib.c (the PACK/UNPACK section:
 * getnum/getnumlimit/getoption/getdetails/packint/copywithendian/
 * str_pack/str_packsize/unpackint/str_unpack) against the clua55 oracle.
 *
 * The size profile is the ORACLE BLOB's wasm32 C type model, not native Go
 * sizes (docs/U3-implementation-plan.md §1): j/J = 8 (lua_Integer), n = 8
 * (double lua_Number), T = 4 (size_t) — so 's' with no size packs a 4-byte
 * length prefix, l/L = 4 (long), i/I default 4 (int), h/H = 2, b/B = 1,
 * f = 4, d = 8; native endianness little; '!' default maxalign = 16
 * (offsetof(struct cD, u) for the LUAI_MAXALIGN union — see
 * packDefaultMaxAlign); MAXINTSIZE = 16; LUAL_PACKPADBYTE = 0x00.
 *
 * Error dialect: getnumlimit/getoption/unpackint raise plain errors
 * (luaL_error — position-prefixed by RaiseError), while getdetails and the
 * three entry points use luaL_argerror (L.ArgError) with the argument
 * numbers stock uses. */

// value used for padding (LUAL_PACKPADBYTE)
const packPadByte = 0x00

// maximum size for the binary representation of an integer (MAXINTSIZE)
const packMaxIntSize = 16

// sizeof(lua_Integer) in the wasm32 oracle model (SZINT)
const packSzInt = 8

// MAX_SIZE on wasm32: sizeof(size_t) == 4.
const packMaxSize = 0xFFFFFFFF

// nativeendian.little for the oracle model: wasm32 is little-endian.
const packNativeLittle = true

// the '!' default: offsetof(struct cD, u) over LUAI_MAXALIGN — 16 on the
// oracle: wasi clang takes the C99 branch (long double u; ...) and its
// wasm32 long double is binary128 with 16-byte alignment (probed:
// packsize("!xXi16") == 16; a default above MAXINTSIZE would error in
// getnumlimit, and bare '!' works — so it is exactly 16).
const packDefaultMaxAlign = 16

// packOption mirrors the C KOption enum.
type packOption int

const (
	pkInt       packOption = iota // signed integers
	pkUint                        // unsigned integers
	pkFloat                       // single-precision floating-point numbers
	pkNumber                      // Lua "native" floating-point numbers
	pkDouble                      // double-precision floating-point numbers
	pkChar                        // fixed-length strings
	pkString                      // strings with prefixed length
	pkZstr                        // zero-terminated strings
	pkPadding                     // padding
	pkPaddalign                   // padding for alignment
	pkNop                         // no-op (configuration or spaces)
)

// packHeader is the C Header (the L field is passed around instead).
type packHeader struct {
	islittle bool
	maxalign uint64
}

// packFmt is the moving cursor over the format string (the C
// "const char **fmt"); peek() at the end returns 0 like *fmt == '\0'.
type packFmt struct {
	s string
	i int
}

func (f *packFmt) eof() bool  { return f.i >= len(f.s) }
func (f *packFmt) next() byte { c := f.s[f.i]; f.i++; return c } // caller checks !eof
func (f *packFmt) peek() byte {
	if f.i < len(f.s) {
		return f.s[f.i]
	}
	return 0
}

// pkDigit ports digit().
func pkDigit(c byte) bool { return '0' <= c && c <= '9' }

// pkGetnum ports getnum: read an integer numeral from 'fmt' or return 'df'
// if there is no numeral. Like the C, accumulation stops early (leaving
// digits unconsumed) once 'a' can no longer grow within MAX_SIZE.
func pkGetnum(f *packFmt, df uint64) uint64 {
	if !pkDigit(f.peek()) {
		return df
	}
	a := uint64(0)
	for {
		a = a*10 + uint64(f.next()-'0')
		if !(pkDigit(f.peek()) && a <= (packMaxSize-9)/10) {
			break
		}
	}
	return a
}

// pkGetnumlimit ports getnumlimit: read a numeral and raise an error if it
// is larger than the maximum size of integers. The %d of the C message
// prints the size_t through a 32-bit int on wasm32, so values past INT_MAX
// render negative — int32 reproduces that exactly.
func pkGetnumlimit(L *LState, f *packFmt, df uint64) uint64 {
	sz := pkGetnum(f, df)
	if sz-1 >= packMaxIntSize { // unsigned compare: sz == 0 or sz > 16
		L.RaiseError("integral size (%d) out of limits [1,%d]", int32(sz), packMaxIntSize)
	}
	return sz
}

// pkGetoption ports getoption: read and classify the next option, filling
// its size (the fixed sizes are the wasm32 oracle model — see the file
// comment).
func pkGetoption(L *LState, h *packHeader, f *packFmt) (packOption, uint64) {
	opt := f.next()
	switch opt {
	case 'b':
		return pkInt, 1 // sizeof(char)
	case 'B':
		return pkUint, 1
	case 'h':
		return pkInt, 2 // sizeof(short)
	case 'H':
		return pkUint, 2
	case 'l':
		return pkInt, 4 // sizeof(long) on wasm32
	case 'L':
		return pkUint, 4
	case 'j':
		return pkInt, packSzInt // sizeof(lua_Integer)
	case 'J':
		return pkUint, packSzInt
	case 'T':
		return pkUint, 4 // sizeof(size_t) on wasm32
	case 'f':
		return pkFloat, 4 // sizeof(float)
	case 'n':
		return pkNumber, 8 // sizeof(lua_Number)
	case 'd':
		return pkDouble, 8 // sizeof(double)
	case 'i':
		return pkInt, pkGetnumlimit(L, f, 4) // sizeof(int)
	case 'I':
		return pkUint, pkGetnumlimit(L, f, 4)
	case 's':
		return pkString, pkGetnumlimit(L, f, 4) // sizeof(size_t)
	case 'c':
		size := pkGetnum(f, 0xFFFFFFFF) // cast_sizet(-1) on wasm32
		if size == 0xFFFFFFFF {
			L.RaiseError("missing size for format option 'c'")
		}
		return pkChar, size
	case 'z':
		return pkZstr, 0
	case 'x':
		return pkPadding, 1
	case 'X':
		return pkPaddalign, 0
	case ' ':
	case '<':
		h.islittle = true
	case '>':
		h.islittle = false
	case '=':
		h.islittle = packNativeLittle
	case '!':
		h.maxalign = pkGetnumlimit(L, f, packDefaultMaxAlign)
	default:
		L.RaiseError("invalid format option '%c'", opt)
	}
	return pkNop, 0
}

// pkGetdetails ports getdetails: read, classify, and fill other details
// about the next option — 'size' and 'ntoalign' (the bytes to skip for
// alignment). Kpaddalign takes its alignment from the FOLLOWING option
// (consuming it); Kchar never aligns; every other alignment is capped by
// the header's maxalign and must be a power of 2. 'totalsize' is the
// running total for pack/packsize and the read position for unpack, like
// the C.
func pkGetdetails(L *LState, h *packHeader, totalsize uint64, f *packFmt) (opt packOption, size uint64, ntoalign uint64) {
	opt, size = pkGetoption(L, h, f)
	align := size // usually, alignment follows size
	if opt == pkPaddalign {
		if f.eof() {
			L.ArgError(1, "invalid next option for option 'X'")
		} else if nopt, nsize := pkGetoption(L, h, f); nopt == pkChar || nsize == 0 {
			L.ArgError(1, "invalid next option for option 'X'")
		} else {
			align = nsize
		}
	}
	if align <= 1 || opt == pkChar { // need no alignment?
		return opt, size, 0
	}
	if align > h.maxalign { // enforce maximum alignment
		align = h.maxalign
	}
	if align&(align-1) != 0 { // not a power of 2
		L.ArgError(1, "format asks for alignment not power of 2")
	}
	szmoda := totalsize & (align - 1)
	return opt, size, (align - szmoda) & (align - 1)
}

// pkPackint ports packint: pack integer 'n' with 'size' bytes and the
// given endianness. The final loop handles sizes larger than a Lua
// integer, correcting the extra sign-extension bytes of negative values
// (which would otherwise be zeros).
func pkPackint(b *[]byte, n uint64, islittle bool, size int, neg bool) {
	buf := make([]byte, size)
	idx := func(i int) int { // byte i counted from the little end
		if islittle {
			return i
		}
		return size - 1 - i
	}
	buf[idx(0)] = byte(n)
	for i := 1; i < size; i++ {
		n >>= 8
		buf[idx(i)] = byte(n)
	}
	if neg && size > packSzInt { // negative number needs sign extension?
		for i := packSzInt; i < size; i++ {
			buf[idx(i)] = 0xFF
		}
	}
	*b = append(*b, buf...)
}

// pkUnpackint ports unpackint: unpack an integer with 'size' bytes at
// 'pos' with the given endianness. Sizes below SZINT sign-extend from the
// top bit when signed; sizes above SZINT check that the unread bytes match
// the sign (all-zero or all-0xFF) so the value fits a Lua integer.
func pkUnpackint(L *LState, data []byte, pos uint64, islittle bool, size int, issigned bool) int64 {
	var res uint64
	limit := size
	if limit > packSzInt {
		limit = packSzInt
	}
	idx := func(i int) int { // byte i counted from the little end
		if islittle {
			return int(pos) + i
		}
		return int(pos) + size - 1 - i
	}
	for i := limit - 1; i >= 0; i-- {
		res <<= 8
		res |= uint64(data[idx(i)])
	}
	if size < packSzInt { // real size smaller than lua_Integer?
		if issigned { // needs sign extension?
			mask := uint64(1) << uint(size*8-1)
			res = (res ^ mask) - mask
		}
	} else if size > packSzInt { // must check unread bytes
		var mask byte
		if !issigned || int64(res) >= 0 {
			mask = 0
		} else {
			mask = 0xFF
		}
		for i := limit; i < size; i++ {
			if data[idx(i)] != mask {
				L.RaiseError("%d-byte integer does not fit into Lua Integer", size)
			}
		}
	}
	return int64(res)
}

// pkPutFloat is the pack side of copywithendian: the value's bytes in the
// requested endianness (the native order is little).
func pkPutFloat(b *[]byte, bits uint64, size int, islittle bool) {
	var enc [8]byte
	if islittle {
		if size == 4 {
			binary.LittleEndian.PutUint32(enc[:4], uint32(bits))
		} else {
			binary.LittleEndian.PutUint64(enc[:8], bits)
		}
	} else {
		if size == 4 {
			binary.BigEndian.PutUint32(enc[:4], uint32(bits))
		} else {
			binary.BigEndian.PutUint64(enc[:8], bits)
		}
	}
	*b = append(*b, enc[:size]...)
}

// pkGetFloat is the unpack side of copywithendian.
func pkGetFloat(data []byte, pos uint64, size int, islittle bool) uint64 {
	if islittle {
		if size == 4 {
			return uint64(binary.LittleEndian.Uint32(data[pos:]))
		}
		return binary.LittleEndian.Uint64(data[pos:])
	}
	if size == 4 {
		return uint64(binary.BigEndian.Uint32(data[pos:]))
	}
	return binary.BigEndian.Uint64(data[pos:])
}

// strPack ports str_pack.
func strPack(L *LState) int {
	f := &packFmt{s: L.CheckString(1)} // format string
	// stock marks the argument/string-buffer boundary with a pushed nil
	// (the buffer lives on the Lua stack there). Go needs no such buffer,
	// but the marker is observable: a MISSING first value argument reads
	// as an actual nil (luaL_checkinteger reports "got nil", not
	// "got no value") — so the marker stays.
	L.Push(LNil)
	arg := 1             // current argument to pack
	var totalsize uint64 // accumulate total size of result
	h := packHeader{islittle: packNativeLittle, maxalign: 1}
	var b []byte
	for !f.eof() {
		opt, size, ntoalign := pkGetdetails(L, &h, totalsize, f)
		if size+ntoalign > packMaxSize-totalsize {
			L.ArgError(arg, "result too long")
		}
		totalsize += ntoalign + size
		for ; ntoalign > 0; ntoalign-- {
			b = append(b, packPadByte) // fill alignment
		}
		arg++
		switch opt {
		case pkInt: // signed integers
			n := checkInteger(L, arg)
			if size < packSzInt { // need overflow check?
				lim := int64(1) << uint(size*8-1)
				if n < -lim || n >= lim {
					L.ArgError(arg, "integer overflow")
				}
			}
			pkPackint(&b, uint64(n), h.islittle, int(size), n < 0)
		case pkUint: // unsigned integers
			n := checkInteger(L, arg)
			if size < packSzInt && uint64(n) >= uint64(1)<<uint(size*8) {
				L.ArgError(arg, "unsigned overflow")
			}
			pkPackint(&b, uint64(n), h.islittle, int(size), false)
		case pkFloat: // C float
			fl := float32(float64(L.CheckNumber(arg)))
			pkPutFloat(&b, uint64(math.Float32bits(fl)), 4, h.islittle)
		case pkNumber: // Lua float
			fl := float64(L.CheckNumber(arg))
			pkPutFloat(&b, math.Float64bits(fl), 8, h.islittle)
		case pkDouble: // C double
			fl := float64(L.CheckNumber(arg))
			pkPutFloat(&b, math.Float64bits(fl), 8, h.islittle)
		case pkChar: // fixed-size string
			s := L.CheckString(arg)
			if uint64(len(s)) > size {
				L.ArgError(arg, "string longer than given size")
			}
			b = append(b, s...)
			for i := uint64(len(s)); i < size; i++ { // zero padding
				b = append(b, packPadByte)
			}
		case pkString: // strings with length count
			s := L.CheckString(arg)
			if !(size >= packSzInt || uint64(len(s)) < uint64(1)<<uint(size*8)) {
				L.ArgError(arg, "string length does not fit in given size")
			}
			pkPackint(&b, uint64(len(s)), h.islittle, int(size), false) // pack length
			b = append(b, s...)
			totalsize += uint64(len(s))
		case pkZstr: // zero-terminated string
			s := L.CheckString(arg)
			if bytes.IndexByte([]byte(s), 0) >= 0 { // strlen(s) == len?
				L.ArgError(arg, "string contains zeros")
			}
			b = append(b, s...)
			b = append(b, 0) // add zero at the end
			totalsize += uint64(len(s)) + 1
		case pkPadding:
			b = append(b, packPadByte)
			fallthrough
		case pkPaddalign, pkNop:
			arg-- // undo increment
		}
	}
	L.Push(LString(b))
	return 1
}

// strPacksize ports str_packsize.
func strPacksize(L *LState) int {
	f := &packFmt{s: L.CheckString(1)} // format string
	var totalsize uint64               // accumulate total size of result
	h := packHeader{islittle: packNativeLittle, maxalign: 1}
	for !f.eof() {
		opt, size, ntoalign := pkGetdetails(L, &h, totalsize, f)
		if opt == pkString || opt == pkZstr {
			L.ArgError(1, "variable-length format")
		}
		size += ntoalign // total space used by option
		if totalsize > packMaxSize-size {
			L.ArgError(1, "format result too large")
		}
		totalsize += size
	}
	L.Push(LInteger(int64(totalsize)))
	return 1
}

// pkPosrelatI ports posrelatI: convert a relative position (negative means
// back from end) into an absolute one in [1, inf); the inverted comparison
// avoids the overflow computing -pos.
func pkPosrelatI(pos int64, length uint64) uint64 {
	if pos > 0 {
		return uint64(pos)
	}
	if pos == 0 {
		return 1
	}
	if pos < -int64(length) { // inverted comparison
		return 1 // clip to 1
	}
	return length + uint64(pos) + 1
}

// strUnpack ports str_unpack; the final return is the next read position.
func strUnpack(L *LState) int {
	f := &packFmt{s: L.CheckString(1)}
	data := []byte(L.CheckString(2))
	ld := uint64(len(data))
	pos := pkPosrelatI(optInteger(L, 3, 1), ld) - 1
	if pos > ld {
		L.ArgError(3, "initial position out of string")
	}
	h := packHeader{islittle: packNativeLittle, maxalign: 1}
	n := 0 // number of results
	for !f.eof() {
		opt, size, ntoalign := pkGetdetails(L, &h, pos, f)
		if ntoalign+size > ld-pos {
			L.ArgError(2, "data string too short")
		}
		pos += ntoalign // skip alignment
		n++
		switch opt {
		case pkInt, pkUint:
			L.Push(LInteger(pkUnpackint(L, data, pos, h.islittle, int(size), opt == pkInt)))
		case pkFloat:
			L.Push(LNumber(float64(math.Float32frombits(uint32(pkGetFloat(data, pos, 4, h.islittle))))))
		case pkNumber, pkDouble:
			L.Push(LNumber(math.Float64frombits(pkGetFloat(data, pos, 8, h.islittle))))
		case pkChar:
			L.Push(LString(string(data[pos : pos+size])))
		case pkString:
			slen := uint64(pkUnpackint(L, data, pos, h.islittle, int(size), false))
			if slen > ld-pos-size {
				L.ArgError(2, "data string too short")
			}
			L.Push(LString(string(data[pos+size : pos+size+slen])))
			pos += slen // skip string
		case pkZstr:
			zlen := ld - pos // strlen: up to the first zero or the end
			if i := bytes.IndexByte(data[pos:], 0); i >= 0 {
				zlen = uint64(i)
			}
			if pos+zlen >= ld {
				L.ArgError(2, "unfinished string for format 'z'")
			}
			L.Push(LString(string(data[pos : pos+zlen])))
			pos += zlen + 1 // skip string plus final '\0'
		case pkPaddalign, pkPadding, pkNop:
			n-- // undo increment
		}
		pos += size
	}
	L.Push(LInteger(int64(pos) + 1)) // next position
	return n + 1
}
