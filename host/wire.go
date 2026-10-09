package host

// The value wire protocol shared with runtime/luawasm55.c (the PT_* tags):
// tag-first, self-describing, little-endian.
//
//	args   = u32 count, count × value   (guest→host, hostfn arguments)
//	result = u32 count, count × value   (host→guest, hostfn returns)
//	error  = 1 value                    (host→guest, hostfn failure)
//
//	value  = 0 nil | 1 false | 2 true | 3 f64 | 4 u32-len bytes
//	       | 5 u32-n (k v)*            (table; args side may carry a
//	                                     PT_MORE byte after n when the
//	                                     guest truncated the expansion)
//	       | 6 function | 10 cycle | 11 depth-truncated   (args side only)
//	       | 13 i64                     (integer — U6e: LUA_VNUMINT crosses
//	                                     lossless; results keep Lua
//	                                     semantics, integers print bare)
//
// The host never sends 6/10/11 back (the guest decoder rejects them);
// host-to-guest tables are always complete.

import (
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
)

// ValueKind mirrors the wire tags (PT_* in runtime/luawasm55.c).
type ValueKind uint8

const (
	KindNil      ValueKind = 0
	KindFalse    ValueKind = 1
	KindTrue     ValueKind = 2
	KindNumber   ValueKind = 3
	KindString   ValueKind = 4
	KindTable    ValueKind = 5
	KindFunction ValueKind = 6
	KindCycle    ValueKind = 10
	KindDeep     ValueKind = 11
	KindInteger  ValueKind = 13
)

const wireMore byte = 12 // PT_MORE: an args-side table was truncated

// KV is one table entry.
type KV struct {
	Key, Val Value
}

// Value is one Lua value as it crosses the seam.
type Value struct {
	Kind  ValueKind
	Num   float64 // KindNumber
	Int   int64   // KindInteger (U6e: lossless, not narrowed through Num)
	Str   string  // KindString
	Pairs []KV    // KindTable
	More  bool    // KindTable: the guest truncated the expansion
}

// Convenience constructors.
func Nil() Value { return Value{Kind: KindNil} }
func Bool(b bool) Value {
	if b {
		return Value{Kind: KindTrue}
	}
	return Value{Kind: KindFalse}
}
func Number(f float64) Value { return Value{Kind: KindNumber, Num: f} }
func Int(n int64) Value      { return Value{Kind: KindInteger, Int: n} }
func String(s string) Value  { return Value{Kind: KindString, Str: s} }
func Table(pairs ...KV) Value {
	return Value{Kind: KindTable, Pairs: pairs}
}

// BoolVal reports the boolean of a KindTrue/KindFalse value.
func (v Value) BoolVal() bool { return v.Kind == KindTrue }

// String renders a value for error messages and logs (NOT the Redis reply
// conversion — that is the daemon's job, host-side and locale-free).
func (v Value) String() string {
	switch v.Kind {
	case KindNil:
		return "nil"
	case KindFalse:
		return "false"
	case KindTrue:
		return "true"
	case KindNumber:
		return numRepr(v.Num)
	case KindInteger:
		return strconv.FormatInt(v.Int, 10)
	case KindString:
		return v.Str
	case KindTable:
		return "table"
	case KindFunction:
		return "function"
	case KindCycle:
		return "table (cycle)"
	case KindDeep:
		return "table (too deep)"
	}
	return "?"
}

// numRepr formats a FLOAT the way stock Lua 5.5 renders floats in
// tostring (lobject.c tostringbuffFloat): %.15g, re-read, %.17g when the
// round-trip loses the value, ".0" appended when the result looks like a
// plain integer (2.0 stays "2.0"), inf/nan in the libc spellings.
// Integers (KindInteger) never come through here — they print bare via
// FormatInt (U6e; ported from formatlib.go's numberToStringStock so the
// package carries its own copy).
func numRepr(f float64) string {
	switch {
	case math.IsNaN(f):
		if math.Signbit(f) {
			return "-nan"
		}
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	}
	s := strconv.FormatFloat(f, 'g', 15, 64)
	if v, err := strconv.ParseFloat(s, 64); err != nil || v != f {
		s = strconv.FormatFloat(f, 'g', 17, 64)
	}
	if strings.Trim(s, "-0123456789") == "" {
		s += ".0"
	}
	return s
}

// decodeValue reads one wire value at buf[off]. depth guards malformed
// nesting (a host bug — surfaced as an error, never a panic).
func decodeValue(buf []byte, off, depth int) (Value, int, error) {
	if depth > 128 {
		return Value{}, off, errors.New("host: wire value nesting too deep")
	}
	if off < 0 || off >= len(buf) {
		return Value{}, off, errors.New("host: truncated wire value")
	}
	switch tag := buf[off]; ValueKind(tag) {
	case KindNil:
		return Nil(), off + 1, nil
	case KindFalse:
		return Bool(false), off + 1, nil
	case KindTrue:
		return Bool(true), off + 1, nil
	case KindNumber:
		if off+9 > len(buf) {
			return Value{}, off, errors.New("host: truncated wire number")
		}
		return Number(math.Float64frombits(binary.LittleEndian.Uint64(buf[off+1:]))), off + 9, nil
	case KindInteger:
		if off+9 > len(buf) {
			return Value{}, off, errors.New("host: truncated wire integer")
		}
		return Int(int64(binary.LittleEndian.Uint64(buf[off+1:]))), off + 9, nil
	case KindString:
		if off+5 > len(buf) {
			return Value{}, off, errors.New("host: truncated wire string header")
		}
		n := int(binary.LittleEndian.Uint32(buf[off+1:]))
		if off+5+n > len(buf) {
			return Value{}, off, errors.New("host: truncated wire string")
		}
		return String(string(buf[off+5 : off+5+n])), off + 5 + n, nil
	case KindTable:
		off++
		if off+4 > len(buf) {
			return Value{}, off, errors.New("host: truncated wire table header")
		}
		n := int(binary.LittleEndian.Uint32(buf[off:]))
		off += 4
		v := Value{Kind: KindTable}
		// args-side truncation sentinel: the guest capped the expansion
		if off < len(buf) && buf[off] == wireMore {
			v.More = true
			off++
		}
		if n > 1<<20 {
			return Value{}, off, errors.New("host: wire table too large")
		}
		v.Pairs = make([]KV, 0, min(n, 64))
		for i := 0; i < n; i++ {
			var k, val Value
			var err error
			if k, off, err = decodeValue(buf, off, depth+1); err != nil {
				return Value{}, off, err
			}
			if val, off, err = decodeValue(buf, off, depth+1); err != nil {
				return Value{}, off, err
			}
			v.Pairs = append(v.Pairs, KV{Key: k, Val: val})
		}
		return v, off, nil
	case KindFunction, KindCycle, KindDeep:
		return Value{Kind: ValueKind(tag)}, off + 1, nil
	default:
		return Value{}, off, fmt.Errorf("host: unknown wire tag %d", tag)
	}
}

// decodeArgs decodes a hostfn argument frame (u32 count + values).
func decodeArgs(buf []byte) ([]Value, error) {
	if len(buf) < 4 {
		return nil, errors.New("host: truncated argument frame")
	}
	n := int(binary.LittleEndian.Uint32(buf))
	off := 4
	if n > 4096 {
		return nil, fmt.Errorf("host: too many hostfn arguments (%d)", n)
	}
	vals := make([]Value, 0, min(n, 16))
	for i := 0; i < n; i++ {
		v, next, err := decodeValue(buf, off, 0)
		if err != nil {
			return nil, err
		}
		vals = append(vals, v)
		off = next
	}
	return vals, nil
}

// encodeValue appends one value (host→guest direction: only the kinds the
// guest decoder accepts — never function/cycle/deep).
func encodeValue(out []byte, v Value) ([]byte, error) {
	switch v.Kind {
	case KindNil:
		return append(out, byte(KindNil)), nil
	case KindFalse, KindTrue:
		return append(out, byte(v.Kind)), nil
	case KindNumber:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], math.Float64bits(v.Num))
		out = append(out, byte(KindNumber))
		return append(out, b[:]...), nil
	case KindInteger:
		var b [8]byte
		binary.LittleEndian.PutUint64(b[:], uint64(v.Int))
		out = append(out, byte(KindInteger))
		return append(out, b[:]...), nil
	case KindString:
		if len(v.Str) > 64<<20 {
			return nil, errors.New("host: hostfn string value too large")
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(v.Str)))
		out = append(out, byte(KindString))
		out = append(out, b[:]...)
		return append(out, v.Str...), nil
	case KindTable:
		if len(v.Pairs) > 512 {
			return nil, errors.New("host: hostfn table has too many entries (>512)")
		}
		var b [4]byte
		binary.LittleEndian.PutUint32(b[:], uint32(len(v.Pairs)))
		out = append(out, byte(KindTable))
		out = append(out, b[:]...)
		var err error
		for _, kv := range v.Pairs {
			if out, err = encodeValue(out, kv.Key); err != nil {
				return nil, err
			}
			if out, err = encodeValue(out, kv.Val); err != nil {
				return nil, err
			}
		}
		return out, nil
	default:
		return nil, fmt.Errorf("host: cannot send a %s value to the guest", v)
	}
}

// encodeResults encodes a hostfn return frame (u32 count + values).
func encodeResults(vals []Value) ([]byte, error) {
	if len(vals) > 4096 {
		return nil, fmt.Errorf("host: too many hostfn results (%d)", len(vals))
	}
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(len(vals)))
	out := append([]byte(nil), b[:]...)
	var err error
	for _, v := range vals {
		if out, err = encodeValue(out, v); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// encodeError encodes the hostfn error frame (one value).
func encodeError(msg string) []byte {
	out, err := encodeValue(nil, String(msg))
	if err != nil { // unreachable: msg is a plain string
		return []byte{byte(KindNil)}
	}
	return out
}
