// Lua pattern match functions for Go
package pm

import (
	"fmt"
)

const (
	EOS               = -1
	_UNKNOWN          = -2
	maxRecursionLevel = 1000000
)

/* Error {{{ */

type Error struct {
	Pos     int
	Message string
}

func newError(pos int, message string, args ...interface{}) *Error {
	if len(args) == 0 {
		return &Error{pos, message}
	}
	return &Error{pos, fmt.Sprintf(message, args...)}
}

func (e *Error) Error() string {
	switch e.Pos {
	case EOS:
		return fmt.Sprintf("%s at EOS", e.Message)
	case _UNKNOWN:
		return fmt.Sprintf("%s", e.Message)
	default:
		return fmt.Sprintf("%s at %d", e.Message, e.Pos)
	}
}

/* }}} */

/* MatchData {{{ */

type MatchData struct {
	// captured positions
	// layout
	// xxxx xxxx xxxx xxx0 : caputured positions
	// xxxx xxxx xxxx xxx1 : position captured positions
	captures []uint32
}

func newMatchState() *MatchData { return &MatchData{[]uint32{}} }

func (st *MatchData) addPosCapture(s, pos int) {
	for s+1 >= len(st.captures) {
		st.captures = append(st.captures, 0)
	}
	st.captures[s] = (uint32(pos) << 1) | 1
	st.captures[s+1] = (uint32(pos) << 1) | 1
}

func (st *MatchData) setCapture(s, pos int) uint32 {
	for s >= len(st.captures) {
		st.captures = append(st.captures, 0)
	}
	v := st.captures[s]
	st.captures[s] = (uint32(pos) << 1)
	return v
}

func (st *MatchData) restoreCapture(s int, pos uint32) { st.captures[s] = pos }

func (st *MatchData) CaptureLength() int { return len(st.captures) }

func (st *MatchData) IsPosCapture(idx int) bool { return (st.captures[idx] & 1) == 1 }

func (st *MatchData) Capture(idx int) int { return int(st.captures[idx] >> 1) }

/* }}} */

/* scanner {{{ */

type scannerState struct {
	Pos     int
	started bool
}

type scanner struct {
	src   []byte
	State scannerState
	saved scannerState
}

func newScanner(src []byte) *scanner {
	return &scanner{
		src: src,
		State: scannerState{
			Pos:     0,
			started: false,
		},
		saved: scannerState{},
	}
}

func (sc *scanner) Length() int { return len(sc.src) }

func (sc *scanner) Next() int {
	if !sc.State.started {
		sc.State.started = true
		if len(sc.src) == 0 {
			sc.State.Pos = EOS
		}
	} else {
		sc.State.Pos = sc.NextPos()
	}
	if sc.State.Pos == EOS {
		return EOS
	}
	return int(sc.src[sc.State.Pos])
}

func (sc *scanner) CurrentPos() int {
	return sc.State.Pos
}

func (sc *scanner) NextPos() int {
	if sc.State.Pos == EOS || sc.State.Pos >= len(sc.src)-1 {
		return EOS
	}
	if !sc.State.started {
		return 0
	} else {
		return sc.State.Pos + 1
	}
}

func (sc *scanner) Peek() int {
	cureof := sc.State.Pos == EOS
	ch := sc.Next()
	if !cureof {
		if sc.State.Pos == EOS {
			sc.State.Pos = len(sc.src) - 1
		} else {
			sc.State.Pos--
			if sc.State.Pos < 0 {
				sc.State.Pos = 0
				sc.State.started = false
			}
		}
	}
	return ch
}

func (sc *scanner) Save() { sc.saved = sc.State }

func (sc *scanner) Restore() { sc.State = sc.saved }

/* }}} */

/* bytecode {{{ */

type opCode int

const (
	opChar opCode = iota
	opMatch
	opTailMatch
	opJmp
	opSplit
	opSave
	opPSave
	opBrace
	opNumber
	opFrontier // 5.2 %f(set): empty-width frontier match
	opPanic    // malformed item: always errors when reached (stock lstrlib raises at match time)
)

type inst struct {
	OpCode   opCode
	Class    class
	Operand1 int
	Operand2 int
}

/* }}} */

/* classes {{{ */

type class interface {
	Matches(ch int) bool
}

// frontierPattern is %f(set): matches the empty string at a transition
// from a char NOT in set to a char IN set (both boundaries compare
// against '\0' beyond the subject ends, like stock's matchbalance-less
// frontier implementation).
type frontierPattern struct {
	Class class
}

// badClass is a malformed construct that stock only rejects when the
// match REACHES it (lstrlib has no parse phase): an unterminated set or
// a trailing '%'. It compiles like any class and errors in Matches,
// with stock's text and no position suffix (pm.lua:394's battery).
type badClass struct {
	Msg string
}

func (pn *badClass) Matches(ch int) bool {
	panic(newError(_UNKNOWN, "%s", pn.Msg))
}

type dotClass struct{}

func (pn *dotClass) Matches(ch int) bool { return true }

type charClass struct {
	Ch int
}

func (pn *charClass) Matches(ch int) bool { return pn.Ch == ch }

type singleClass struct {
	Class int
}

func (pn *singleClass) Matches(ch int) bool {
	ret := false
	switch pn.Class {
	case 'a', 'A':
		ret = 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z'
	case 'c', 'C':
		ret = (0x00 <= ch && ch <= 0x1F) || ch == 0x7F
	case 'd', 'D':
		ret = '0' <= ch && ch <= '9'
	case 'g', 'G':
		// stock match_class 'g' = isgraph: printable except space
		// (lctype ISGRAPH covers 0x21..0x7E; pm.lua:56's "%g%g%g+" battery)
		ret = 0x21 <= ch && ch <= 0x7E
	case 'l', 'L':
		ret = 'a' <= ch && ch <= 'z'
	case 'p', 'P':
		ret = (0x21 <= ch && ch <= 0x2f) || (0x3a <= ch && ch <= 0x40) || (0x5b <= ch && ch <= 0x60) || (0x7b <= ch && ch <= 0x7e)
	case 's', 'S':
		switch ch {
		case ' ', '\f', '\n', '\r', '\t', '\v':
			ret = true
		}
	case 'u', 'U':
		ret = 'A' <= ch && ch <= 'Z'
	case 'w', 'W':
		ret = '0' <= ch && ch <= '9' || 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z'
	case 'x', 'X':
		ret = '0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f' || 'A' <= ch && ch <= 'F'
	case 'z', 'Z':
		// deprecated since 5.2, but 5.5.1 lstrlib still implements it
		// ("case 'z': res = (c == 0)") — the oracle is the authority
		ret = ch == 0
	default:
		return ch == pn.Class
	}
	if 'A' <= pn.Class && pn.Class <= 'Z' {
		return !ret
	}
	return ret
}

type setClass struct {
	IsNot   bool
	Classes []class
}

func (pn *setClass) Matches(ch int) bool {
	for _, class := range pn.Classes {
		if class.Matches(ch) {
			return !pn.IsNot
		}
	}
	return pn.IsNot
}

type rangeClass struct {
	Begin class
	End   class
}

func (pn *rangeClass) Matches(ch int) bool {
	switch begin := pn.Begin.(type) {
	case *charClass:
		end, ok := pn.End.(*charClass)
		if !ok {
			return false
		}
		return begin.Ch <= ch && ch <= end.Ch
	}
	return false
}

// }}}

// patterns {{{

type pattern interface{}

type singlePattern struct {
	Class class
}

type seqPattern struct {
	MustHead bool
	MustTail bool
	Patterns []pattern
}

type repeatPattern struct {
	Type  int
	Class class
}

type posCapPattern struct{}

type capPattern struct {
	Pattern pattern
}

type numberPattern struct {
	N int
}

type bracePattern struct {
	Begin int
	End   int
}

// badPattern is a malformed ITEM (not class) that stock rejects only
// when the match reaches it: compiles to opPanic, which raises the
// message with no position suffix.
type badPattern struct {
	Msg string
}

// }}}

/* parse {{{ */

func parseClass(sc *scanner, allowset bool) class {
	ch := sc.Next()
	switch ch {
	case '%':
		c := sc.Next()
		if c == EOS {
			return &badClass{"malformed pattern (ends with '%')"}
		}
		return &singleClass{c}
	case '.':
		if allowset {
			return &dotClass{}
		}
		return &charClass{ch}
	case '[':
		if allowset {
			return parseClassSet(sc)
		}
		return &charClass{ch}
	//case '^' '$', '(', ')', ']', '*', '+', '-', '?':
	//	panic(newError(sc.CurrentPos(), "invalid %c", ch))
	case EOS:
		panic(newError(sc.CurrentPos(), "unexpected EOS"))
	default:
		return &charClass{ch}
	}
}

// parseClassSet ports stock matchbracketclass's scan: a '-' is a range
// separator only when it follows a single literal-char member and
// precedes a non-']' item (stock's *(p+1) == '-' test); the range end is
// always a literal char; any other '-' is a literal member — so
// "[a-b-c]" is {a..b, '-', c} and "[a-]" is {a, '-'}.
func parseClassSet(sc *scanner) class {
	set := &setClass{false, []class{}}
	if sc.Peek() == '^' {
		set.IsNot = true
		sc.Next()
	}
	prevChar := false // the previous member is a single literal char
	for {
		ch := sc.Peek()
		switch {
		case ch == EOS:
			return &badClass{"malformed pattern (missing ']')"}
		case ch == ']' && len(set.Classes) > 0:
			sc.Next()
			return set
		case ch == '-' && prevChar:
			sc.Next() // consume '-'
			if nx := sc.Peek(); nx == ']' || nx == EOS {
				set.Classes = append(set.Classes, &charClass{'-'})
				prevChar = true
				continue
			}
			begin := set.Classes[len(set.Classes)-1]
			set.Classes = set.Classes[:len(set.Classes)-1]
			set.Classes = append(set.Classes, &rangeClass{begin, &charClass{sc.Next()}})
			prevChar = false
		default:
			_, wasChar := parseClassPeek(sc, set)
			prevChar = wasChar
		}
	}
}

// parseClassPeek appends the next member to the set and reports whether
// it was a single literal char (a range-start candidate).
func parseClassPeek(sc *scanner, set *setClass) (class, bool) {
	ch := sc.Peek()
	if ch == ']' || ch == '-' || ch == EOS {
		// first-position ']' or '-' is a literal member
		sc.Next()
		set.Classes = append(set.Classes, &charClass{ch})
		return &charClass{ch}, true
	}
	c := parseClass(sc, false)
	set.Classes = append(set.Classes, c)
	_, isChar := c.(*charClass)
	return c, isChar
}

func parsePattern(sc *scanner, toplevel bool) *seqPattern {
	pat := &seqPattern{}
	if toplevel {
		if sc.Peek() == '^' {
			sc.Next()
			pat.MustHead = true
		}
	}
	for {
		ch := sc.Peek()
		switch ch {
		case '%':
			sc.Save()
			sc.Next()
			switch sc.Peek() {
			case '0':
				// stock check_capture rejects a pattern-side '%0' when the
				// match reaches it — opNumber's range check raises
				// "invalid capture index %0" (pm.lua:256)
				sc.Next()
				pat.Patterns = append(pat.Patterns, &numberPattern{0})
			case '1', '2', '3', '4', '5', '6', '7', '8', '9':
				pat.Patterns = append(pat.Patterns, &numberPattern{sc.Next() - 48})
			case 'b':
				sc.Next()
				b1, b2 := sc.Next(), sc.Next()
				if b1 == EOS || b2 == EOS {
					// stock matchbalance: "malformed pattern (missing
					// arguments to '%b')" when the item is reached
					pat.Patterns = append(pat.Patterns, &badPattern{"malformed pattern (missing arguments to '%b')"})
				} else {
					pat.Patterns = append(pat.Patterns, &bracePattern{b1, b2})
				}
			case 'f': // 5.2 frontier pattern: %f[set]
				sc.Next()
				chk := sc.Next()
				if chk != '[' {
					// stock raises when the match reaches the item
					pat.Patterns = append(pat.Patterns, &badPattern{"missing '[' after '%f' in pattern"})
				} else {
					pat.Patterns = append(pat.Patterns, &frontierPattern{parseClassSet(sc)})
				}
			default:
				sc.Restore()
				pat.Patterns = append(pat.Patterns, &singlePattern{parseClass(sc, true)})
			}
		case '.', '[', ']':
			pat.Patterns = append(pat.Patterns, &singlePattern{parseClass(sc, true)})
		//case ']':
		//	panic(newError(sc.CurrentPos(), "invalid ']'"))
		case ')':
			if toplevel {
				// stock end_capture raises "invalid pattern capture" when
				// the match reaches the ')' item (pm.lua:392)
				pat.Patterns = append(pat.Patterns, &badPattern{"invalid pattern capture"})
				sc.Next()
			} else {
				return pat
			}
		case '(':
			sc.Next()
			if sc.Peek() == ')' {
				sc.Next()
				pat.Patterns = append(pat.Patterns, &posCapPattern{})
			} else {
				ret := &capPattern{parsePattern(sc, false)}
				if sc.Peek() != ')' {
					panic(newError(_UNKNOWN, "unfinished capture"))
				}
				sc.Next()
				pat.Patterns = append(pat.Patterns, ret)
			}
		case '*', '+', '-', '?':
			sc.Next()
			if len(pat.Patterns) > 0 {
				spat, ok := pat.Patterns[len(pat.Patterns)-1].(*singlePattern)
				if ok {
					pat.Patterns = pat.Patterns[0 : len(pat.Patterns)-1]
					pat.Patterns = append(pat.Patterns, &repeatPattern{ch, spat.Class})
					continue
				}
			}
			pat.Patterns = append(pat.Patterns, &singlePattern{&charClass{ch}})
		case '$':
			if toplevel && (sc.NextPos() == sc.Length()-1 || sc.NextPos() == EOS) {
				pat.MustTail = true
			} else {
				pat.Patterns = append(pat.Patterns, &singlePattern{&charClass{ch}})
			}
			sc.Next()
		case EOS:
			sc.Next()
			goto exit
		default:
			sc.Next()
			pat.Patterns = append(pat.Patterns, &singlePattern{&charClass{ch}})
		}
	}
exit:
	return pat
}

type iptr struct {
	insts   []inst
	capture int
}

// internPanic registers a badPattern/badClass message for an opPanic
// operand (compile-time).
func internPanic(msg string) int {
	id := len(panicMsgs)
	panicMsgs = append(panicMsgs, msg)
	return id
}

func compilePattern(p pattern, ps ...*iptr) []inst {
	var ptr *iptr
	toplevel := false
	if len(ps) == 0 {
		toplevel = true
		ptr = &iptr{[]inst{inst{opSave, nil, 0, -1}}, 2}
	} else {
		ptr = ps[0]
	}
	switch pat := p.(type) {
	case *singlePattern:
		if bc, ok := pat.Class.(*badClass); ok {
			// opChar short-circuits at end-of-subject; stock's class_end
			// rejects the malformed escape whenever the ITEM runs
			ptr.insts = append(ptr.insts, inst{opPanic, nil, internPanic(bc.Msg), -1})
		} else {
			ptr.insts = append(ptr.insts, inst{opChar, pat.Class, -1, -1})
		}
	case *seqPattern:
		for _, cp := range pat.Patterns {
			compilePattern(cp, ptr)
		}
	case *repeatPattern:
		idx := len(ptr.insts)
		switch pat.Type {
		case '*':
			ptr.insts = append(ptr.insts,
				inst{opSplit, nil, idx + 1, idx + 3},
				inst{opChar, pat.Class, -1, -1},
				inst{opJmp, nil, idx, -1})
		case '+':
			ptr.insts = append(ptr.insts,
				inst{opChar, pat.Class, -1, -1},
				inst{opSplit, nil, idx, idx + 2})
		case '-':
			ptr.insts = append(ptr.insts,
				inst{opSplit, nil, idx + 3, idx + 1},
				inst{opChar, pat.Class, -1, -1},
				inst{opJmp, nil, idx, -1})
		case '?':
			ptr.insts = append(ptr.insts,
				inst{opSplit, nil, idx + 1, idx + 2},
				inst{opChar, pat.Class, -1, -1})
		}
	case *posCapPattern:
		ptr.insts = append(ptr.insts, inst{opPSave, nil, ptr.capture, -1})
		ptr.capture += 2
	case *capPattern:
		c0, c1 := ptr.capture, ptr.capture+1
		ptr.capture += 2
		ptr.insts = append(ptr.insts, inst{opSave, nil, c0, -1})
		compilePattern(pat.Pattern, ptr)
		ptr.insts = append(ptr.insts, inst{opSave, nil, c1, -1})
	case *frontierPattern:
		ptr.insts = append(ptr.insts, inst{opFrontier, pat.Class, -1, -1})
	case *bracePattern:
		ptr.insts = append(ptr.insts, inst{opBrace, nil, pat.Begin, pat.End})
	case *numberPattern:
		ptr.insts = append(ptr.insts, inst{opNumber, nil, pat.N, -1})
	case *badPattern:
		ptr.insts = append(ptr.insts, inst{opPanic, nil, internPanic(pat.Msg), -1})
	}
	if toplevel {
		if p.(*seqPattern).MustTail {
			ptr.insts = append(ptr.insts, inst{opSave, nil, 1, -1}, inst{opTailMatch, nil, -1, -1})
		}
		ptr.insts = append(ptr.insts, inst{opSave, nil, 1, -1}, inst{opMatch, nil, -1, -1})
	}
	return ptr.insts
}

/* }}} parse */

/* VM {{{ */

// panicMsgs interns badPattern messages for the opPanic operand.
var panicMsgs []string

// Simple recursive virtual machine based on the
// "Regular Expression Matching: the Virtual Machine Approach" (https://swtch.com/~rsc/regexp/regexp2.html)
func recursiveVM(src []byte, insts []inst, pc, sp, recLevel int, ms ...*MatchData) (bool, int, *MatchData) {
	recLevel++
	if recLevel > maxRecursionLevel {
		panic(newError(_UNKNOWN, "pattern/input too complex"))
	}
	var m *MatchData
	if len(ms) == 0 {
		m = newMatchState()
	} else {
		m = ms[0]
	}
redo:
	inst := insts[pc]
	switch inst.OpCode {
	case opChar:
		if sp >= len(src) || !inst.Class.Matches(int(src[sp])) {
			return false, sp, m
		}
		pc++
		sp++
		goto redo
	case opMatch:
		return true, sp, m
	case opTailMatch:
		return sp >= len(src), sp, m
	case opJmp:
		pc = inst.Operand1
		goto redo
	case opSplit:
		if ok, nsp, _ := recursiveVM(src, insts, inst.Operand1, sp, recLevel, m); ok {
			return true, nsp, m
		}
		pc = inst.Operand2
		goto redo
	case opSave:
		s := m.setCapture(inst.Operand1, sp)
		if ok, nsp, _ := recursiveVM(src, insts, pc+1, sp, recLevel, m); ok {
			return true, nsp, m
		}
		m.restoreCapture(inst.Operand1, s)
		return false, sp, m
	case opPSave:
		m.addPosCapture(inst.Operand1, sp+1)
		pc++
		goto redo
	case opBrace:
		if sp >= len(src) || int(src[sp]) != inst.Operand1 {
			return false, sp, m
		}
		count := 1
		for sp = sp + 1; sp < len(src); sp++ {
			if int(src[sp]) == inst.Operand2 {
				count--
			}
			if count == 0 {
				pc++
				sp++
				goto redo
			}
			if int(src[sp]) == inst.Operand1 {
				count++
			}
		}
		return false, sp, m
	case opFrontier:
		// %f[set]: prev char not in set, current char in set; the
		// positions just outside the subject read as '\0'
		prev := 0
		if sp > 0 {
			prev = int(src[sp-1])
		}
		curr := 0
		if sp < len(src) {
			curr = int(src[sp])
		}
		if inst.Class.Matches(prev) || !inst.Class.Matches(curr) {
			return false, sp, m
		}
		pc++
		goto redo
	case opPanic:
		panic(newError(_UNKNOWN, "%s", panicMsgs[inst.Operand1]))
	case opNumber:
		idx := inst.Operand1 * 2
		// stock check_capture: '%0' is never a back reference; an
		// unfinished/absent capture is "invalid capture index %n"
		if inst.Operand1 == 0 || idx >= m.CaptureLength()-1 {
			// stock check_capture names the digit: "invalid capture
			// index %1" for an unfinished/absent capture (pm.lua:257)
			panic(newError(_UNKNOWN, "invalid capture index %%%d", inst.Operand1))
		}
		capture := src[m.Capture(idx):m.Capture(idx+1)]
		for i := 0; i < len(capture); i++ {
			if i+sp >= len(src) || capture[i] != src[i+sp] {
				return false, sp, m
			}
		}
		pc++
		sp += len(capture)
		goto redo
	}
	panic("should not reach here")
}

/* }}} */

/* API {{{ */

func Find(p string, src []byte, offset, limit int) (matches []*MatchData, err error) {
	defer func() {
		if v := recover(); v != nil {
			if perr, ok := v.(*Error); ok {
				err = perr
			} else {
				panic(v)
			}
		}
	}()
	pat := parsePattern(newScanner([]byte(p)), true)
	insts := compilePattern(pat)
	matches = []*MatchData{}
	// 5.3.3+ empty-match discipline (lstrlib str_gsub/gmatch_aux): a match
	// ending exactly where the last ACCEPTED match ended is rejected, and
	// the scan then advances one character — so " *" over "a b cd" yields
	// -a-b-c-d- (5 matches), never a second empty match at a previous
	// match's end position (pm.lua:186's battery). lastmatch = -1 means
	// none yet (a first match is always accepted).
	lastmatch := -1
	for sp := offset; sp <= len(src) && limit != len(matches); {
		ok, nsp, ms := recursiveVM(src, insts, 0, sp, 0)
		if ok && nsp != lastmatch {
			matches = append(matches, ms)
			lastmatch = nsp
			sp = nsp
			if pat.MustHead {
				break
			}
			continue // retry at the match end (an empty match there is rejected)
		}
		sp++
		if pat.MustHead {
			break
		}
	}
	return
}

/* }}} */
