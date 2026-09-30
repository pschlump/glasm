package parse

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"

	"github.com/pschlump/golua55/ast"
)

const EOF = -1
const whitespace1 = 1<<'\t' | 1<<' '
const whitespace2 = 1<<'\t' | 1<<'\n' | 1<<'\r' | 1<<' '

// Error is a lexer/parser error. U1 re-pins its rendering to stock Lua's
// shape — "<source>:<line>: <message> near '<token>'" — so parse failures
// diff byte-exact against the clua55 oracle (upgrade plan D7 direction;
// the texts themselves are stock 5.5 wording ported from llex.c). EOF
// errors report "<eof>" as the near token (unquoted, like txtToken on
// TK_EOS) at the last real line, matching luaX_syntaxerror.
type Error struct {
	Pos     ast.Position
	Message string
	Token   string
	// NearEOF: report "near <eof>" (unquoted) instead of "near '<token>'".
	NearEOF bool
	// Sem: a luaK_semerror-class error — stock zeroes the token before
	// luaX_syntaxerror, so the message carries no "near" part at all
	// (attribute/const/label errors; U4).
	Sem bool
	// EOFLine is the last real line number, used when Pos.Line == EOF
	// (stock reports the current line, never a sentinel, at EOZ).
	EOFLine int
}

func (e *Error) Error() string {
	pos := e.Pos
	src := ChunkID(pos.Source)
	if e.Sem {
		return fmt.Sprintf("%s:%d: %s", src, semErrorLine(e), e.Message)
	}
	if e.NearEOF || pos.Line == EOF {
		return fmt.Sprintf("%s:%d: %s near <eof>", src, e.EOFLine, e.Message)
	} else {
		return fmt.Sprintf("%s:%d: %s near '%s'", src, pos.Line, e.Message, e.Token)
	}
}

// semErrorLine is the line a semerror reports: the current token's line,
// falling back to the last real line at end-of-input (stock's ls->linenumber
// never advances past the final token).
func semErrorLine(e *Error) int {
	if e.Pos.Line == EOF {
		return e.EOFLine
	}
	return e.Pos.Line
}

// ChunkID renders a source name the way stock Lua's luaO_chunkid does
// (lobject.c): "=name" keeps the literal text after '=', "@file" keeps
// the file name (both truncated to 60), anything else becomes
// [string "src"] — cut at the first newline and truncated. Callers that
// mean a file name pass "@file"; the engines' bare case names render
// verbatim only if '@'-prefixed first.
func ChunkID(source string) string {
	const idsize = 60
	if strings.HasPrefix(source, "=") || strings.HasPrefix(source, "@") {
		s := source[1:]
		if len(s) > idsize {
			if source[0] == '=' {
				return s[:idsize-1]
			}
			return "..." + s[len(s)-(idsize-3):]
		}
		return s
	}
	const pre, pos, rets = `[string "`, `"]`, "..."
	max := idsize - len(pre) - len(pos) - len(rets) - 1
	cut := source
	if i := strings.IndexByte(cut, '\n'); i >= 0 {
		cut = cut[:i]
	}
	if len(source) < max && !strings.Contains(source, "\n") {
		return pre + source + pos
	}
	if len(cut) > max {
		cut = cut[:max]
	}
	return pre + cut + rets + pos
}

func writeChar(buf *bytes.Buffer, c int) { buf.WriteByte(byte(c)) }

func isDecimal(ch int) bool { return '0' <= ch && ch <= '9' }

func isIdent(ch int, pos int) bool {
	return ch == '_' || 'A' <= ch && ch <= 'Z' || 'a' <= ch && ch <= 'z' || isDecimal(ch) && pos > 0
}

func isDigit(ch int) bool {
	return '0' <= ch && ch <= '9' || 'a' <= ch && ch <= 'f' || 'A' <= ch && ch <= 'F'
}

type Scanner struct {
	Pos ast.Position
	// lastLine is the last real (non-EOF) line number seen; it is what
	// stock Lua's linenumber holds when the scanner hits EOZ.
	lastLine int
	reader   *bufio.Reader
}

func NewScanner(reader io.Reader, source string) *Scanner {
	return &Scanner{
		Pos: ast.Position{
			Source: source,
			Line:   1,
			Column: 0,
		},
		lastLine: 1,
		reader:   bufio.NewReaderSize(reader, 4096),
	}
}

func (sc *Scanner) Error(tok string, msg string) *Error {
	return &Error{Pos: sc.Pos, Message: msg, Token: tok, EOFLine: sc.lastLine}
}

// ErrorEOF reports msg with "near <eof>" (the txtToken rendering of
// TK_EOS: unquoted, at the last real line).
func (sc *Scanner) ErrorEOF(msg string) *Error {
	return &Error{Pos: sc.Pos, Message: msg, NearEOF: true, EOFLine: sc.lastLine}
}

func (sc *Scanner) TokenError(tok ast.Token, msg string) *Error {
	eof := tok.Pos.Line
	if eof == EOF {
		eof = sc.lastLine
	}
	return &Error{Pos: tok.Pos, Message: msg, Token: tok.Str, EOFLine: eof}
}

func (sc *Scanner) readNext() int {
	ch, err := sc.reader.ReadByte()
	if err == io.EOF {
		return EOF
	}
	return int(ch)
}

func (sc *Scanner) Newline(ch int) {
	if ch < 0 {
		return
	}
	sc.Pos.Line += 1
	sc.Pos.Column = 0
	next := sc.Peek()
	if ch == '\n' && next == '\r' || ch == '\r' && next == '\n' {
		sc.reader.ReadByte()
	}
}

func (sc *Scanner) Next() int {
	ch := sc.readNext()
	switch ch {
	case '\n', '\r':
		sc.Newline(ch)
		ch = int('\n')
	case EOF:
		if sc.Pos.Line != EOF {
			sc.lastLine = sc.Pos.Line
		}
		sc.Pos.Line = EOF
		sc.Pos.Column = 0
	default:
		sc.Pos.Column++
	}
	return ch
}

func (sc *Scanner) Peek() int {
	ch := sc.readNext()
	if ch != EOF {
		sc.reader.UnreadByte()
	}
	return ch
}

func (sc *Scanner) skipWhiteSpace(whitespace int64) int {
	ch := sc.Next()
	for ; whitespace&(1<<uint(ch)) != 0; ch = sc.Next() {
	}
	return ch
}

func (sc *Scanner) skipComments(ch int) error {
	// multiline comment
	if sc.Peek() == '[' {
		ch = sc.Next()
		if sc.Peek() == '[' || sc.Peek() == '=' {
			var buf bytes.Buffer
			// a comment shares the long-string scanner (errors report
			// "comment", per stock read_long_string with seminfo NULL)
			return sc.scanMultilineString(sc.Next(), &buf, true)
		}
	}
	for {
		if ch == '\n' || ch == '\r' || ch < 0 {
			break
		}
		ch = sc.Next()
	}
	return nil
}

func (sc *Scanner) scanIdent(ch int, buf *bytes.Buffer) error {
	writeChar(buf, ch)
	for isIdent(sc.Peek(), 1) {
		writeChar(buf, sc.Next())
	}
	return nil
}

func (sc *Scanner) scanDecimal(ch int, buf *bytes.Buffer) error {
	writeChar(buf, ch)
	for isDecimal(sc.Peek()) {
		writeChar(buf, sc.Next())
	}
	return nil
}

// scanNumber is a port of stock Lua's read_numeral (llex.c): consume the
// numeral greedily — xdigits and dots, with the exponent mark e/E (p/P for
// hex) each followed by an optional sign — then, if the numeral touches a
// letter, consume it to force an error, and finally validate the text.
// U1 adds hex floats (0x1p-2, 0xA.8p0) and the stock "malformed number"
// error (replacing the old silent NaN fallthrough in the compiler).
func (sc *Scanner) scanNumber(ch int, buf *bytes.Buffer) error {
	expo := "eE"
	writeChar(buf, ch)
	if ch == '0' && (sc.Peek() == 'x' || sc.Peek() == 'X') {
		writeChar(buf, sc.Next())
		expo = "pP"
	}
	for {
		if c := sc.Peek(); c != EOF && strings.IndexByte(expo, byte(c)) >= 0 {
			writeChar(buf, sc.Next())
			if c := sc.Peek(); c == '-' || c == '+' {
				writeChar(buf, sc.Next())
			}
		} else if isDigit(sc.Peek()) || sc.Peek() == '.' {
			writeChar(buf, sc.Next())
		} else {
			break
		}
	}
	if isIdent(sc.Peek(), 0) { // numeral touching a letter: force an error
		writeChar(buf, sc.Next())
	}
	if !numeralOK(buf.String()) {
		return sc.Error(buf.String(), "malformed number")
	}
	return nil
}

// numeralOK reports whether the scanned numeral text is a valid Lua number
// (decimal/hex integer, decimal float, or hex float). It mirrors the
// acceptance of C's strtod/luaO_str2num: "3." and "3.e5" are valid, hex
// floats may omit the p-exponent, and underscores are not. Note Go's
// base-0 ParseInt is NOT usable here: it would read "0777" as octal (Lua
// says 777) and accept "0b101"/"0o17" (unknown to Lua). 5.3+ acceptance
// (U2): a decimal integer that overflows int64 is a valid numeral reading
// as a float, and plain hex integers of ANY length are valid (they wrap
// as unsigned: 0xFFFFFFFFFFFFFFFF == -1). The value parsing itself lives
// in lua.parseNumber (utils.go), which applies the same rules; this copy
// exists because the parse package cannot import the lua package.
func numeralOK(s string) bool {
	isHex := len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
	if isHex {
		body := s[2:]
		if strings.IndexAny(body, ".pP") >= 0 {
			if !hasMantissaDigit(body, true) {
				return false // "0x." and friends: no digit at all
			}
			_, err := strconv.ParseFloat(normalizeNumeral(s), 64)
			return err == nil || errors.Is(err, strconv.ErrRange)
		}
		// plain hex integer: always valid, wrapping in lua.parseNumber
		if len(body) == 0 {
			return false
		}
		for i := 0; i < len(body); i++ {
			if !isHexDigit(body[i]) {
				return false
			}
		}
		return true
	}
	if strings.IndexAny(s, ".eE") >= 0 {
		if !hasMantissaDigit(s, false) {
			return false // ".e5" and friends: no digit at all
		}
		_, err := strconv.ParseFloat(normalizeNumeral(s), 64)
		// out-of-range floats are valid numerals with value ±Inf
		// (math.lua: 10e500 == math.huge)
		return err == nil || errors.Is(err, strconv.ErrRange)
	}
	if _, err := strconv.ParseInt(s, 10, 64); err == nil {
		return true
	}
	// decimal integer overflow: valid numeral, value read as a float
	_, err := strconv.ParseFloat(normalizeNumeral(s), 64)
	return err == nil || errors.Is(err, strconv.ErrRange)
}

// hasMantissaDigit is the strtod contract — at least one digit before the
// exponent ('p' for hex, 'e' for decimal; 'e' is a hex digit, not a hex
// exponent). utils.go carries the same rule as numeralHasMantissaDigit.
func hasMantissaDigit(body string, hex bool) bool {
	for i := 0; i < len(body); i++ {
		c := body[i]
		if isHexDigit(c) && (hex || (c >= '0' && c <= '9')) {
			return true
		}
		if !hex && (c == 'e' || c == 'E') {
			return false
		}
		if hex && (c == 'p' || c == 'P') {
			return false
		}
	}
	return false
}

// normalizeNumeral adapts Lua numeral syntax to Go's ParseFloat: digits
// after a trailing '.' ("3." → "3.0"), and a p-exponent on dot-bearing hex
// numerals ("0x8.8" → "0x8.8p0"). The digit test is base-aware ('e' is a
// hex digit but a decimal exponent marker). Only forms the scanner can
// produce are handled. utils.go's parseNumber carries the same
// normalizations.
func normalizeNumeral(s string) string {
	b := []byte(s)
	hex := len(s) > 2 && s[0] == '0' && (s[1] == 'x' || s[1] == 'X')
	for i := 0; i < len(b); i++ {
		if b[i] == '.' {
			next := byte(0) // the end counts as "no digit follows"
			if i+1 < len(b) {
				next = b[i+1]
			}
			if !(isHexDigit(next) && (hex || (next >= '0' && next <= '9'))) {
				b = append(b, 0)
				copy(b[i+2:], b[i+1:])
				b[i+1] = '0'
				i++ // skip the inserted zero
			}
		}
	}
	t := string(b)
	isHex := len(t) > 2 && t[0] == '0' && (t[1] == 'x' || t[1] == 'X')
	if isHex && strings.IndexAny(t, "pP") < 0 {
		t += "p0"
	}
	return t
}

func isHexDigit(c byte) bool {
	return '0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F'
}

// scanString reads a short literal string. `buf` receives the decoded
// bytes; `raw` accumulates the source text as stock Lua's scanner buffer
// holds it (delimiter + translated escapes + offending character on
// error) so that "near" tokens in error messages match stock byte-for-
// byte.
func (sc *Scanner) scanString(quote int, buf *bytes.Buffer) error {
	raw := []byte{byte(quote)}
	ch := sc.Next()
	for ch != quote {
		if ch == '\n' || ch == '\r' || ch < 0 {
			// stock: TK_EOS for end-of-input (near <eof>), TK_STRING
			// (the buffer) for a raw newline inside the literal
			if ch < 0 {
				return sc.ErrorEOF("unfinished string")
			}
			return sc.Error(string(raw), "unfinished string")
		}
		if ch == '\\' {
			raw = append(raw, '\\')
			if err := sc.scanEscape(buf, &raw); err != nil {
				return err
			}
		} else {
			writeChar(buf, ch)
			raw = append(raw, byte(ch))
		}
		ch = sc.Next()
	}
	return nil
}

// scanEscape handles the character after a backslash inside a short
// string. It is a port of read_string's escape switch (llex.c), including
// the 5.2 additions \xXX and \z, the ≤255 check on \ddd, and the
// "invalid escape sequence" error for anything else. `raw` mirrors
// stock's save/buffremove discipline so error tokens render identically.
func (sc *Scanner) scanEscape(buf *bytes.Buffer, raw *[]byte) error {
	ch := sc.Next()
	saved := func(c byte) { *raw = append(*raw, c) }
	switch ch {
	case 'a':
		buf.WriteByte('\a')
		saved('\a')
	case 'b':
		buf.WriteByte('\b')
		saved('\b')
	case 'f':
		buf.WriteByte('\f')
		saved('\f')
	case 'n':
		buf.WriteByte('\n')
		saved('\n')
	case 'r':
		buf.WriteByte('\r')
		saved('\r')
	case 't':
		buf.WriteByte('\t')
		saved('\t')
	case 'v':
		buf.WriteByte('\v')
		saved('\v')
	case '\\':
		buf.WriteByte('\\')
		saved('\\')
	case '"':
		buf.WriteByte('"')
		saved('"')
	case '\'':
		buf.WriteByte('\'')
		saved('\'')
	case '\n':
		// (Scanner.Next already normalizes "\r\n"/"\r" to '\n')
		buf.WriteByte('\n')
		saved('\n')
	case EOF:
		// stock: no_save — the enclosing loop's next read reports
		// "unfinished string" near <eof>
		return nil
	case 'x': // \xXX: exactly two hexadecimal digits (5.2)
		saved('x')
		r := 0
		prev := -1
		for i := 0; i < 2; i++ {
			c := sc.Peek()
			if c == EOF || !isHexDigit(byte(c)) {
				// gethexa saves its current before checking; esccheck
				// saves the offending character too
				if prev >= 0 {
					saved(byte(prev))
				}
				if c != EOF {
					sc.Next()
					saved(byte(c))
				}
				return sc.Error(string(*raw), "hexadecimal digit expected")
			}
			sc.Next()
			if prev >= 0 { // gethexa#2's save_and_next of the first digit
				saved(byte(prev))
			}
			prev = c
			r = (r << 4) + hexValue(byte(c))
		}
		// stock readhexaesc's buffremove(2) drops 'x' and the first
		// digit; the second digit was never saved
		*raw = (*raw)[:len(*raw)-2]
		buf.WriteByte(byte(r))
		saved(byte(r))
	case 'z': // \z: skip the following span of whitespace (5.2)
		*raw = (*raw)[:len(*raw)-1] // stock buffremove(1): drop the '\\'
		for {
			c := sc.Peek()
			if c == ' ' || c == '\t' || c == '\n' || c == '\r' || c == '\v' || c == '\f' {
				sc.Next()
			} else {
				break
			}
		}
	case 'u': // \u{XXX}: UTF-8 escape (5.3; port of utf8esc/readutf8esc)
		saved('u')
		if c := sc.Peek(); c != '{' {
			if c != EOF {
				sc.Next()
				saved(byte(c))
			}
			return sc.Error(string(*raw), "missing '{'")
		}
		saved('{')
		sc.Next() // consume '{'
		c := sc.Peek()
		if c == EOF || !isHexDigit(byte(c)) {
			if c != EOF {
				sc.Next()
				saved(byte(c))
			}
			return sc.Error(string(*raw), "hexadecimal digit expected")
		}
		r := uint32(hexValue(byte(c)))
		i := 4 // saved chars to drop: "\u{X"
		for {
			saved(byte(c)) // save_and_next of the digit folded into r
			sc.Next()
			if c = sc.Peek(); c == EOF || !isHexDigit(byte(c)) {
				break
			}
			i++
			if r > 0x7FFFFFFF>>4 {
				sc.Next()
				saved(byte(c))
				return sc.Error(string(*raw), "UTF-8 value too large")
			}
			r = r<<4 + uint32(hexValue(byte(c)))
		}
		if c != '}' {
			if c != EOF {
				sc.Next()
				saved(byte(c))
			}
			return sc.Error(string(*raw), "missing '}'")
		}
		sc.Next() // skip '}'
		*raw = (*raw)[:len(*raw)-i]
		writeUtf8Esc(buf, r)
	default:
		if '0' <= ch && ch <= '9' { // \ddd: up to three digits, ≤ 255
			ndigits := 1
			saved(byte(ch))
			r := ch - '0'
			for i := 0; i < 2 && isDecimal(sc.Peek()); i++ {
				d := sc.Next()
				saved(byte(d))
				r = 10*r + d - '0'
				ndigits++
			}
			if r > 255 {
				if c := sc.Peek(); c != EOF {
					saved(byte(c))
				}
				return sc.Error(string(*raw), "decimal escape too large")
			}
			// stock buffremove(i): drop the saved digits, keep '\' plus
			// the decoded value
			*raw = (*raw)[:len(*raw)-ndigits]
			buf.WriteByte(byte(r))
			saved(byte(r))
			return nil
		}
		saved(byte(ch))
		return sc.Error(string(*raw), "invalid escape sequence")
	}
	return nil
}

func hexValue(c byte) int {
	switch {
	case '0' <= c && c <= '9':
		return int(c - '0')
	case 'a' <= c && c <= 'f':
		return int(c-'a') + 10
	default:
		return int(c-'A') + 10
	}
}

// writeUtf8Esc ports stock luaO_utf8esc: encode x (<= 0x7FFFFFFF) in up to
// six bytes of Lua's extended UTF-8 scheme (not capped at 0x10FFFF).
func writeUtf8Esc(buf *bytes.Buffer, x uint32) {
	var b [8]byte
	n := 1
	if x < 0x80 {
		b[7] = byte(x)
	} else {
		mfb := uint32(0x3f) // maximum that fits in the first byte
		for x > mfb {       // add continuation bytes
			b[8-n] = byte(0x80 | (x & 0x3f))
			n++
			x >>= 6
			mfb >>= 1
		}
		b[8-n] = byte((^mfb << 1) | x) // add first byte
	}
	buf.Write(b[8-n : 8])
}

func (sc *Scanner) countSep(ch int) (int, int) {
	count := 0
	for ; ch == '='; count = count + 1 {
		ch = sc.Next()
	}
	return count, ch
}

// scanMultilineString reads a long-bracket string ([[...]], [=[...]=]).
// U1 re-pins its error texts to stock 5.5 (llex.c read_long_string):
// "invalid long string delimiter" and "unfinished long <string|comment>
// (starting at line N)" — comments share the scanner but report
// "comment". isComment selects the wording.
func (sc *Scanner) scanMultilineString(ch int, buf *bytes.Buffer, isComment bool) error {
	startLine := sc.Pos.Line
	var count1, count2 int
	count1, ch = sc.countSep(ch)
	if ch != '[' {
		return sc.Error(string(rune(ch)), "invalid long string delimiter")
	}
	ch = sc.Next()
	if ch == '\n' || ch == '\r' {
		ch = sc.Next()
	}
	for {
		if ch < 0 {
			what := "string"
			if isComment {
				what = "comment"
			}
			return sc.ErrorEOF(fmt.Sprintf("unfinished long %s (starting at line %d)", what, startLine))
		} else if ch == ']' {
			count2, ch = sc.countSep(sc.Next())
			if count1 == count2 && ch == ']' {
				goto finally
			}
			buf.WriteByte(']')
			buf.WriteString(strings.Repeat("=", count2))
			continue
		}
		writeChar(buf, ch)
		ch = sc.Next()
	}

finally:
	return nil
}

var reservedWords = map[string]int{
	"and": TAnd, "break": TBreak, "do": TDo, "else": TElse, "elseif": TElseIf,
	"end": TEnd, "false": TFalse, "for": TFor, "function": TFunction,
	"if": TIf, "in": TIn, "local": TLocal, "nil": TNil, "not": TNot, "or": TOr,
	"return": TReturn, "repeat": TRepeat, "then": TThen, "true": TTrue,
	"until": TUntil, "while": TWhile, "goto": TGoto}

func (sc *Scanner) Scan(lexer *Lexer) (ast.Token, error) {
redo:
	var err error
	tok := ast.Token{}
	newline := false

	ch := sc.skipWhiteSpace(whitespace1)
	if ch == '\n' || ch == '\r' {
		newline = true
		ch = sc.skipWhiteSpace(whitespace2)
	}

	if ch == '(' && lexer.PrevTokenType == ')' {
		lexer.PNewLine = newline
	} else {
		lexer.PNewLine = false
	}

	var _buf bytes.Buffer
	buf := &_buf
	tok.Pos = sc.Pos

	switch {
	case isIdent(ch, 0):
		tok.Type = TIdent
		err = sc.scanIdent(ch, buf)
		tok.Str = buf.String()
		if err != nil {
			goto finally
		}
		if typ, ok := reservedWords[tok.Str]; ok {
			tok.Type = typ
		}
	case isDecimal(ch):
		tok.Type = TNumber
		err = sc.scanNumber(ch, buf)
		tok.Str = buf.String()
	default:
		switch ch {
		case EOF:
			tok.Type = EOF
		case '-':
			if sc.Peek() == '-' {
				err = sc.skipComments(sc.Next())
				if err != nil {
					goto finally
				}
				goto redo
			} else {
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '"', '\'':
			tok.Type = TString
			err = sc.scanString(ch, buf)
			tok.Str = buf.String()
		case '[':
			if c := sc.Peek(); c == '[' || c == '=' {
				tok.Type = TString
				err = sc.scanMultilineString(sc.Next(), buf, false)
				tok.Str = buf.String()
			} else {
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '=':
			if sc.Peek() == '=' {
				tok.Type = TEqeq
				tok.Str = "=="
				sc.Next()
			} else {
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '~':
			if sc.Peek() == '=' {
				tok.Type = TNeq
				tok.Str = "~="
				sc.Next()
			} else {
				// stock 5.5 returns '~' as a single-char token (BXOR);
				// U2b gives the grammar its binary/unary uses
				tok.Type = TBXor
				tok.Str = "~"
			}
		case '<':
			switch sc.Peek() {
			case '=':
				tok.Type = TLte
				tok.Str = "<="
				sc.Next()
			case '<':
				tok.Type = TShl
				tok.Str = "<<"
				sc.Next()
			default:
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '>':
			switch sc.Peek() {
			case '=':
				tok.Type = TGte
				tok.Str = ">="
				sc.Next()
			case '>':
				tok.Type = TShr
				tok.Str = ">>"
				sc.Next()
			default:
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '.':
			ch2 := sc.Peek()
			switch {
			case isDecimal(ch2):
				tok.Type = TNumber
				err = sc.scanNumber(ch, buf)
				tok.Str = buf.String()
			case ch2 == '.':
				writeChar(buf, ch)
				writeChar(buf, sc.Next())
				if sc.Peek() == '.' {
					writeChar(buf, sc.Next())
					tok.Type = T3Comma
				} else {
					tok.Type = T2Comma
				}
				tok.Str = buf.String()
			default:
				tok.Type = '.'
				tok.Str = "."
			}
		case ':':
			if sc.Peek() == ':' {
				tok.Type = T2Colon
				tok.Str = "::"
				sc.Next()
			} else {
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '/':
			if sc.Peek() == '/' {
				tok.Type = TIDiv
				tok.Str = "//"
				sc.Next()
			} else {
				tok.Type = ch
				tok.Str = string(rune(ch))
			}
		case '&':
			tok.Type = TBand
			tok.Str = "&"
		case '|':
			tok.Type = TBor
			tok.Str = "|"
		case '+', '*', '%', '^', '#', '(', ')', '{', '}', ']', ';', ',':
			tok.Type = ch
			tok.Str = string(rune(ch))
		default:
			// stock llex has no invalid-character error: any other char
			// becomes a single-char token and the parser rejects it with
			// "unexpected symbol" (U2 adds the real bitwise operators)
			tok.Type = ch
			tok.Str = string(rune(ch))
		}
	}

finally:
	tok.Name = TokenName(int(tok.Type))
	return tok, err
}

// yacc interface {{{

type Lexer struct {
	scanner       *Scanner
	Stmts         []ast.Stmt
	PNewLine      bool
	Token         ast.Token
	PrevTokenType int
	// pushed is a one-token pushback (the `global` contextual-keyword
	// lookahead — U3a). Zero when empty.
	pushed    ast.Token
	hasPushed bool
}

func (lx *Lexer) Lex(lval *yySymType) int {
	lx.PrevTokenType = lx.Token.Type
	var tok ast.Token
	if lx.hasPushed {
		tok = lx.pushed
		lx.hasPushed = false
	} else {
		var err error
		tok, err = lx.scanner.Scan(lx)
		if err != nil {
			panic(err)
		}
		// U3a: `global` is a contextual keyword (a full reserved word at
		// U5). An identifier "global" followed by a token that can only
		// continue a global DECLARATION — a name, `function`, `<` (an
		// attribute), or `*` — tokenizes as TGlobal; anything else keeps
		// it a plain identifier (`global = 1`, `global.f()`, `global, x
		// = ...` still parse as expressions in 5.1-era corpora).
		if tok.Type == TIdent && tok.Str == "global" {
			next, err := lx.scanner.Scan(lx)
			if err != nil {
				panic(err)
			}
			lx.pushed, lx.hasPushed = next, true
			switch {
			case next.Type == TIdent, next.Type == TFunction, next.Str == "*", next.Str == "<":
				tok.Type = TGlobal
			}
		}
	}
	if tok.Type < 0 {
		// $end to goyacc — but still record the token so error paths
		// report "near <eof>" (stock's txtToken for TK_EOS)
		lx.Token = tok
		return 0
	}
	lval.token = tok
	lx.Token = tok
	return int(tok.Type)
}

// Error is goyacc's syntax-error hook (and the grammar actions' error
// path). goyacc reports "syntax error[: unexpected <tok>]" — stock Lua's
// parser says "unexpected symbol" for a token that cannot start an
// expression (primaryexp's default case) — so the message is rewritten to
// the stock wording; the near token comes from lx.Token.
func (lx *Lexer) Error(message string) {
	if strings.HasPrefix(message, "syntax error") || message == "parse error" {
		message = "unexpected symbol"
	}
	tok := lx.Token
	e := lx.scanner.TokenError(tok, message)
	if tok.Pos.Line == EOF || tok.Type == EOF {
		e.NearEOF = true
	}
	panic(e)
}

// SyntaxErrorAtNext reports stock's exprstat/assignment error class
// (lparser.c's check_condition "syntax error"): the parser has consumed
// the offending expression and errors at the token FOLLOWING it, which
// goyacc may not have scanned yet (default reductions). Scan one token
// and error there — e.g. a bare "returna" chunk yields
// "...: syntax error near <eof>", matching stock byte-for-byte.
func (lx *Lexer) SyntaxErrorAtNext() {
	// scan directly: Lex() drops the EOF token without recording it
	tok, err := lx.scanner.Scan(lx)
	if err != nil {
		panic(err)
	}
	lx.Token = tok
	e := lx.scanner.TokenError(tok, "syntax error")
	if tok.Pos.Line == EOF || tok.Type == EOF {
		e.NearEOF = true
	}
	panic(e)
}

func (lx *Lexer) TokenError(tok ast.Token, message string) {
	panic(lx.scanner.TokenError(tok, message))
}

// SemError raises a luaK_semerror-class error (U4): stock wording with no
// "near '<token>'" suffix, reported at the current line — the lexer is
// positioned on the lookahead when grammar actions run, matching stock's
// ls->linenumber at the semerror. Used for the attribute/const checks the
// stock parser raises mid-parse.
func (lx *Lexer) SemError(message string) {
	e := lx.scanner.TokenError(lx.Token, message)
	e.Sem = true
	panic(e)
}

func Parse(reader io.Reader, name string) (chunk []ast.Stmt, err error) {
	lexer := &Lexer{scanner: NewScanner(reader, name)}
	chunk = nil
	defer func() {
		if e := recover(); e != nil {
			err, _ = e.(error)
		}
	}()
	yyParse(lexer)
	chunk = lexer.Stmts
	return
}

// }}}

// Dump {{{

func isInlineDumpNode(rv reflect.Value) bool {
	switch rv.Kind() {
	case reflect.Struct, reflect.Slice, reflect.Interface, reflect.Ptr:
		return false
	default:
		return true
	}
}

func dump(node interface{}, level int, s string) string {
	rt := reflect.TypeOf(node)
	if fmt.Sprint(rt) == "<nil>" {
		return strings.Repeat(s, level) + "<nil>"
	}

	rv := reflect.ValueOf(node)
	buf := []string{}
	switch rt.Kind() {
	case reflect.Slice:
		if rv.Len() == 0 {
			return strings.Repeat(s, level) + "<empty>"
		}
		for i := 0; i < rv.Len(); i++ {
			buf = append(buf, dump(rv.Index(i).Interface(), level, s))
		}
	case reflect.Ptr:
		vt := rv.Elem()
		tt := rt.Elem()
		indicies := []int{}
		for i := 0; i < tt.NumField(); i++ {
			if strings.Index(tt.Field(i).Name, "Base") > -1 {
				continue
			}
			indicies = append(indicies, i)
		}
		switch {
		case len(indicies) == 0:
			return strings.Repeat(s, level) + "<empty>"
		case len(indicies) == 1 && isInlineDumpNode(vt.Field(indicies[0])):
			for _, i := range indicies {
				buf = append(buf, strings.Repeat(s, level)+"- Node$"+tt.Name()+": "+dump(vt.Field(i).Interface(), 0, s))
			}
		default:
			buf = append(buf, strings.Repeat(s, level)+"- Node$"+tt.Name())
			for _, i := range indicies {
				if isInlineDumpNode(vt.Field(i)) {
					inf := dump(vt.Field(i).Interface(), 0, s)
					buf = append(buf, strings.Repeat(s, level+1)+tt.Field(i).Name+": "+inf)
				} else {
					buf = append(buf, strings.Repeat(s, level+1)+tt.Field(i).Name+": ")
					buf = append(buf, dump(vt.Field(i).Interface(), level+2, s))
				}
			}
		}
	default:
		buf = append(buf, strings.Repeat(s, level)+fmt.Sprint(node))
	}
	return strings.Join(buf, "\n")
}

func Dump(chunk []ast.Stmt) string {
	return dump(chunk, 0, "   ")
}

// }}
