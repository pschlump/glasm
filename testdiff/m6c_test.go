package testdiff

// M6c gates (design §8.6, ledger rows 33-34): the production blob flavor
// (lua55_prod.wasm — zero wasi imports) and the sandbox globals lockdown
// (rt_sandbox(1)), plus the D5 determinism gates in m6cdet_test.go.
//
// The artifact test parses the embedded blob's own section bytes: the
// build's output is self-checking (A8) — if a wasi import ever leaks back
// in (a vendored edit, a wasi-sdk change), this fails before any engine
// runs. The surface test pins the frozen globals list (m6 plan §4.3);
// the matrix proves dev-blob+sandbox ≡ prod-blob over the full corpus.

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"sort"
	"strings"
	"testing"

	"github.com/pschlump/glasm/wasm"
)

// lua55ProdSHA256 pins the embedded 5.5 prod blob (U6d, row 95;
// re-bumped at U6e: the hostfn seam — host.host_call import +
// rt_hostfn/rt_encode_value exports, and the PT_INTEGER wire tag;
// re-bumped at U9a2: the lresults driver export; re-bumped at U9c:
// the rt_dd_* direct-dispatch exports, row 104; re-bumped at U9c again:
// rt_dd_enter's line param — the budget arm stages "stack overflow"
// with the call site's position, same commit as the backend arm; and
// again: the direct frame's stack presence — a minimal real CallInfo +
// sea entry per direct call, so lua_getstack/getinfo and where() see
// direct frames like trampoline ones, row 104).
// Rebuild runtime/build55.sh → update this constant (the artifact test
// is the self-check that forces the bump). Brittle by design (A8:
// checksummed artifact).
const lua55ProdSHA256 = "f26793954d8ef1d535575ed8cd27378fc1d582ea0ee098b7f9d6552909c4ddd7"

// TestM6cProdBlobArtifact (U6d edition, ledger row 95): lua55_prod.wasm
// — the 5.5 production flavor of the rt blob. The build's output is
// self-checking (A8): zero wasi imports (any leak — a vendored edit, a
// wasi-sdk change — fails here before any engine runs), exactly the three
// host.* imports the driver declares (U6e added host_call), the full v4
// rt_* export surface,
// and byte-identity with host/'s copy (the embed dance's drift lock).
func TestM6cProdBlobArtifact(t *testing.T) {
	imps, err := wasm.Imports(lua55ProdWasm)
	if err != nil {
		t.Fatalf("parse prod blob imports: %v", err)
	}
	got := map[string]bool{}
	for _, im := range imps {
		if im.Module != "host" {
			t.Errorf("import %s.%s: non-host module (prod blob must have zero wasi imports)",
				im.Module, im.Name)
		}
		got[im.Name] = true
	}
	want := []string{"event", "wasm_dispatch", "host_call"}
	for _, w := range want {
		if !got[w] {
			t.Errorf("host.%s missing from prod blob (have %v)", w, got)
		}
	}
	if len(imps) != len(want) {
		t.Errorf("prod blob has %d imports, want exactly the %d host.* ones", len(imps), len(want))
	}

	exports, err := wasm.Exports(lua55ProdWasm)
	if err != nil {
		t.Fatalf("parse prod blob exports: %v", err)
	}
	exp := map[string]bool{}
	for _, e := range exports {
		exp[e.Name] = true
	}
	for _, name := range []string{
		// driver surface
		"lnewstate", "lclose", "ldostring", "lglobals", "linbuf", "lnamebuf",
		"lerrlen", "lerrcopy", "lresults",
		// ABI v4 core
		"rt_abi_version", "rt_set_state", "rt_frame_alloc", "rt_settable",
		"rt_gettable", "rt_arith", "rt_concat", "rt_call_addr", "rt_forprep",
		"rt_getglobal", "rt_setglobal", "rt_wasm_proto", "rt_newclosure",
		"rt_varargtab", "rt_close", "rt_wasm_push_frame", "rt_frame_restore",
		// U6b: the cap + deadline surface (ledger rows 36-37)
		"rt_set_memlimit", "rt_mem_used_bytes", "rt_ctrl_addr", "rt_set_deadline",
		"rt_deadline_flag", "rt_deadline",
		// U6c: the lockdown surface (row 94)
		"rt_sandbox", "rt_protect_globals", "rt_globals_readonly",
		// U6e: the hostfn seam (row 96)
		"rt_hostfn", "rt_encode_value",
		// U9c: the direct-dispatch helpers (row 104)
		"rt_dd_callee", "rt_dd_enter", "rt_dd_ret", "rt_dd_err",
		"rt_dd_reset"} {
		if !exp[name] {
			t.Errorf("prod blob export %s missing", name)
		}
	}

	sum := sha256.Sum256(lua55ProdWasm)
	if hex.EncodeToString(sum[:]) != lua55ProdSHA256 {
		t.Errorf("prod blob checksum %s != pinned %s (stale embed? rebuild + go clean -testcache, then bump the const)",
			hex.EncodeToString(sum[:]), lua55ProdSHA256)
	}

	// the host package embeds its own copy (U6e's cutover; go:embed
	// cannot cross package dirs) — the two must be byte-identical.
	hostCopy, err := os.ReadFile("../host/lua55_prod.wasm")
	if err != nil {
		t.Fatalf("read host copy: %v", err)
	}
	if !bytes.Equal(hostCopy, lua55ProdWasm) {
		t.Error("host/lua55_prod.wasm differs from testdiff's copy — re-run runtime/build55.sh (the copies must move together)")
	}

	// dev-blob sanity: the parser reads the 5.5 oracle blob's wasi imports
	// fine — guards against a parser regression passing trivially.
	if wasi, err := wasm.HasWASIImports(lua55SjljWasm); err != nil || !wasi {
		t.Errorf("sjlj blob HasWASIImports = %v, %v; want true (parser sanity)", wasi, err)
	}
}

// sandboxCase runs src on a sandboxed engine (dev or prod blob) and
// returns its log.
func sandboxCase(t *testing.T, e Engine, src string) []string {
	t.Helper()
	log := e.Run(Case{Name: "m6c_sandbox.lua", Dir: ".", Source: []byte(src)})
	for _, l := range log {
		if strings.HasPrefix(l, "ENGINE-") {
			t.Fatalf("engine error under sandbox: %s", l)
		}
	}
	return log
}

func TestM6cSandboxSurface(t *testing.T) {
	// U6c (ledger row 94): the sandbox surface re-pinned on the 5.5 dev
	// blob — rt_sandbox(1) nils io os package require dofile loadfile
	// load debug warn (loadstring/module never existed in 5.5; utf8 and
	// print stay — m6 plan §4.3's list, 5.5-shaped). U6d (row 95): the
	// prod legs run the same script on lua55_prod.wasm, where the
	// surface is COMPILED OUT (linit55.c) — rt_sandbox is the no-op
	// returning 0 and the observable _G is identical to dev+sandbox.
	src := `
local killed = {"io","os","package","require","dofile","loadfile","load","debug","warn"}
for _, g in ipairs(killed) do
  if _G[g] ~= nil then print("LEAK "..g) end
end
local kept = {"pcall","xpcall","error","assert","select","next","ipairs","pairs","print",
  "_G","_VERSION","string","table","math","utf8","coroutine",
  "tostring","tonumber","type","rawget","rawset","rawequal","rawlen",
  "setmetatable","getmetatable","collectgarbage"}
for _, g in ipairs(kept) do
  if _G[g] == nil then print("MISSING "..g) end
end
-- the kept surface actually works, and the killed one errors
print(pcall(function() io.write("x") end))
print(type(string.format("%d", 7)))
`
	wantTail := []string{
		// stock 5.5 index message with the position prefix (row 56)
		"PRINT\tfalse\tm6c_sandbox.lua:14: attempt to index a nil value (global 'io')",
		"PRINT\tstring",
	}

	engines := []struct {
		name string
		e    Engine
	}{
		{"dev-wazero-sandbox", &WazeroEngine{name: "dev-wazero-sandbox", Sandbox: true}},
		{"prod-wazero", &WazeroEngine{name: "prod-wazero", RtBin: lua55ProdWasm, Sandbox: true}},
		{"prod-wasmtime", &WasmEngine{name: "prod-wasmtime", RtBin: lua55ProdWasm, Sandbox: true}},
	}
	for _, tc := range engines {
		t.Run(tc.name, func(t *testing.T) {
			log := sandboxCase(t, tc.e, src)
			var prints []string
			for _, l := range log {
				if strings.HasPrefix(l, "PRINT\t") {
					prints = append(prints, l)
				}
			}
			if len(prints) != 2 {
				t.Fatalf("prints = %v (leaks/missing above would add lines)", prints)
			}
			for i, w := range wantTail {
				if prints[i] != w {
					t.Errorf("print[%d] = %q, want %q", i, prints[i], w)
				}
			}
			// io.flush drain skipped, no STDOUT by construction
			for _, l := range log {
				if strings.HasPrefix(l, "STDOUT\t") {
					t.Errorf("unexpected STDOUT line under sandbox: %s", l)
				}
			}
		})
	}
}

// TestM6cProtectGlobals (U6c, ledger row 94): the Redis 7.2.7 script-env
// lockdown on the dev blob — rt_protect_globals(1) after sandbox + arg
// staging. Writes into every table reachable from _G raise "Attempt to
// modify a readonly table"; missing-global reads raise the Redis text;
// a swapped _ENV (a private table the script built) stays writable. The
// texts are pinned absolutely (host-surface — the interp has no
// counterpart), the rows 36/37 rule.
func TestM6cProtectGlobals(t *testing.T) {
	src := `
local function strip(e) return (e:gsub('^.-:%d+: ', '')) end
local ok, err = pcall(function() string.format = 1 end)
print(ok, strip(err))
local ok2, err2 = pcall(function() brand_new_global = 1 end)
print(ok2, strip(err2))
local ok3, err3 = pcall(function() rawset(_G, "x", 1) end)
print(ok3, strip(err3))
local ok4, err4 = pcall(function() string[1] = 1 end)
print(ok4, strip(err4))
local ok5, err5 = pcall(function() table.insert(string, 1) end)
print(ok5, strip(err5))
local ok6, err6 = pcall(function() return nosuchglobal end)
print(ok6, strip(err6))
-- a sandbox-nil'd global is an ABSENT key, so reading one bare raises
-- the same Redis text (sandbox-nil × protect-raise stacking)
local ok7, err7 = pcall(function() return io end)
print(ok7, strip(err7))
print(rawget(_G, "io"), type(_G), type(string), type(rawget(_G, "string")))
local ok8 = pcall(function() local t = {} t[1] = 1 rawset(t, 2, 2) return #t end)
print(ok8)
local function priv()
  local _ENV = {}
  x = 1
  return x
end
print(priv())
`
	want := []string{
		"PRINT\tfalse\tAttempt to modify a readonly table",
		"PRINT\tfalse\tAttempt to modify a readonly table",
		"PRINT\tfalse\tAttempt to modify a readonly table",
		"PRINT\tfalse\tAttempt to modify a readonly table",
		"PRINT\tfalse\tAttempt to modify a readonly table",
		"PRINT\tfalse\tScript attempted to access nonexistent global variable 'nosuchglobal'",
		"PRINT\tfalse\tScript attempted to access nonexistent global variable 'io'",
		"PRINT\tnil\ttable\ttable\ttable",
		"PRINT\ttrue",
		"PRINT\t1",
	}
	engines := []struct {
		name string
		e    Engine
	}{
		{"wasm", &WasmEngine{name: "wasm-protect", Sandbox: true, Protect: true}},
		{"wazero", &WazeroEngine{name: "wazero-protect", Sandbox: true, Protect: true}},
	}
	for _, tc := range engines {
		t.Run(tc.name, func(t *testing.T) {
			log := sandboxCase(t, tc.e, src)
			var prints []string
			for _, l := range log {
				if strings.HasPrefix(l, "PRINT\t") {
					prints = append(prints, l)
				}
			}
			if len(prints) != len(want) {
				t.Fatalf("prints = %v (want %d lines)", prints, len(want))
			}
			for i, w := range want {
				if prints[i] != w {
					t.Errorf("print[%d] = %q, want %q", i, prints[i], w)
				}
			}
		})
	}
}

// TestM6cSandboxGlobalsFrozen pins the exact _G key set under the prod
// sandbox — the frozen allowlist (any change to the sandbox surface or the
// kept libraries is a ledger-row-33/95 change and must update this
// constant). U6d: re-pinned on lua55_prod.wasm — 5.5 base minus the
// loader entries, plus coroutine/string/table/math/utf8 and the host's
// arg table.
func TestM6cSandboxGlobalsFrozen(t *testing.T) {
	src := `
local t = {}
for k in pairs(_G) do t[#t+1] = k end
table.sort(t)
print(table.concat(t, ","))
`
	log := sandboxCase(t, &WazeroEngine{name: "prod", RtBin: lua55ProdWasm, Sandbox: true}, src)
	var got string
	for _, l := range log {
		if strings.HasPrefix(l, "PRINT\t") {
			got = strings.TrimPrefix(l, "PRINT\t")
		}
	}
	names := strings.Split(got, ",")
	sort.Strings(names)
	gotSorted := strings.Join(names, ",")
	want := "_G,_VERSION,arg,assert,collectgarbage,coroutine,error,getmetatable,ipairs,math,next,pairs,pcall,print,rawequal,rawget,rawlen,rawset,select,setmetatable,string,table,tonumber,tostring,type,utf8,xpcall"
	if gotSorted != want {
		t.Errorf("frozen _G set drifted:\n got  %q\n want %q", gotSorted, want)
	}
}

// TestM6cSandboxMatrix: the dev blob under rt_sandbox(1) must be
// indistinguishable from the prod blob over the full opcode-matrix corpus
// (GLOBALS lines are dropped by DiffLogs by contract; no corpus case uses
// the removed surface outside dead code — see the ledger row 33 notes).
// U6d (row 95): re-armed on lua55_prod.wasm — dev-sandbox's nine nils
// vs the compiled-out surface must be the SAME observable engine.
func TestM6cSandboxMatrix(t *testing.T) {
	cases, err := LoadCorpus("../_wasm-tests")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
	dev := &WazeroEngine{name: "dev-sandbox", Sandbox: true, SkipUnsupported: true}
	prod := &WazeroEngine{name: "prod-sandbox", RtBin: lua55ProdWasm, Sandbox: true, SkipUnsupported: true}
	for _, c := range cases {
		a := dev.Run(c)
		b := prod.Run(c)
		if diff := DiffLogs(a, b); diff != "" {
			t.Errorf("%s: %s", c.Name, diff)
		}
	}
}
