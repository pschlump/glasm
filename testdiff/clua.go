package testdiff

// The C-Lua-wasm oracle engine (design doc §7, M2): stock Lua 5.1.5
// compiled to runtime/lua51.wasm (wasi-sdk + Binaryen asyncify for
// setjmp/longjmp), driven from wazero. Values cross as the typed protocol
// of runtime/luawasm.c and are formatted by the same Go normalizers the
// interp engine uses, so both engines produce comparable event logs.
//
// The wasm blob is embedded; rebuild it with runtime/build.sh.

import (
	_ "embed"
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"os"
	"path/filepath"
	"strconv"
	"unsafe"

	wt "github.com/bytecodealliance/wasmtime-go/v48"
)

//go:embed lua51_sjlj.wasm
var lua51SjljWasm []byte

// lua51_prod.wasm — the M6c production flavor of the same blob: zero
// wasi_snapshot_preview1 imports (host.* only), sandbox-ready libs
// (base/table/string/math), rt_sandbox exported. The M6c gates
// (m6c_test.go) parse this artifact's own bytes as a build gate.
//
//go:embed lua51_prod.wasm
var lua51ProdWasm []byte

// lua55_prod.wasm — U6d (ledger row 95): the 5.5 production flavor of
// the rt blob. Zero wasi imports (host.{event,wasm_dispatch} only); the
// library surface is compiled down to the §4.3 allowlist (linit55.c —
// base minus load/dofile/loadfile/warn, coroutine, string, table,
// math, utf8) so rt_sandbox(1) finds all nine §4.5 names absent and
// returns 0. The U6d gates (m6c/m6cdet/m6d prod legs) parse and run
// this artifact; host/ embeds its own copy (build55.sh keeps them
// identical — the artifact test's cmp is the drift lock).
//
//go:embed lua55_prod.wasm
var lua55ProdWasm []byte

// value-protocol tags (must match runtime/luawasm.c)
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
)

type cval struct {
	tag  byte
	num  float64
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
	case ptString:
		n := p.u32()
		s := string(p.b[p.off : p.off+int(n)])
		p.off += int(n)
		return cval{tag: t, str: s}, nil
	}
	return cval{}, fmt.Errorf("clua: bad value tag %d at %d", t, p.off-1)
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

// CLua is the stock-C-Lua-in-wasm oracle engine.
type CLua struct {
	name string
}

// NewCLua returns a C-Lua oracle engine with the given display name.
func NewCLua(name string) *CLua { return &CLua{name: name} }

func (e *CLua) Name() string { return e.name }

func (e *CLua) Run(c Case) []string {
	var log []string
	emit := func(event, payload string) {
		log = append(log, event+"\t"+payload)
	}

	wd, _ := os.Getwd()
	if err := os.Chdir(c.Dir); err != nil {
		return []string{fmt.Sprintf("ENGINE-ERROR\tchdir %s: %v", c.Dir, err)}
	}
	defer os.Chdir(wd)
	defer func() {
		matches, _ := filepath.Glob(c.Dir + "/testdiff.tmp*")
		for _, m := range matches {
			os.Remove(m)
		}
	}()

	// Case.Args: the C driver's ldostring installs arg = {[0]=cn} directly
	// before the pcall — no host-side window to add entries. Skip with a
	// reason rather than diverge (§8.9 ledger discipline); no corpus case
	// sets Args (it is the standalone runners' surface).
	if len(c.Args) > 0 {
		return []string{"SKIP-UNSUPPORTED\tclua engine does not support Case.Args"}
	}

	// The oracle runs on wasmtime because the C Lua runtime's error
	// handling needs setjmp/longjmp, which requires the wasm EH proposal
	// (lua51_sjlj.wasm, -mllvm -wasm-enable-sjlj). wazero implements only
	// finalized core wasm. Production never runs stock C Lua — the M4+
	// backend uses error-flag propagation by design (plan §4.4).
	cfg := wt.NewConfig()
	cfg.SetWasmExceptions(true) // legacy EH: required by -mllvm -wasm-enable-sjlj setjmp
	engine := wt.NewEngineWithConfig(cfg)
	store := wt.NewStore(engine)
	module, err := wt.NewModule(engine, lua51SjljWasm)
	if err != nil {
		return []string{"ENGINE-ERROR\tmodule: " + err.Error()}
	}
	linker := wt.NewLinker(engine)
	if err := linker.DefineWasi(); err != nil {
		return []string{"ENGINE-ERROR\twasi: " + err.Error()}
	}

	rng := rand.New(rand.NewSource(42))

	// guest memory, captured by the host shims; set right after
	// instantiation — the shims can only be called by this module.
	var guestMem *wt.Memory

	memRead := func(ptr, length int32) []byte {
		if guestMem == nil {
			return nil
		}
		base := guestMem.Data(store)
		size := guestMem.DataSize(store)
		if uintptr(ptr)+uintptr(length) > size {
			return nil
		}
		return unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base)+uintptr(ptr))), length)
	}

	if err := linker.DefineFunc(store, "host", "event",
		func(kind, ptr, length int32) {
			if raw := memRead(ptr, length); raw != nil && length > 0 {
				e.onEvent(raw, kind, emit)
			}
		}); err != nil {
		return []string{"ENGINE-ERROR\thost event: " + err.Error()}
	}
	if err := linker.DefineFunc(store, "host", "random01",
		func() float64 { return rng.Float64() }); err != nil {
		return []string{"ENGINE-ERROR\thost random01: " + err.Error()}
	}
	if err := linker.DefineFunc(store, "host", "randomint",
		func(lo, hi int32) int32 { return int32(rng.Intn(int(hi-lo+1)) + int(lo)) }); err != nil {
		return []string{"ENGINE-ERROR\thost randomint: " + err.Error()}
	}
	if err := linker.DefineFunc(store, "host", "randomseed",
		func(seed int64) { rng = rand.New(rand.NewSource(seed)) }); err != nil {
		return []string{"ENGINE-ERROR\thost randomseed: " + err.Error()}
	}
	// ABI v3 reverse seam (M5a): the oracle never registers wasm protos,
	// so a refusing stub satisfies the import (the adapter re-raises -3)
	if err := linker.DefineFunc(store, "host", "wasm_dispatch",
		func(idx, frame, cl, nargs, want int32) int32 { return -3 }); err != nil {
		return []string{"ENGINE-ERROR\thost wasm_dispatch: " + err.Error()}
	}
	// M7a: the hostfn seam is a host-package feature — refuse with the
	// staged error value (the oracle never registers host functions).
	if err := linker.DefineFunc(store, "host", "host_call",
		func(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 {
			if dst := memRead(retPtr, int32(len(wireErrHostFns))); dst != nil {
				copy(dst, wireErrHostFns)
			}
			return -1
		}); err != nil {
		return []string{"ENGINE-ERROR\thost host_call: " + err.Error()}
	}

	wasi := wt.NewWasiConfig()
	if err := wasi.PreopenDir(c.Dir, "/", true); err != nil {
		return []string{"ENGINE-ERROR\tpreopen: " + err.Error()}
	}
	stdoutFile, err := os.CreateTemp("", "clua-stdout-*")
	if err != nil {
		return []string{"ENGINE-ERROR\tstdout tmp: " + err.Error()}
	}
	stdoutFile.Close()
	defer os.Remove(stdoutFile.Name())
	if err := wasi.SetStdoutFile(stdoutFile.Name()); err != nil {
		return []string{"ENGINE-ERROR\tstdout: " + err.Error()}
	}
	store.SetWasi(wasi)

	inst, err := linker.Instantiate(store, module)
	if err != nil {
		return []string{"ENGINE-ERROR\tinstantiate: " + err.Error()}
	}
	if init := inst.GetFunc(store, "_initialize"); init != nil {
		if _, err := init.Call(store); err != nil {
			return []string{"ENGINE-ERROR\tinitialize: " + err.Error()}
		}
	}
	guestMem = inst.GetExport(store, "memory").Memory()

	call := func(fn string, args ...interface{}) (uint64, error) {
		f := inst.GetFunc(store, fn)
		if f == nil {
			return 0, fmt.Errorf("clua: missing export %s", fn)
		}
		// the driver exports are all i32; wasmtime-go does not narrow
		for i, a := range args {
			switch v := a.(type) {
			case uint64:
				args[i] = int32(v)
			case int:
				args[i] = int32(v)
			}
		}
		res, err := f.Call(store, args...)
		if err != nil {
			return 0, err
		}
		switch v := res.(type) {
		case nil:
			return 0, fmt.Errorf("clua: %s returned no result", fn)
		case int32:
			return uint64(uint32(v)), nil
		case int64:
			return uint64(v), nil
		default:
			return 0, fmt.Errorf("clua: %s returned %T", fn, res)
		}
	}
	writeMem := func(ptr uint64, b []byte) bool {
		if guestMem == nil {
			return false
		}
		base := guestMem.Data(store)
		size := guestMem.DataSize(store)
		if uintptr(ptr)+uintptr(len(b)) > size {
			return false
		}
		copy(unsafe.Slice((*byte)(unsafe.Pointer(uintptr(base)+uintptr(ptr))), len(b)), b)
		return true
	}

	inAddr, err := call("linbuf")
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	nameAddr, err := call("lnamebuf")
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	name := c.Name
	if len(name) > 500 {
		name = name[:500]
	}
	if !writeMem(nameAddr, []byte(name)) || !writeMem(inAddr, c.Source) {
		return []string{"ENGINE-ERROR\tstaging write failed"}
	}

	L, err := call("lnewstate")
	if err != nil {
		return []string{"ENGINE-ERROR\t" + err.Error()}
	}
	status, err := call("ldostring", L, inAddr, uint64(len(c.Source)), nameAddr, 1)
	if err != nil {
		// A trap is always a bug in the runtime port (§8.7), reported as such.
		return append(log, fmt.Sprintf("ENGINE-ERROR\ttrap: %v", err))
	}
	if status != 0 {
		errLen, _ := call("lerrlen")
		if errLen > 0 {
			if _, err := call("lerrcopy", inAddr, errLen); err == nil {
				emit("ERROR", strconv.Quote(string(memRead(int32(inAddr), int32(errLen)))))
			}
		} else {
			emit("ERROR", `""`)
		}
	}
	// Ledger row 29: lclose returns without libc exit(), so wasi-libc's
	// atexit stdout flush never runs and io.write's buffered tail is
	// lost. Best-effort io.flush() in the live state (before lclose)
	// drains it; failures (a script that closed io.stdout) are ignored.
	if inAddr, err := call("linbuf"); err == nil {
		if nameAddr, err := call("lnamebuf"); err == nil {
			src := []byte("io.flush()")
			if writeMem(inAddr, src) && writeMem(nameAddr, []byte("=flush")) {
				_, _ = call("ldostring", L, inAddr, uint64(len(src)), nameAddr, 0)
			}
		}
	}
	call("lclose", L)
	if out, err := os.ReadFile(stdoutFile.Name()); err == nil && len(out) > 0 {
		// WASI stdout (io.write, C-level prints). Ledger row 3: the interp
		// engine does not capture io.write, so cross-engine diffs exclude it.
		for i := 0; i < len(out); {
			j := indexByte(out[i:], '\n')
			if j < 0 {
				emit("STDOUT", string(out[i:]))
				break
			}
			emit("STDOUT", string(out[i:i+j]))
			i += j + 1
		}
	}
	return log
}

func indexByte(b []byte, c byte) int {
	for i, x := range b {
		if x == c {
			return i
		}
	}
	return -1
}

// onEvent decodes the typed event buffer from wasm memory.
func (e *CLua) onEvent(raw []byte, kind int32, emit func(string, string)) {
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
