package lua

import (
	"fmt"
	"strings"

	"github.com/pschlump/golua55/pm"
)

const emptyLString LString = LString("")

func OpenString(L *LState) int {
	mod := L.RegisterModule(StringLibName, strFuncs).(*LTable)
	// U1 (D8 hard cut): gfind (the 5.0/5.1 alias for gmatch) is removed.
	// U3c (row 58): the string metatable is a separate table carrying
	// the arith metamethods + __index = the library (stock
	// createmetatable shape) — the module table itself is no longer the
	// metatable, so user overrides via getmetatable("") stay clean.
	L.G.builtinMts[int(LTString)] = createStringMetatable(L, mod)
	L.Push(mod)
	return 1
}

var strFuncs = map[string]LGFunction{
	"byte":     strByte,
	"char":     strChar,
	"dump":     strDump,
	"find":     strFind,
	"format":   strFormat,
	"gmatch":   strGmatch,
	"gsub":     strGsub,
	"len":      strLen,
	"lower":    strLower,
	"match":    strMatch,
	"pack":     strPack,     // U3g (packlib.go)
	"packsize": strPacksize, //
	"unpack":   strUnpack,   //
	"rep":      strRep,
	"reverse":  strReverse,
	"sub":      strSub,
	"upper":    strUpper,
}

// posRelatI ports posrelatI: relative initial position (negative counts
// back from the end), clipped to [1, inf).
func posRelatI(pos int64, l int) int64 {
	if pos > 0 {
		return pos
	}
	if pos == 0 {
		return 1
	}
	if pos < -int64(l) {
		return 1
	}
	return int64(l) + pos + 1
}

// getEndPos ports getendpos: optional ending position (default def),
// clipped to [0, len].
func getEndPos(L *LState, arg int, def int64, l int) int64 {
	pos := optInteger(L, arg, def)
	if pos > int64(l) {
		return int64(l)
	}
	if pos >= 0 {
		return pos
	}
	if pos < -int64(l) {
		return 0
	}
	return int64(l) + pos + 1
}

// strByte ports str_byte: the byte values (integers) of every character
// starting in [i, j] (j defaults to i); an empty interval returns no
// values.
func strByte(L *LState) int {
	str := L.CheckString(1)
	l := len(str)
	pi := optInteger(L, 2, 1)
	posi := posRelatI(pi, l)
	pose := getEndPos(L, 3, pi, l)
	if posi > pose {
		return 0 // empty interval
	}
	if pose-posi >= 1<<31-1 {
		L.RaiseError("string slice too long")
	}
	n := int(pose-posi) + 1
	for i := 0; i < n; i++ {
		L.Push(LInteger(int64(str[posi-1+int64(i)])))
	}
	return n
}

// strChar ports str_char: bytes from the (unsigned-checked) arguments.
func strChar(L *LState) int {
	top := L.GetTop()
	bytes := make([]byte, top)
	for i := 1; i <= top; i++ {
		c := uint64(checkInteger(L, i))
		if c > 255 {
			L.ArgError(i, "value out of range")
		}
		bytes[i-1] = byte(c)
	}
	L.Push(LString(bytes))
	return 1
}

func strDump(L *LState) int {
	L.RaiseError("GopherLua does not support the string.dump")
	return 0
}

// pmHasSpecials ports nospecials: whether the pattern contains any of
// Lua's magic characters (a pattern without them is a plain substring).
func pmHasSpecials(p string) bool {
	return strings.ContainsAny(p, "^$*+?.([%-")
}

// strFind ports str_find_aux(find=1): init via posrelatI (start after
// the string's end fails immediately); an empty pattern or plain=true
// does a plain search; otherwise the pattern matcher. Positions are
// integers.
func strFind(L *LState) int {
	str := L.CheckString(1)
	pattern := L.CheckString(2)
	ls := len(str)
	init := int(posRelatI(optInteger(L, 3, 1), ls)) - 1
	if init > ls {
		L.Push(LNil)
		return 1
	}
	plain := false
	if L.GetTop() == 4 {
		plain = LVAsBool(L.Get(4))
	}
	if plain || !pmHasSpecials(pattern) {
		if len(pattern) == 0 {
			if init <= ls {
				L.Push(LInteger(int64(init + 1)))
				L.Push(LInteger(int64(init)))
				return 2
			}
			L.Push(LNil)
			return 1
		}
		pos := strings.Index(str[init:], pattern)
		if pos < 0 {
			L.Push(LNil)
			return 1
		}
		L.Push(LInteger(int64(init + pos + 1)))
		L.Push(LInteger(int64(init + pos + len(pattern))))
		return 2
	}

	mds, err := pm.Find(pattern, unsafeFastStringToReadOnlyBytes(str), init, 1)
	if err != nil {
		L.RaiseError("%s", err.Error())
	}
	if len(mds) == 0 {
		L.Push(LNil)
		return 1
	}
	md := mds[0]
	L.Push(LInteger(int64(md.Capture(0) + 1)))
	L.Push(LInteger(int64(md.Capture(1))))
	for i := 2; i < md.CaptureLength(); i += 2 {
		if md.IsPosCapture(i) {
			L.Push(LInteger(int64(md.Capture(i))))
		} else {
			L.Push(LString(str[md.Capture(i):md.Capture(i+1)]))
		}
	}
	return md.CaptureLength()/2 + 1
}

// strFormat lives in formatlib.go (the stock 5.5.1 port, U3c).

func strGsub(L *LState) int {
	str := L.CheckString(1)
	pat := L.CheckString(2)
	L.CheckTypes(3, LTString, LTTable, LTFunction)
	repl := L.CheckAny(3)
	limit := L.OptInt(4, -1)

	mds, err := pm.Find(pat, unsafeFastStringToReadOnlyBytes(str), 0, limit)
	if err != nil {
		L.RaiseError("%s", err.Error())
	}
	if len(mds) == 0 {
		L.SetTop(1)
		L.Push(LNumber(0))
		return 2
	}
	switch lv := repl.(type) {
	case LString:
		L.Push(LString(strGsubStr(L, str, string(lv), mds)))
	case *LTable:
		L.Push(LString(strGsubTable(L, str, lv, mds)))
	case *LFunction:
		L.Push(LString(strGsubFunc(L, str, lv, mds)))
	}
	L.Push(LNumber(len(mds)))
	return 2
}

type replaceInfo struct {
	Indicies []int
	String   string
}

func checkCaptureIndex(L *LState, m *pm.MatchData, idx int) {
	if idx <= 2 {
		return
	}
	if idx >= m.CaptureLength() {
		L.RaiseError("invalid capture index")
	}
}

func capturedString(L *LState, m *pm.MatchData, str string, idx int) string {
	checkCaptureIndex(L, m, idx)
	if idx >= m.CaptureLength() && idx == 2 {
		idx = 0
	}
	if m.IsPosCapture(idx) {
		return fmt.Sprint(m.Capture(idx))
	} else {
		return str[m.Capture(idx):m.Capture(idx+1)]
	}

}

func strGsubDoReplace(str string, info []replaceInfo) string {
	offset := 0
	buf := []byte(str)
	for _, replace := range info {
		oldlen := len(buf)
		b1 := append([]byte(""), buf[0:offset+replace.Indicies[0]]...)
		b2 := []byte("")
		index2 := offset + replace.Indicies[1]
		if index2 <= len(buf) {
			b2 = append(b2, buf[index2:len(buf)]...)
		}
		buf = append(b1, replace.String...)
		buf = append(buf, b2...)
		offset += len(buf) - oldlen
	}
	return string(buf)
}

func strGsubStr(L *LState, str string, repl string, matches []*pm.MatchData) string {
	infoList := make([]replaceInfo, 0, len(matches))
	for _, match := range matches {
		start, end := match.Capture(0), match.Capture(1)
		sc := newFlagScanner('%', "", "", repl)
		for c, eos := sc.Next(); !eos; c, eos = sc.Next() {
			if !sc.ChangeFlag {
				if sc.HasFlag {
					if c >= '0' && c <= '9' {
						sc.AppendString(capturedString(L, match, str, 2*(int(c)-48)))
					} else {
						sc.AppendChar('%')
						sc.AppendChar(c)
					}
					sc.HasFlag = false
				} else {
					sc.AppendChar(c)
				}
			}
		}
		infoList = append(infoList, replaceInfo{[]int{start, end}, sc.String()})
	}

	return strGsubDoReplace(str, infoList)
}

func strGsubTable(L *LState, str string, repl *LTable, matches []*pm.MatchData) string {
	infoList := make([]replaceInfo, 0, len(matches))
	for _, match := range matches {
		idx := 0
		if match.CaptureLength() > 2 { // has captures
			idx = 2
		}
		var value LValue
		if match.IsPosCapture(idx) {
			value = L.GetTable(repl, LNumber(match.Capture(idx)))
		} else {
			value = L.GetField(repl, str[match.Capture(idx):match.Capture(idx+1)])
		}
		if !LVIsFalse(value) {
			infoList = append(infoList, replaceInfo{[]int{match.Capture(0), match.Capture(1)}, LVAsString(value)})
		}
	}
	return strGsubDoReplace(str, infoList)
}

func strGsubFunc(L *LState, str string, repl *LFunction, matches []*pm.MatchData) string {
	infoList := make([]replaceInfo, 0, len(matches))
	for _, match := range matches {
		start, end := match.Capture(0), match.Capture(1)
		L.Push(repl)
		nargs := 0
		if match.CaptureLength() > 2 { // has captures
			for i := 2; i < match.CaptureLength(); i += 2 {
				if match.IsPosCapture(i) {
					L.Push(LNumber(match.Capture(i)))
				} else {
					L.Push(LString(capturedString(L, match, str, i)))
				}
				nargs++
			}
		} else {
			L.Push(LString(capturedString(L, match, str, 0)))
			nargs++
		}
		L.Call(nargs, 1)
		ret := L.reg.Pop()
		if !LVIsFalse(ret) {
			infoList = append(infoList, replaceInfo{[]int{start, end}, LVAsString(ret)})
		}
	}
	return strGsubDoReplace(str, infoList)
}

type strMatchData struct {
	str     string
	pos     int
	matches []*pm.MatchData
}

// strGmatchIter keeps its state in an UPVALUE (stock gmatch_aux reads
// the GMatchState from upvalue 3) so the iterator also works called
// without arguments — e.g. resumed across a coroutine via
// coroutine.wrap(string.gmatch(...)).
func strGmatchIter(L *LState) int {
	md, _ := L.Get(UpvalueIndex(1)).(*LUserData)
	state := md.Value.(*strMatchData)
	str := state.str
	matches := state.matches
	idx := state.pos
	state.pos++
	if idx == len(matches) {
		return 0
	}
	match := matches[idx]
	if match.CaptureLength() == 2 {
		L.Push(LString(str[match.Capture(0):match.Capture(1)]))
		return 1
	}

	for i := 2; i < match.CaptureLength(); i += 2 {
		if match.IsPosCapture(i) {
			L.Push(LInteger(int64(match.Capture(i))))
		} else {
			L.Push(LString(str[match.Capture(i):match.Capture(i+1)]))
		}
	}
	return match.CaptureLength()/2 - 1
}

// strGmatch ports gmatch: the iterator closure carries the match state
// in an upvalue; returns iterator + the string + 0 for the generic for.
// 5.5 gained an init position (negative counts from the end).
func strGmatch(L *LState) int {
	str := L.CheckString(1)
	pattern := L.CheckString(2)
	ls := len(str)
	init := int(posRelatI(optInteger(L, 3, 1), ls)) - 1
	if init < 0 {
		init = 0
	}
	mds, err := pm.Find(pattern, []byte(str), init, -1)
	if err != nil {
		L.RaiseError("%s", err.Error())
	}
	ud := L.NewUserData()
	ud.Value = &strMatchData{str, 0, mds}
	L.Push(L.NewClosure(strGmatchIter, ud))
	L.Push(LString(str))
	L.Push(LInteger(0))
	return 3
}

func strLen(L *LState) int {
	str := L.CheckString(1)
	L.Push(LInteger(int64(len(str))))
	return 1
}

// strLower/strUpper are byte-wise ASCII case conversions (stock runs
// tolower/toupper per byte in the C locale — not UTF-8 aware).
func strLower(L *LState) int {
	str := L.CheckString(1)
	b := []byte(str)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	L.Push(LString(b))
	return 1
}

// strMatch ports str_find_aux(find=0): init via posrelatI; returns the
// captures (or the whole match).
func strMatch(L *LState) int {
	str := L.CheckString(1)
	pattern := L.CheckString(2)
	l := len(str)
	init := int(posRelatI(optInteger(L, 3, 1), l)) - 1
	if init > l {
		L.Push(LNil)
		return 1
	}

	mds, err := pm.Find(pattern, unsafeFastStringToReadOnlyBytes(str), init, 1)
	if err != nil {
		L.RaiseError("%s", err.Error())
	}
	if len(mds) == 0 {
		L.Push(LNil)
		return 1
	}
	md := mds[0]
	nsubs := md.CaptureLength() / 2
	switch nsubs {
	case 1:
		L.Push(LString(str[md.Capture(0):md.Capture(1)]))
		return 1
	default:
		for i := 2; i < md.CaptureLength(); i += 2 {
			if md.IsPosCapture(i) {
				L.Push(LInteger(int64(md.Capture(i))))
			} else {
				L.Push(LString(str[md.Capture(i):md.Capture(i+1)]))
			}
		}
		return nsubs - 1
	}
}

// strRep ports str_rep: n copies joined by sep (n <= 0 or both empty →
// ""), with stock's "resulting string too large" overflow guard.
func strRep(L *LState) int {
	str := L.CheckString(1)
	n := checkInteger(L, 2)
	sep := L.OptString(3, "")
	slen, lsep := int64(len(str)), int64(len(sep))
	const maxSize = int64(1)<<62 - 1 // stock MAX_SIZE over size_t/lua_Integer
	if n <= 0 || slen|lsep == 0 {
		L.Push(emptyLString)
		return 1
	}
	if slen > maxSize-lsep || slen+lsep > maxSize/n {
		L.RaiseError("resulting string too large")
	}
	buf := make([]byte, 0, int(n*(slen+lsep))-int(lsep))
	for i := int64(0); i < n; i++ {
		if i > 0 {
			buf = append(buf, sep...)
		}
		buf = append(buf, str...)
	}
	L.Push(LString(buf))
	return 1
}

func strReverse(L *LState) int {
	str := L.CheckString(1)
	bts := []byte(str)
	out := make([]byte, len(bts))
	for i, j := 0, len(bts)-1; j >= 0; i, j = i+1, j-1 {
		out[i] = bts[j]
	}
	L.Push(LString(string(out)))
	return 1
}

// strSub ports str_sub: posrelatI start, getendpos end (default -1).
func strSub(L *LState) int {
	str := L.CheckString(1)
	l := len(str)
	start := posRelatI(checkInteger(L, 2), l)
	end := getEndPos(L, 3, -1, l)
	if start <= end {
		L.Push(LString(str[start-1 : end]))
	} else {
		L.Push(emptyLString)
	}
	return 1
}

func strUpper(L *LState) int {
	str := L.CheckString(1)
	b := []byte(str)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	L.Push(LString(b))
	return 1
}

//
