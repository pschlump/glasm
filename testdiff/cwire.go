package testdiff

// The C-side value wire: the PT_* tag format the C runtime emits into
// wasm memory (host.* events — PRINT payloads and GLOBALS snapshots) and
// its decoder. Extracted from clua.go at the U8 retirement of the 5.1
// engine: clua55 and the wasm/wazero engines all decode this same wire
// (runtime/luawasm55.c emits it; the 5.1 emitter, runtime/luawasm.c,
// went with its tree).

import (
	"encoding/binary"
	"fmt"
	"math"
	"strconv"
)

// value-protocol tags (must match runtime/luawasm55.c — U6e adds
// PT_INTEGER=13 there)
const (
	ptNil = iota
	ptFalse
	ptTrue
	ptNumber
	ptString
	ptTable
	ptFunction
	ptUserdata
	ptThread
	ptLightUserdata
	ptCycle
	ptDeepTable
	ptMore
	ptInteger
)

type cval struct {
	tag  byte
	num  float64
	inum int64 // ptInteger: LUA_VNUMINT, lossless (U6e)
	str  string
	tbl  []([2]cval)
	more bool
}

type cparser struct {
	b   []byte
	off int
}

func (p *cparser) byte() byte  { v := p.b[p.off]; p.off++; return v }
func (p *cparser) u32() uint32 { v := binary.LittleEndian.Uint32(p.b[p.off:]); p.off += 4; return v }
func (p *cparser) f64() float64 {
	v := math.Float64frombits(binary.LittleEndian.Uint64(p.b[p.off:]))
	p.off += 8
	return v
}
func (p *cparser) i64() int64 {
	v := int64(binary.LittleEndian.Uint64(p.b[p.off:]))
	p.off += 8
	return v
}

// value decodes one value. expand selects whether a PT_TABLE tag carries
// entries (GLOBALS serialization) or is bare (print never expands tables —
// enc_value with expand=0 emits just the tag).
func (p *cparser) value(expand bool) (cval, error) {
	t := p.byte()
	switch t {
	case ptNil, ptFalse, ptTrue, ptTable, ptFunction, ptUserdata, ptThread, ptLightUserdata, ptCycle, ptDeepTable:
		if t == ptTable && expand {
			return p.table()
		}
		return cval{tag: t}, nil
	case ptNumber:
		return cval{tag: t, num: p.f64()}, nil
	case ptInteger:
		return cval{tag: t, inum: p.i64()}, nil
	case ptString:
		n := p.u32()
		s := string(p.b[p.off : p.off+int(n)])
		p.off += int(n)
		return cval{tag: t, str: s}, nil
	}
	return cval{}, fmt.Errorf("cwire: bad value tag %d at %d", t, p.off-1)
}

func (p *cparser) table() (cval, error) {
	v := cval{tag: ptTable}
	n := p.u32()
	if p.off < len(p.b) && p.b[p.off] == ptMore {
		p.off++
		v.more = true
	}
	for i := uint32(0); i < n; i++ {
		var kv [2]cval
		var err error
		if kv[0], err = p.value(true); err != nil {
			return v, err
		}
		if kv[1], err = p.value(true); err != nil {
			return v, err
		}
		v.tbl = append(v.tbl, kv)
	}
	return v, nil
}

// cprintArg mirrors PrintArg for the C engine.
func cprintArg(v cval) string {
	switch v.tag {
	case ptString:
		return v.str
	case ptNumber:
		return NumRepr(v.num)
	case ptInteger:
		return strconv.FormatInt(v.inum, 10)
	case ptNil:
		return "nil"
	case ptFalse:
		return "false"
	case ptTrue:
		return "true"
	case ptTable:
		return "<table>"
	case ptFunction:
		return "<function>"
	case ptUserdata, ptLightUserdata:
		return "<userdata>"
	case ptThread:
		return "<thread>"
	}
	return fmt.Sprintf("<tag %d>", v.tag)
}

// crepr mirrors ValueRepr for the C engine.
func crepr(v cval, depth int) string {
	switch v.tag {
	case ptString:
		return strconv.Quote(v.str)
	case ptNumber:
		return NumRepr(v.num)
	case ptInteger:
		return strconv.FormatInt(v.inum, 10)
	case ptNil:
		return "nil"
	case ptFalse:
		return "false"
	case ptTrue:
		return "true"
	case ptFunction:
		return "<function>"
	case ptUserdata, ptLightUserdata:
		return "<userdata>"
	case ptThread:
		return "<thread>"
	case ptCycle:
		return "<cycle>"
	case ptDeepTable:
		return "<deep>"
	case ptTable:
		out := "{"
		for i, kv := range v.tbl {
			if i > 0 {
				out += ", "
			}
			out += crepr(kv[0], depth+1) + "=" + crepr(kv[1], depth+1)
		}
		if v.more {
			if len(v.tbl) > 0 {
				out += ", "
			}
			out += "<...>"
		}
		return out + "}"
	}
	return fmt.Sprintf("<tag %d>", v.tag)
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// onEvent decodes the typed event buffer from wasm memory. (Was a method
// on the retired CLua engine; a plain function since the U8 extraction.)
func onEvent(raw []byte, kind int32, emit func(string, string)) {
	if kind == 1 { // EVT_PRINT
		p := &cparser{b: raw}
		nargs := p.u32()
		parts := make([]string, 0, nargs)
		for i := uint32(0); i < nargs; i++ {
			v, err := p.value(false)
			if err != nil {
				return
			}
			parts = append(parts, cprintArg(v))
		}
		payload := ""
		for i, s := range parts {
			if i > 0 {
				payload += "\t"
			}
			payload += s
		}
		emit("PRINT", payload)
	} else if kind == 2 { // EVT_GLOBALS
		p := &cparser{b: raw}
		v, err := p.value(true)
		if err != nil {
			emit("GLOBALS", "<decode error: "+err.Error()+">")
			return
		}
		// tables arrive in lua_next order; sort exactly like the interp
		// engine's serializer does (by normalized key text).
		emit("GLOBALS", sortedRepr(v))
	}
}

// FormatResults renders the one canonical RESULTS payload (U9a2): the
// explicit value count, then the tab-joined rendered values — the count
// is its own field so trailing-nil and truncation drift diffs even when
// the rendered values happen to agree. Tabs are prefixes (Join would
// leave a trailing tab on the zero-return line).
func FormatResults(n int, parts []string) string {
	out := strconv.Itoa(n)
	for _, p := range parts {
		out += "\t" + p
	}
	return out
}

// decodeResultsWire decodes the U9a2 results wire — [u32 count]
// followed by count enc_value(expand=1) frames — the exact shape
// lresults emits on the driver legs (the clua55 engines) and the
// script-module legs compose per result cell via rt_encode_value. The
// values render through sortedRepr (the GLOBALS renderer family), so
// returned tables compare by contents under the same sorted-key parity
// the globals snapshots already prove.
func decodeResultsWire(raw []byte) (string, error) {
	p := &cparser{b: raw}
	n := p.u32()
	parts := make([]string, 0, int(n))
	for i := uint32(0); i < n; i++ {
		v, err := p.value(true)
		if err != nil {
			return "", err
		}
		parts = append(parts, sortedRepr(v))
	}
	return FormatResults(int(n), parts), nil
}

// resultsLineFromCells builds the RESULTS line for the script-module
// legs (wasm/wazero/wazero-prod): their chunk results are staged as
// TValue cells at entryFrame+16*i, so the wire is composed by pulling
// each cell through the blob's rt_encode_value export (row 96 — the
// same expand=1 encoder lresults uses). pull returns cell i's encoded
// bytes; a pull failure degrades to an explicit marker line, never a
// silently-missing event.
func resultsLineFromCells(n int, pull func(i int) []byte) string {
	wire := make([]byte, 4, 4+16*n)
	binary.LittleEndian.PutUint32(wire, uint32(n))
	for i := 0; i < n; i++ {
		b := pull(i)
		if b == nil {
			return "<encode failed>"
		}
		wire = append(wire, b...)
	}
	payload, err := decodeResultsWire(wire)
	if err != nil {
		return "<decode error: " + err.Error() + ">"
	}
	return payload
}

// sortedRepr sorts table entries at every level to match
// serializeTable's deterministic output.
func sortedRepr(v cval) string {
	switch v.tag {
	case ptTable:
		type kv2 struct{ k, v cval }
		entries := make([]kv2, 0, len(v.tbl))
		for _, kv := range v.tbl {
			entries = append(entries, kv2{kv[0], kv[1]})
		}
		// insertion-sort by key repr (tables are small; keep it simple)
		for i := 1; i < len(entries); i++ {
			for j := i; j > 0 && creprKey(entries[j-1].k) > creprKey(entries[j].k); j-- {
				entries[j-1], entries[j] = entries[j], entries[j-1]
			}
		}
		out := "{"
		for i, e := range entries {
			if i > 0 {
				out += ", "
			}
			out += sortedRepr(e.k) + "=" + sortedRepr(e.v)
		}
		if v.more {
			if len(entries) > 0 {
				out += ", "
			}
			out += "<...>"
		}
		return out + "}"
	}
	return crepr(v, 0)
}

func creprKey(v cval) string { return crepr(v, 0) }
