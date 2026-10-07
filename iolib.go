package lua

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"syscall"
)

var ioFuncs = map[string]LGFunction{
	"close":   ioClose,
	"flush":   ioFlush,
	"lines":   ioLines,
	"input":   ioInput,
	"output":  ioOutput,
	"open":    ioOpenFile,
	"popen":   ioPopen,
	"read":    ioRead,
	"type":    ioType,
	"tmpfile": ioTmpFile,
	"write":   ioWrite,
}

const lFileClass = "FILE*"

type lFile struct {
	fp     *os.File
	pp     *exec.Cmd
	writer io.Writer
	reader *bufio.Reader
	stdout io.ReadCloser
	closed bool
}

type lFileType int

const (
	lFileFile lFileType = iota
	lFileProcess
)

const fileDefOutIndex = 1
const fileDefInIndex = 2
const fileDefaultWriteBuffer = 4096
const fileDefaultReadBuffer = 4096

// checkFile is stock tofile's luaL_checkudata(L, 1, LUA_FILEHANDLE): the
// FILE* type name in the argcheck texts ("FILE* expected, got ..."), not
// the generic userdata type (errors.lua:368 pokes it through __gc).
func checkFile(L *LState) *lFile {
	if ud, ok := L.Get(1).(*LUserData); ok {
		if file, ok := ud.Value.(*lFile); ok {
			return file
		}
	}
	L.typeErrorStr(1, lFileClass)
	return nil
}

func errorIfFileIsClosed(L *LState, file *lFile) {
	if file.closed {
		L.ArgError(1, "file is closed")
	}
}

func newFile(L *LState, file *os.File, path string, flag int, perm os.FileMode, writable, readable bool) (*LUserData, error) {
	ud := L.NewUserData()
	var err error
	if file == nil {
		file, err = os.OpenFile(path, flag, perm)
		if err != nil {
			return nil, err
		}
	}
	lfile := &lFile{fp: file, pp: nil, writer: nil, reader: nil, stdout: nil, closed: false}
	ud.Value = lfile
	if writable {
		lfile.writer = file
	}
	if readable {
		lfile.reader = bufio.NewReaderSize(file, fileDefaultReadBuffer)
	}
	L.SetMetatable(ud, L.GetTypeMetatable(lFileClass))
	return ud, nil
}

func newProcess(L *LState, cmd string, writable, readable bool) (*LUserData, error) {
	ud := L.NewUserData()
	c, args := popenArgs(cmd)
	pp := exec.Command(c, args...)
	lfile := &lFile{fp: nil, pp: pp, writer: nil, reader: nil, stdout: nil, closed: false}
	ud.Value = lfile

	var err error
	if writable {
		lfile.writer, err = pp.StdinPipe()
	}
	if readable {
		lfile.stdout, err = pp.StdoutPipe()
		lfile.reader = bufio.NewReaderSize(lfile.stdout, fileDefaultReadBuffer)
	}
	if err != nil {
		return nil, err
	}
	err = pp.Start()
	if err != nil {
		return nil, err
	}

	L.SetMetatable(ud, L.GetTypeMetatable(lFileClass))
	return ud, nil
}

func (file *lFile) Type() lFileType {
	if file.fp == nil {
		return lFileProcess
	}
	return lFileFile
}

func (file *lFile) Name() string {
	switch file.Type() {
	case lFileFile:
		return fmt.Sprintf("file %s", file.fp.Name())
	case lFileProcess:
		return fmt.Sprintf("process %s", file.pp.Path)
	}
	return ""
}

func (file *lFile) AbandonReadBuffer() error {
	if file.Type() == lFileFile && file.reader != nil {
		_, err := file.fp.Seek(-int64(file.reader.Buffered()), 1)
		if err != nil {
			return err
		}
		file.reader = bufio.NewReaderSize(file.fp, fileDefaultReadBuffer)
	}
	return nil
}

func fileDefOut(L *LState) *LUserData {
	return L.Get(UpvalueIndex(1)).(*LTable).RawGetInt(fileDefOutIndex).(*LUserData)
}

func fileDefIn(L *LState) *LUserData {
	return L.Get(UpvalueIndex(1)).(*LTable).RawGetInt(fileDefInIndex).(*LUserData)
}

func fileIsWritable(L *LState, file *lFile) int {
	if file.writer == nil {
		L.Push(LNil)
		L.Push(LString(fmt.Sprintf("%s is opened for only reading.", file.Name())))
		L.Push(LNumber(1)) // C-Lua compatibility: Original Lua pushes errno to the stack
		return 3
	}
	return 0
}

func fileIsReadable(L *LState, file *lFile) int {
	if file.reader == nil {
		L.Push(LNil)
		L.Push(LString(fmt.Sprintf("%s is opened for only writing.", file.Name())))
		L.Push(LNumber(1)) // C-Lua compatibility: Original Lua pushes errno to the stack
		return 3
	}
	return 0
}

var stdFiles = []struct {
	name     string
	file     *os.File
	writable bool
	readable bool
}{
	{"stdout", os.Stdout, true, false},
	{"stdin", os.Stdin, false, true},
	{"stderr", os.Stderr, true, false},
}

func OpenIo(L *LState) int {
	mod := L.RegisterModule(IoLibName, map[string]LGFunction{}).(*LTable)
	mt := L.NewTypeMetatable(lFileClass)
	mt.RawSetString("__index", mt)
	// stock luaL_newmetatable(LUA_FILEHANDLE): the class metatable
	// carries __name = "FILE*", which error texts render as the type
	mt.RawSetString("__name", LString(lFileClass))
	L.SetFuncs(mt, fileMethods)
	mt.RawSetString("lines", L.NewClosure(fileLines, L.NewFunction(fileLinesIter)))

	for _, finfo := range stdFiles {
		file, _ := newFile(L, finfo.file, "", 0, os.FileMode(0), finfo.writable, finfo.readable)
		mod.RawSetString(finfo.name, file)
	}
	uv := L.CreateTable(2, 0)
	uv.RawSetInt(fileDefOutIndex, mod.RawGetString("stdout"))
	uv.RawSetInt(fileDefInIndex, mod.RawGetString("stdin"))
	for name, fn := range ioFuncs {
		mod.RawSetString(name, L.NewClosure(fn, uv))
	}
	mod.RawSetString("lines", L.NewClosure(ioLines, uv, L.NewClosure(ioLinesIter, uv)))
	// Modifications are being made in-place rather than returned?
	L.Push(mod)
	return 1
}

var fileMethods = map[string]LGFunction{
	"__tostring": fileToString,
	// stock's metameth table maps the file __close AND __gc to f_gc, NOT
	// io_close: a no-op on an already-closed handle. io.lines(filename)
	// returns the file as the generic-for's 4th to-be-closed value (U5a —
	// TFORPREP registers it), and the lines iterator already closed it at
	// EOF by the time the loop-exit close fires on the same handle.
	"__close": fileCloseMeta,
	"__gc":    fileCloseMeta,
	"write":   fileWrite,
	"close":   fileClose,
	"flush":   fileFlush,
	"lines":   fileLines,
	"read":    fileRead,
	"seek":    fileSeek,
	"setvbuf": fileSetVBuf,
}

func fileToString(L *LState) int {
	file := checkFile(L)
	if file.Type() == lFileFile {
		if file.closed {
			L.Push(LString("file (closed)"))
		} else {
			L.Push(LString("file"))
		}
	} else {
		if file.closed {
			L.Push(LString("process (closed)"))
		} else {
			L.Push(LString("process"))
		}
	}
	return 1
}

func fileWriteAux(L *LState, file *lFile, idx int, handle LValue) int {
	if n := fileIsWritable(L, file); n != 0 {
		return n
	}
	errorIfFileIsClosed(L, file)
	top := L.GetTop()
	out := file.writer
	var err error
	for i := idx; i <= top; i++ {
		L.CheckTypes(i, LTNumber, LTString)
		s := LVAsString(L.Get(i))
		if _, err = out.Write(unsafeFastStringToReadOnlyBytes(s)); err != nil {
			goto errreturn
		}
	}

	file.AbandonReadBuffer()
	// 5.2: file:write returns the file itself (was true)
	L.Push(handle)
	return 1
errreturn:

	file.AbandonReadBuffer()
	L.Push(LNil)
	L.Push(LString(err.Error()))
	L.Push(LNumber(1)) // C-Lua compatibility: Original Lua pushes errno to the stack
	return 3
}

func fileCloseAux(L *LState, file *lFile) int {
	file.closed = true
	var err error
	if file.writer != nil {
		if bwriter, ok := file.writer.(*bufio.Writer); ok {
			if err = bwriter.Flush(); err != nil {
				goto errreturn
			}
		}
	}
	file.AbandonReadBuffer()

	switch file.Type() {
	case lFileFile:
		if err = file.fp.Close(); err != nil {
			goto errreturn
		}
		L.Push(LTrue)
		return 1
	case lFileProcess:
		if file.stdout != nil {
			file.stdout.Close() // ignore errors
		}
		err = file.pp.Wait()
		var exitStatus int // Initialised to zero value = 0
		if err != nil {
			if e2, ok := err.(*exec.ExitError); ok {
				if s, ok := e2.Sys().(syscall.WaitStatus); ok {
					exitStatus = s.ExitStatus()
				} else {
					err = errors.New("Unimplemented for system where exec.ExitError.Sys() is not syscall.WaitStatus.")
				}
			}
		} else {
			exitStatus = 0
		}
		L.Push(LNumber(exitStatus))
		return 1
	}

errreturn:
	L.RaiseError("%s", err.Error())
	return 0
}

func fileFlushAux(L *LState, file *lFile) int {
	if n := fileIsWritable(L, file); n != 0 {
		return n
	}
	errorIfFileIsClosed(L, file)

	if bwriter, ok := file.writer.(*bufio.Writer); ok {
		if err := bwriter.Flush(); err != nil {
			L.Push(LNil)
			L.Push(LString(err.Error()))
			return 2
		}
	}
	L.Push(LTrue)
	return 1
}

// readNumberFile ports liolib.c read_number: scan a maximal valid
// numeral prefix (spaces, sign, hex, decimal point, exponent; the C
// accepts both the locale decimal point and '.'), convert through the
// full numeral grammar so the SUBTYPE is preserved ("3" → integer,
// "3.5" → float); anything else fails to nil.
func readNumberFile(L *LState, file *lFile) bool {
	rd := file.reader
	const maxLenNum = 200
	var buff []byte
	c := -1
	// skip spaces
	for {
		b, err := rd.ReadByte()
		if err != nil {
			c = -1
			break
		}
		if b == ' ' || b == '\t' || b == '\n' || b == '\v' || b == '\f' || b == '\r' {
			continue
		}
		c = int(b)
		break
	}
	nextc := func() bool {
		if len(buff) >= maxLenNum {
			return false
		}
		buff = append(buff, byte(c))
		b, err := rd.ReadByte()
		c = -1
		if err == nil {
			c = int(b)
		}
		return true
	}
	test2 := func(set string) bool {
		if c == int(set[0]) || c == int(set[1]) {
			return nextc()
		}
		return false
	}
	isDigit := func() bool { return c >= '0' && c <= '9' }
	isXdigit := func() bool {
		return isDigit() || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
	}
	readdigits := func(hex bool) int {
		count := 0
		for (hex && isXdigit() || !hex && isDigit()) && nextc() {
			count++
		}
		return count
	}
	count := 0
	hex := false
	test2("-+")
	if test2("00") {
		if test2("xX") {
			hex = true
		} else {
			count = 1
		}
	}
	count += readdigits(hex)
	if test2("..") { // matches a single '.' (set of 2)
		count += readdigits(hex)
	}
	if count > 0 && test2(func() string {
		if hex {
			return "pP"
		}
		return "eE"
	}()) {
		test2("-+")
		readdigits(false)
	}
	// unread the look-ahead byte
	if c >= 0 {
		rd.UnreadByte()
	}
	if v, err := parseNumber(string(buff)); err == nil {
		L.Push(v)
		return true
	}
	L.Push(LNil)
	return false
}

// fileReadAux ports g_read: each argument is one option — an integer
// count (0 → EOF probe returning "" or nil), or a string option with an
// optional '*' prefix: n (number, subtype-preserving) / l / L / a; no
// arguments defaults to "l". Reading stops at the first failure.
func fileReadAux(L *LState, file *lFile, idx int) int {
	if n := fileIsReadable(L, file); n != 0 {
		return n
	}
	errorIfFileIsClosed(L, file)
	base := L.GetTop()
	if base == idx-1 { // no options: default "l"
		buf, err, iseof := readBufioLine(file.reader)
		if iseof {
			L.Push(LNil)
			return 1
		}
		if err != nil {
			L.Push(LNil)
			L.Push(LString(err.Error()))
			L.Push(LNumber(1))
			return 3
		}
		L.Push(LString(string(buf)))
		return 1
	}
	nresults := 0
	for i := idx; i <= base; i++ {
		var success bool
		switch lv := L.Get(i).(type) {
		case LInteger, LNumber:
			size := checkInteger(L, i)
			if size == 0 {
				// test_eof: "" iff not at EOF
				if _, err := file.reader.ReadByte(); err != nil {
					L.Push(LNil)
					nresults++
					return nresults
				}
				file.reader.UnreadByte()
				L.Push(emptyLString)
				success = true
			} else {
				var buf []byte
				var iseof bool
				var err error
				buf, err, iseof = readBufioSize(file.reader, size)
				if iseof || err == io.EOF {
					L.Push(LNil)
					nresults++
					return nresults
				}
				if err != nil {
					L.Push(LNil)
					L.Push(LString(err.Error()))
					L.Push(LNumber(1))
					return nresults + 3
				}
				L.Push(LString(string(buf)))
				success = true
			}
		case LString:
			options := string(lv)
			if len(options) > 0 && options[0] == '*' {
				options = options[1:]
			}
			if len(options) == 0 {
				L.ArgError(i, "invalid format")
			}
			switch options[0] {
			case 'n':
				success = readNumberFile(L, file)
				if !success {
					nresults++
					return nresults
				}
			case 'l':
				buf, err, iseof := readBufioLine(file.reader)
				if iseof {
					L.Push(LNil)
					nresults++
					return nresults
				}
				if err != nil && err != io.EOF {
					L.Push(LNil)
					L.Push(LString(err.Error()))
					L.Push(LNumber(1))
					return nresults + 3
				}
				L.Push(LString(string(buf)))
				success = true
			case 'L': // line with the end-of-line kept
				buf, _, iseof, err := readLineKeep(file.reader)
				if iseof {
					L.Push(LNil)
					nresults++
					return nresults
				}
				if err != nil && err != io.EOF {
					L.Push(LNil)
					L.Push(LString(err.Error()))
					L.Push(LNumber(1))
					return nresults + 3
				}
				L.Push(LString(string(buf)))
				success = true
			case 'a':
				buf, err := io.ReadAll(file.reader)
				if err != nil && err != io.EOF {
					L.Push(LNil)
					L.Push(LString(err.Error()))
					L.Push(LNumber(1))
					return nresults + 3
				}
				L.Push(LString(string(buf)))
				success = true // 'a' always succeeds
			default:
				L.ArgError(i, "invalid format")
			}
		default:
			L.TypeError(i, LTString)
		}
		nresults++
	}
	return nresults
}

var fileSeekOptions = []string{"set", "cur", "end"}

func fileSeek(L *LState) int {
	file := checkFile(L)
	if file.Type() != lFileFile {
		L.Push(LNil)
		L.Push(LString("can not seek a process."))
		return 2
	}

	top := L.GetTop()
	if top == 1 {
		L.Push(LString("cur"))
		L.Push(LNumber(0))
	} else if top == 2 {
		L.Push(LNumber(0))
	}

	var pos int64
	var err error

	err = file.AbandonReadBuffer()
	if err != nil {
		goto errreturn
	}

	pos, err = file.fp.Seek(L.CheckInt64(3), L.CheckOption(2, fileSeekOptions))
	if err != nil {
		goto errreturn
	}

	L.Push(LNumber(pos))
	return 1

errreturn:
	L.Push(LNil)
	L.Push(LString(err.Error()))
	return 2
}

// readLineKeep reads one line keeping the trailing newline when present
// (the 5.2 *L option). A final partial line without a newline is returned
// as-is (stock does not synthesize one).
func readLineKeep(reader *bufio.Reader) (line []byte, hadNL, iseof bool, err error) {
	s, e := reader.ReadString('\n')
	if len(s) > 0 {
		return []byte(s), e == nil, false, nil
	}
	if e == io.EOF {
		return nil, false, true, nil
	}
	return nil, false, false, e
}

func fileWrite(L *LState) int {
	handle := L.CheckUserData(1)
	return fileWriteAux(L, checkFile(L), 2, handle)
}

func fileClose(L *LState) int {
	return fileCloseAux(L, checkFile(L))
}

// fileCloseMeta is the file metatable's __close (stock f_gc used as
// __close): close unless already closed — the lines iterator closes at
// EOF, then the generic-for's loop-exit close fires on the same handle.
func fileCloseMeta(L *LState) int {
	file := checkFile(L)
	if file.closed {
		return 0
	}
	return fileCloseAux(L, file)
}

func fileFlush(L *LState) int {
	return fileFlushAux(L, checkFile(L))
}

// fileLinesIter: the shared 5.4 lines iterator. Upvalues: (1) iterbase,
// (2) the file userdata, (3) format count, (4) toclose flag, (5..) the
// saved read formats. Each call runs g_read over the formats; at EOF the
// generator closes the file when it owns it.
func fileLinesIter(L *LState) int {
	ud, _ := L.Get(UpvalueIndex(2)).(*LUserData)
	file := ud.Value.(*lFile)
	errorIfFileIsClosed(L, file)
	nformats := int(L.ToInt64(UpvalueIndex(3)))
	L.SetTop(1)
	for i := 0; i < nformats; i++ {
		L.Push(L.Get(UpvalueIndex(5 + i)))
	}
	n := fileReadAux(L, file, 2)
	if v := L.Get(L.GetTop() - n + 1); v != LNil && v.Type() != LTNil {
		return n
	}
	// EOF (first result nil): close if the generator owns the file
	if LVAsBool(L.Get(UpvalueIndex(4))) {
		L.SetTop(0)
		fileCloseAux(L, file)
	}
	return 0
}

func fileLines(L *LState) int {
	file := checkFile(L)
	ud := L.CheckUserData(1)
	if n := fileIsReadable(L, file); n != 0 {
		return 0
	}
	nformats := L.GetTop() - 1
	upvalues := []LValue{L.Get(UpvalueIndex(1)), ud, LInteger(nformats), LFalse}
	for i := 0; i < nformats; i++ {
		upvalues = append(upvalues, L.Get(2+i))
	}
	L.Push(L.NewClosure(fileLinesIter, upvalues...))
	return 1
}

func fileRead(L *LState) int {
	return fileReadAux(L, checkFile(L), 2)
}

var filebufOptions = []string{"no", "full"}

func fileSetVBuf(L *LState) int {
	var err error
	var writer io.Writer
	file := checkFile(L)
	if n := fileIsWritable(L, file); n != 0 {
		return n
	}
	switch filebufOptions[L.CheckOption(2, filebufOptions)] {
	case "no":
		switch file.Type() {
		case lFileFile:
			file.writer = file.fp
		case lFileProcess:
			file.writer, err = file.pp.StdinPipe()
			if err != nil {
				goto errreturn
			}
		}
	case "full", "line": // TODO line buffer not supported
		bufsize := L.OptInt(3, fileDefaultWriteBuffer)
		switch file.Type() {
		case lFileFile:
			file.writer = bufio.NewWriterSize(file.fp, bufsize)
		case lFileProcess:
			writer, err = file.pp.StdinPipe()
			if err != nil {
				goto errreturn
			}
			file.writer = bufio.NewWriterSize(writer, bufsize)
		}
	}
	L.Push(LTrue)
	return 1
errreturn:
	L.Push(LNil)
	L.Push(LString(err.Error()))
	return 2
}

func ioInput(L *LState) int {
	if L.GetTop() == 0 {
		L.Push(fileDefIn(L))
		return 1
	}
	switch lv := L.Get(1).(type) {
	case LString:
		file, err := newFile(L, nil, string(lv), os.O_RDONLY, 0600, false, true)
		if err != nil {
			L.RaiseError("%s", err.Error())
		}
		L.Get(UpvalueIndex(1)).(*LTable).RawSetInt(fileDefInIndex, file)
		L.Push(file)
		return 1
	case *LUserData:
		if _, ok := lv.Value.(*lFile); ok {
			L.Get(UpvalueIndex(1)).(*LTable).RawSetInt(fileDefInIndex, lv)
			L.Push(lv)
			return 1
		}

	}
	// stock g_iofile: a non-string, non-nil argument goes through
	// tofile → luaL_checkudata(L, 1, "FILE*")
	L.typeErrorStr(1, lFileClass)
	return 0
}

func ioClose(L *LState) int {
	if L.GetTop() == 0 {
		return fileCloseAux(L, fileDefOut(L).Value.(*lFile))
	}
	return fileClose(L)
}

func ioFlush(L *LState) int {
	return fileFlushAux(L, fileDefOut(L).Value.(*lFile))
}

func ioLinesIter(L *LState) int {
	return fileLinesIter(L)
}

// ioLines ports 5.4 io.lines: with a filename it opens the file and
// returns FOUR values (iterator, state, control, the file — the
// to-be-closed variable of the generic for); with no argument (or the
// default input) it returns just the iterator; the remaining arguments
// are read formats forwarded to each iteration.
func ioLines(L *LState) int {
	var ud *LUserData
	toclose := false
	if v := L.Get(1); v == LNil || v.Type() == LTNil {
		ud = fileDefIn(L)
	} else {
		path := L.CheckString(1)
		file, err := newFile(L, nil, path, os.O_RDONLY, os.FileMode(0600), false, true)
		if err != nil {
			L.RaiseError("%s: No such file or directory", path)
		}
		ud = file
		toclose = true
	}
	nformats := L.GetTop() - 1
	if nformats < 0 {
		nformats = 0
	}
	upvalues := []LValue{L.Get(UpvalueIndex(1)), ud, LInteger(nformats), LBool(toclose)}
	for i := 0; i < nformats; i++ {
		upvalues = append(upvalues, L.Get(2+i))
	}
	_ = nformats
	L.Push(L.NewClosure(ioLinesIter, upvalues...))
	if toclose {
		L.Push(LNil) // state
		L.Push(LNil) // control
		L.Push(ud)   // the file as the to-be-closed variable
		return 4
	}
	return 1
}

var ioOpenOpions = []string{"r", "rb", "w", "wb", "a", "ab", "r+", "rb+", "w+", "wb+", "a+", "ab+"}

func ioOpenFile(L *LState) int {
	path := L.CheckString(1)
	if L.GetTop() == 1 {
		L.Push(LString("r"))
	}
	mode := os.O_RDONLY
	perm := 0600
	writable := true
	readable := true
	switch ioOpenOpions[L.CheckOption(2, ioOpenOpions)] {
	case "r", "rb":
		mode = os.O_RDONLY
		writable = false
	case "w", "wb":
		mode = os.O_WRONLY | os.O_TRUNC | os.O_CREATE
		readable = false
	case "a", "ab":
		mode = os.O_WRONLY | os.O_APPEND | os.O_CREATE
	case "r+", "rb+":
		mode = os.O_RDWR
	case "w+", "wb+":
		mode = os.O_RDWR | os.O_TRUNC | os.O_CREATE
	case "a+", "ab+":
		mode = os.O_APPEND | os.O_RDWR | os.O_CREATE
	}
	file, err := newFile(L, nil, path, mode, os.FileMode(perm), writable, readable)
	if err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		L.Push(LNumber(1)) // C-Lua compatibility: Original Lua pushes errno to the stack
		return 3
	}
	L.Push(file)
	return 1

}

var ioPopenOptions = []string{"r", "w"}

func ioPopen(L *LState) int {
	cmd := L.CheckString(1)
	if L.GetTop() == 1 {
		L.Push(LString("r"))
	} else if L.GetTop() > 1 && (L.Get(2)).Type() == LTNil {
		L.SetTop(1)
		L.Push(LString("r"))
	}
	var file *LUserData
	var err error

	switch ioPopenOptions[L.CheckOption(2, ioPopenOptions)] {
	case "r":
		file, err = newProcess(L, cmd, false, true)
	case "w":
		file, err = newProcess(L, cmd, true, false)
	}
	if err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	}
	L.Push(file)
	return 1
}

func ioRead(L *LState) int {
	return fileReadAux(L, fileDefIn(L).Value.(*lFile), 1)
}

func ioType(L *LState) int {
	ud, udok := L.Get(1).(*LUserData)
	if !udok {
		L.Push(LNil)
		return 1
	}
	file, ok := ud.Value.(*lFile)
	if !ok {
		L.Push(LNil)
		return 1
	}
	if file.closed {
		L.Push(LString("closed file"))
		return 1
	}
	L.Push(LString("file"))
	return 1
}

func ioTmpFile(L *LState) int {
	file, err := os.CreateTemp("", "")
	if err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	}
	L.G.tempFiles = append(L.G.tempFiles, file)
	ud, _ := newFile(L, file, "", 0, os.FileMode(0), true, true)
	L.Push(ud)
	return 1
}

func ioOutput(L *LState) int {
	if L.GetTop() == 0 {
		L.Push(fileDefOut(L))
		return 1
	}
	switch lv := L.Get(1).(type) {
	case LString:
		file, err := newFile(L, nil, string(lv), os.O_WRONLY|os.O_CREATE, 0600, true, false)
		if err != nil {
			L.RaiseError("%s", err.Error())
		}
		L.Get(UpvalueIndex(1)).(*LTable).RawSetInt(fileDefOutIndex, file)
		L.Push(file)
		return 1
	case *LUserData:
		if _, ok := lv.Value.(*lFile); ok {
			L.Get(UpvalueIndex(1)).(*LTable).RawSetInt(fileDefOutIndex, lv)
			L.Push(lv)
			return 1
		}

	}
	// stock g_iofile: a non-string, non-nil argument goes through
	// tofile → luaL_checkudata(L, 1, "FILE*")
	L.typeErrorStr(1, lFileClass)
	return 0
}

func ioWrite(L *LState) int {
	// 5.2: io.write returns the default output file
	return fileWriteAux(L, fileDefOut(L).Value.(*lFile), 1, fileDefOut(L))
}

//
