package lua

// dump.go — string.dump and binary-chunk load (U5f, row 25's closure):
// ports of stock 5.5.1 ldump.c/lundump.c over the fork's FunctionProto.
// The byte format is stock's exactly — signature, LUAC_VERSION 0x55,
// MSB-first varints, zigzag integers, the dedup'd string table, and the
// lineinfo/abslineinfo delta encoding — so dumps cross-load with C Lua
// 5.5.1 in both directions. The one degenerate liberty: the writer
// emits an ABSLINEINFO entry per instruction (valid for stock's decoder,
// which anchors every query at the entry itself) instead of packing
// small deltas; dumps are slightly larger, semantics identical.

import (
	"encoding/binary"
	"fmt"
	"math"
)

const (
	luacSignature = "\x1bLua"
	luacVersion   = 0x55 // 5*16+5
	luacFormat    = 0
	luacData      = "\x19\x93\r\n\x1a\n"
	absLineInfo   = -0x80
)

var (
	luacIntChk  int64   = -0x5678
	luacInstChk uint32  = 0x12345678
	luacNumChk  float64 = -370.5
)

// stock's variant tags (lobject.h makevariant): the tag byte that
// prefixes each dumped constant
const (
	tagVNIL    = 0x00
	tagVFALSE  = 0x01
	tagVTRUE   = 0x11
	tagVNUMINT = 0x03
	tagVNUMFLT = 0x13
	tagVSHRSTR = 0x04
	tagVLNGSTR = 0x14
)

/* dump {{{ */

type dumpState struct {
	buf    []byte
	offset int
	strip  bool
	// strs interns dumped strings so repeats reuse the earlier index
	// (stock's aux table); index 0 is reserved for NULL
	strs map[string]uint64
	nstr uint64
}

func (d *dumpState) block(b []byte) {
	d.buf = append(d.buf, b...)
	d.offset += len(b)
}

func (d *dumpState) byte_(x byte) {
	d.buf = append(d.buf, x)
	d.offset++
}

// varint writes x MSB-first, 7 bits per byte, high bit chaining.
func (d *dumpState) varint(x uint64) {
	var buff [10]byte
	n := 1
	buff[9] = byte(x & 0x7f)
	for x >>= 7; x != 0; x >>= 7 {
		n++
		buff[10-n] = byte((x & 0x7f) | 0x80)
	}
	d.block(buff[10-n:])
}

func (d *dumpState) int_(x int) { d.varint(uint64(x)) }

// integer writes the zigzag form (0→0, -1→1, 1→2, -2→3, ...).
func (d *dumpState) integer(x int64) {
	if x >= 0 {
		d.varint(uint64(2 * x))
	} else {
		d.varint(uint64(2*^x) + 1)
	}
}

func (d *dumpState) number(x float64) {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], math.Float64bits(x))
	d.block(b[:])
}

// align pads with zeros to a multiple of n (the code and abslineinfo
// vectors are alignment-sensitive in the stock format).
func (d *dumpState) align(n int) {
	if pad := n - d.offset%n; pad < n {
		d.block(make([]byte, pad))
	}
}

func (d *dumpState) str(s string, null bool) {
	if null {
		d.varint(0)
		d.varint(0)
		return
	}
	if idx, ok := d.strs[s]; ok {
		d.varint(0)
		d.varint(idx)
		return
	}
	d.varint(uint64(len(s) + 1))
	d.block(append([]byte(s), 0))
	d.nstr++
	d.strs[s] = d.nstr
}

func (d *dumpState) header() {
	d.block([]byte(luacSignature))
	d.byte_(luacVersion)
	d.byte_(luacFormat)
	d.block([]byte(luacData))
	// checknum-style: sizeof + the sample value
	d.byte_(4)
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], uint32(int32(luacIntChk)))
	d.block(b[:])
	d.byte_(4)
	binary.LittleEndian.PutUint32(b[:], luacInstChk)
	d.block(b[:])
	d.byte_(8)
	var b8 [8]byte
	binary.LittleEndian.PutUint64(b8[:], uint64(luacIntChk))
	d.block(b8[:])
	d.byte_(8)
	binary.LittleEndian.PutUint64(b8[:], math.Float64bits(luacNumChk))
	d.block(b8[:])
}

func (d *dumpState) code(p *FunctionProto) {
	d.int_(len(p.Code))
	d.align(4)
	b := make([]byte, 4*len(p.Code))
	for i, inst := range p.Code {
		binary.LittleEndian.PutUint32(b[4*i:], inst)
	}
	d.block(b)
}

func (d *dumpState) constants(p *FunctionProto) {
	d.int_(len(p.Constants))
	for _, c := range p.Constants {
		switch v := c.(type) {
		case *LNilType:
			d.byte_(tagVNIL)
		case LBool:
			if bool(v) {
				d.byte_(tagVTRUE)
			} else {
				d.byte_(tagVFALSE)
			}
		case LInteger:
			d.byte_(tagVNUMINT)
			d.integer(int64(v))
		case LNumber:
			d.byte_(tagVNUMFLT)
			d.number(float64(v))
		case LString:
			if len(v) < 40 {
				d.byte_(tagVSHRSTR)
			} else {
				d.byte_(tagVLNGSTR)
			}
			d.str(string(v), false)
		default:
			panic(fmt.Sprintf("dump: unexpected constant type %T", c))
		}
	}
}

func (d *dumpState) upvalues(p *FunctionProto) {
	d.int_(len(p.Upvalues))
	for _, uv := range p.Upvalues {
		instack := byte(0)
		if uv.InStack {
			instack = 1
		}
		d.byte_(instack)
		d.byte_(uv.Idx)
		d.byte_(0) // kind — unused by the fork
	}
}

func (d *dumpState) protos(p *FunctionProto) {
	d.int_(len(p.FunctionPrototypes))
	for _, np := range p.FunctionPrototypes {
		d.function(np)
	}
}

func (d *dumpState) debug(p *FunctionProto) {
	// lineinfo: the degenerate all-absolute encoding — every instruction
	// carries an ABSLINEINFO byte plus its own (pc, line) entry
	n := 0
	if !d.strip {
		n = len(p.DbgSourcePositions)
	}
	d.int_(n)
	if n > 0 {
		lineinfo := make([]byte, n)
		for i := range lineinfo {
			lineinfo[i] = 0x80
		}
		d.block(lineinfo)
		d.int_(n)
		d.align(4)
		abs := make([]byte, 8*n)
		for pc, line := range p.DbgSourcePositions {
			binary.LittleEndian.PutUint32(abs[8*pc:], uint32(pc))
			binary.LittleEndian.PutUint32(abs[8*pc+4:], uint32(line))
		}
		d.block(abs)
	} else {
		d.int_(0)
	}
	n = 0
	if !d.strip {
		n = len(p.DbgLocals)
	}
	d.int_(n)
	for _, lv := range p.DbgLocals {
		null := d.strip
		name := ""
		if lv != nil {
			name = lv.Name
		}
		d.str(name, null || name == "")
		var start, end int
		if lv != nil && !d.strip {
			start, end = lv.StartPc, lv.EndPc
		}
		d.int_(start)
		d.int_(end)
	}
	n = 0
	if !d.strip {
		n = len(p.Upvalues)
	}
	d.int_(n)
	for _, uv := range p.Upvalues {
		d.str(uv.Name, d.strip || uv.Name == "")
	}
}

func (d *dumpState) function(p *FunctionProto) {
	d.int_(p.LineDefined)
	d.int_(p.LastLineDefined)
	d.byte_(p.NumParameters)
	d.byte_(p.IsVarArg &^ 4) // PF_FIXED is loader-side state
	d.byte_(p.NumUsedRegisters)
	d.code(p)
	d.constants(p)
	d.upvalues(p)
	d.protos(p)
	d.str(p.SourceName, d.strip || p.SourceName == "")
	d.debug(p)
}

// dumpProto serializes a FunctionProto in stock 5.5.1 precompiled form.
func dumpProto(p *FunctionProto, strip bool) []byte {
	d := &dumpState{strs: map[string]uint64{}, strip: strip}
	d.header()
	d.byte_(byte(len(p.Upvalues)))
	d.function(p)
	return d.buf
}

/* }}} */

/* undump {{{ */

type loadState struct {
	data   []byte
	pos    int
	name   string
	offset int
	// strs is the dedup list: index → string (stock's anchor table)
	strs map[uint64]string
}

func (s *loadState) fail(why string) error {
	return fmt.Errorf("%s: bad binary format (%s)", s.name, why)
}

func (s *loadState) read(n int) ([]byte, error) {
	if s.pos+n > len(s.data) {
		return nil, s.fail("truncated chunk")
	}
	b := s.data[s.pos : s.pos+n]
	s.pos += n
	s.offset += n
	return b, nil
}

func (s *loadState) byte_() (byte, error) {
	b, err := s.read(1)
	if err != nil {
		return 0, err
	}
	return b[0], nil
}

func (s *loadState) varint() (uint64, error) {
	var x uint64
	for {
		b, err := s.byte_()
		if err != nil {
			return 0, err
		}
		// stock guards against values exceeding the width of the
		// unsigned type; anything that survives 10 chunks of 7 bits is
		// absurd for our uses, and the consumers below re-check ranges
		x = (x << 7) | uint64(b&0x7f)
		if b&0x80 == 0 {
			return x, nil
		}
	}
}

func (s *loadState) int_() (int, error) {
	v, err := s.varint()
	if err != nil {
		return 0, err
	}
	if v > math.MaxInt32 {
		return 0, s.fail("integer overflow")
	}
	return int(v), nil
}

func (s *loadState) integer() (int64, error) {
	cx, err := s.varint()
	if err != nil {
		return 0, err
	}
	if cx&1 != 0 {
		return int64(^(cx >> 1)), nil
	}
	return int64(cx >> 1), nil
}

func (s *loadState) number() (float64, error) {
	b, err := s.read(8)
	if err != nil {
		return 0, err
	}
	return math.Float64frombits(binary.LittleEndian.Uint64(b)), nil
}

func (s *loadState) align(n int) error {
	if pad := n - s.offset%n; pad < n {
		_, err := s.read(pad)
		return err
	}
	return nil
}

// str reads a (nullable, possibly deduplicated) string.
func (s *loadState) str() (string, error) {
	size, err := s.varint()
	if err != nil {
		return "", err
	}
	if size == 0 {
		idx, err := s.varint()
		if err != nil {
			return "", err
		}
		if idx == 0 {
			return "", nil
		}
		if str, ok := s.strs[idx]; ok {
			return str, nil
		}
		return "", s.fail("invalid string index")
	}
	b, err := s.read(int(size))
	if err != nil {
		return "", err
	}
	str := string(b[:len(b)-1]) // drop the trailing NUL
	s.strs[uint64(len(s.strs)+1)] = str
	return str, nil
}

func (s *loadState) literal(lit, msg string) error {
	b, err := s.read(len(lit))
	if err != nil {
		return err
	}
	if string(b) != lit {
		return s.fail(msg)
	}
	return nil
}

func (s *loadState) checknum(size int, read func() ([]byte, error), equal func([]byte) bool, tname string) error {
	sz, err := s.byte_()
	if err != nil {
		return err
	}
	if int(sz) != size {
		return s.fail(fmt.Sprintf("%s size mismatch", tname))
	}
	b, err := read()
	if err != nil {
		return err
	}
	if !equal(b) {
		return s.fail(fmt.Sprintf("%s format mismatch", tname))
	}
	return nil
}

func (s *loadState) header() error {
	if err := s.literal(luacSignature[1:], "not a binary chunk"); err != nil {
		return err
	}
	if v, err := s.byte_(); err != nil {
		return err
	} else if v != luacVersion {
		return s.fail("version mismatch")
	}
	if v, err := s.byte_(); err != nil {
		return err
	} else if v != luacFormat {
		return s.fail("format mismatch")
	}
	if err := s.literal(luacData, "corrupted chunk"); err != nil {
		return err
	}
	// int / Instruction / lua_Integer / lua_Number, sizeof + sample
	for _, chk := range []struct {
		tname string
		size  int
		val   []byte
	}{
		{"int", 4, le32(uint32(int32(luacIntChk)))},
		{"instruction", 4, le32(luacInstChk)},
		{"Lua integer", 8, le64(uint64(luacIntChk))},
		{"Lua number", 8, le64(math.Float64bits(luacNumChk))},
	} {
		want := chk.val
		if err := s.checknum(chk.size, func() ([]byte, error) { return s.read(chk.size) },
			func(b []byte) bool { return string(b) == string(want) }, chk.tname); err != nil {
			return err
		}
	}
	return nil
}

func le32(v uint32) []byte {
	var b [4]byte
	binary.LittleEndian.PutUint32(b[:], v)
	return b[:]
}

func le64(v uint64) []byte {
	var b [8]byte
	binary.LittleEndian.PutUint64(b[:], v)
	return b[:]
}

func (s *loadState) code(p *FunctionProto) error {
	n, err := s.int_()
	if err != nil {
		return err
	}
	if n < 0 {
		return s.fail("bad code size")
	}
	if err := s.align(4); err != nil {
		return err
	}
	b, err := s.read(4 * n)
	if err != nil {
		return err
	}
	p.Code = make([]uint32, n)
	for i := range p.Code {
		p.Code[i] = binary.LittleEndian.Uint32(b[4*i:])
	}
	return nil
}

func (s *loadState) constants(p *FunctionProto) error {
	n, err := s.int_()
	if err != nil {
		return err
	}
	p.Constants = make([]LValue, n)
	for i := 0; i < n; i++ {
		t, err := s.byte_()
		if err != nil {
			return err
		}
		switch t {
		case tagVNIL:
			p.Constants[i] = LNil
		case tagVFALSE:
			p.Constants[i] = LFalse
		case tagVTRUE:
			p.Constants[i] = LTrue
		case tagVNUMFLT:
			v, err := s.number()
			if err != nil {
				return err
			}
			p.Constants[i] = LNumber(v)
		case tagVNUMINT:
			v, err := s.integer()
			if err != nil {
				return err
			}
			p.Constants[i] = LInteger(v)
		case tagVSHRSTR, tagVLNGSTR:
			v, err := s.str()
			if err != nil {
				return err
			}
			if v == "" {
				return s.fail("bad format for constant string")
			}
			p.Constants[i] = LString(v)
		default:
			return s.fail("invalid constant")
		}
	}
	return nil
}

func (s *loadState) upvalues(p *FunctionProto) error {
	n, err := s.int_()
	if err != nil {
		return err
	}
	p.Upvalues = make([]Upvaldesc, n)
	// the descriptor count is authoritative (the compiler's NumUpvalues
	// mirrors len(DbgUpvalues); a stripped blob keeps descriptors but
	// drops the names)
	p.NumUpvalues = uint8(n)
	for i := 0; i < n; i++ {
		instack, err := s.byte_()
		if err != nil {
			return err
		}
		idx, err := s.byte_()
		if err != nil {
			return err
		}
		if _, err := s.byte_(); err != nil { // kind — unused
			return err
		}
		p.Upvalues[i].InStack = instack != 0
		p.Upvalues[i].Idx = idx
	}
	return nil
}

func (s *loadState) protos(p *FunctionProto) error {
	n, err := s.int_()
	if err != nil {
		return err
	}
	p.FunctionPrototypes = make([]*FunctionProto, n)
	for i := 0; i < n; i++ {
		np := &FunctionProto{EnvUpvalueIndex: -1}
		if err := s.function(np); err != nil {
			return err
		}
		p.FunctionPrototypes[i] = np
	}
	return nil
}

func (s *loadState) debug(p *FunctionProto) error {
	n, err := s.int_()
	if err != nil {
		return err
	}
	var lineinfo []byte
	if n > 0 {
		b, err := s.read(n)
		if err != nil {
			return err
		}
		lineinfo = b
	}
	nAbs, err := s.int_()
	if err != nil {
		return err
	}
	type absEntry struct{ pc, line int32 }
	abs := []absEntry{}
	if nAbs > 0 {
		if err := s.align(4); err != nil {
			return err
		}
		b, err := s.read(8 * nAbs)
		if err != nil {
			return err
		}
		for i := 0; i < nAbs; i++ {
			abs = append(abs, absEntry{
				int32(binary.LittleEndian.Uint32(b[8*i:])),
				int32(binary.LittleEndian.Uint32(b[8*i+4:])),
			})
		}
	}
	// reconstruct absolute lines per pc, stock luaG_getfuncline's walk:
	// the baseline starts at linedefined; an ABSLINEINFO byte anchors to
	// the matching entry, other bytes are deltas
	if len(lineinfo) > 0 {
		lines := make([]int, len(lineinfo))
		cur := p.LineDefined
		ai := 0
		for pc, raw := range lineinfo {
			if int8(raw) == absLineInfo {
				for ai < len(abs) && int(abs[ai].pc) < pc {
					ai++
				}
				if ai < len(abs) && int(abs[ai].pc) == pc {
					cur = int(abs[ai].line)
				}
			} else {
				cur += int(int8(raw))
			}
			lines[pc] = cur
		}
		p.DbgSourcePositions = lines
	}
	nLoc, err := s.int_()
	if err != nil {
		return err
	}
	if nLoc > 0 {
		p.DbgLocals = make([]*DbgLocalInfo, nLoc)
		for i := 0; i < nLoc; i++ {
			name, err := s.str()
			if err != nil {
				return err
			}
			start, err := s.int_()
			if err != nil {
				return err
			}
			end, err := s.int_()
			if err != nil {
				return err
			}
			p.DbgLocals[i] = &DbgLocalInfo{Name: name, StartPc: start, EndPc: end}
		}
	}
	nUp, err := s.int_()
	if err != nil {
		return err
	}
	if nUp != 0 && nUp != len(p.Upvalues) {
		return s.fail("corrupted chunk")
	}
	if nUp != 0 {
		p.DbgUpvalues = make([]string, nUp)
		for i := 0; i < nUp; i++ {
			name, err := s.str()
			if err != nil {
				return err
			}
			p.DbgUpvalues[i] = name
			if i < len(p.Upvalues) {
				p.Upvalues[i].Name = name
			}
		}
		// the compiler records where _ENV lives by NAME (compile.go's
		// Upvalues.Find); stripped blobs keep the loader-side main-proto
		// fallback in undumpProto
		p.EnvUpvalueIndex = -1
		for i, name := range p.DbgUpvalues {
			if name == "_ENV" {
				p.EnvUpvalueIndex = int8(i)
				break
			}
		}
	}
	return nil
}

func (s *loadState) function(p *FunctionProto) error {
	var err error
	if p.LineDefined, err = s.int_(); err != nil {
		return err
	}
	if p.LastLineDefined, err = s.int_(); err != nil {
		return err
	}
	if b, err := s.byte_(); err != nil {
		return err
	} else {
		p.NumParameters = b
	}
	if b, err := s.byte_(); err != nil {
		return err
	} else {
		p.IsVarArg = b &^ 4 // drop PF_FIXED
	}
	if b, err := s.byte_(); err != nil {
		return err
	} else {
		p.NumUsedRegisters = b
	}
	if err := s.code(p); err != nil {
		return err
	}
	if err := s.constants(p); err != nil {
		return err
	}
	if err := s.upvalues(p); err != nil {
		return err
	}
	if err := s.protos(p); err != nil {
		return err
	}
	if src, err := s.str(); err != nil {
		return err
	} else {
		p.SourceName = src
		// the stream's empty slot is stock's NULL source (an empty
		// TString dumps as size 0 too — indistinguishable, and stock's
		// load yields NULL for both): mark it so getinfo/where take the
		// "?" shapes instead of `[string ""]`
		p.SourceIsNull = src == ""
	}
	return s.debug(p)
}

// undumpProto parses a stock 5.5.1 precompiled chunk into a main
// FunctionProto. data must start with the binary signature.
func undumpProto(data []byte, chunkname string) (*FunctionProto, error) {
	name := chunkname
	if len(name) > 0 && (name[0] == '@' || name[0] == '=') {
		name = name[1:]
	} else if len(name) > 0 && name[0] == luacSignature[0] {
		name = "binary string"
	}
	s := &loadState{data: data, pos: 1, offset: 1, name: name, strs: map[uint64]string{}}
	if err := s.header(); err != nil {
		return nil, err
	}
	nup, err := s.byte_()
	if err != nil {
		return nil, err
	}
	p := &FunctionProto{EnvUpvalueIndex: -1}
	if err := s.function(p); err != nil {
		return nil, err
	}
	if int(nup) != len(p.Upvalues) {
		return nil, s.fail("corrupted chunk")
	}
	// a main proto's upvalue 0 is always _ENV (the 5.2 scheme); the
	// loader seeds the cell regardless of whether names were stripped
	if len(p.Upvalues) > 0 {
		p.EnvUpvalueIndex = 0
	}
	return p, nil
}

/* }}} */
