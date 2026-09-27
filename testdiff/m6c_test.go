package testdiff

// M6c gates (design §8.6, ledger rows 33-34): the production blob flavor
// (lua51_prod.wasm — zero wasi imports) and the sandbox globals lockdown
// (rt_sandbox(1)), plus the D5 determinism gates in m6cdet_test.go.
//
// The artifact test parses the embedded blob's own section bytes: the
// build's output is self-checking (A8) — if a wasi import ever leaks back
// in (a vendored edit, a wasi-sdk change), this fails before any engine
// runs. The surface test pins the frozen globals list (m6 plan §4.3);
// the matrix proves dev-blob+sandbox ≡ prod-blob over the full corpus.

import (
	"crypto/sha256"
	"encoding/hex"
	"sort"
	"strings"
	"testing"

	"github.com/pschlump/golua55/wasm"
)

// lua51ProdSHA256 pins the embedded prod blob. Rebuild runtime/build.sh →
// update this constant (the artifact test is the self-check that forces
// the bump). Brittle by design (A8: checksummed artifact). M7a bump: the
// hostfn seam (host.host_call import + rt_hostfn/rt_encode_value exports).
const lua51ProdSHA256 = "4325f355a86e7144c2229e24653f3995f3d9a5caa16e131dae7afc0640b9333f"

func TestM6cProdBlobArtifact(t *testing.T) {
	imps, err := wasm.Imports(lua51ProdWasm)
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
	want := []string{"event", "random01", "randomint", "randomseed", "wasm_dispatch",
		"host_call"} // M7a: the Lua→Go function seam (redis.call)
	for _, w := range want {
		if !got[w] {
			t.Errorf("host.%s missing from prod blob (have %v)", w, got)
		}
	}
	if len(imps) != len(want) {
		t.Errorf("prod blob has %d imports, want exactly the %d host.* ones", len(imps), len(want))
	}

	exports, err := wasm.Exports(lua51ProdWasm)
	if err != nil {
		t.Fatalf("parse prod blob exports: %v", err)
	}
	exp := map[string]bool{}
	for _, e := range exports {
		exp[e.Name] = true
	}
	for _, name := range []string{"rt_sandbox", "lnewstate", "ldostring", "lglobals",
		"rt_set_state", "rt_set_dialect", "rt_abi_version", "rt_frame_alloc", "linbuf", "lnamebuf",
		// M6d: the cap + deadline surface (ledger rows 36-37)
		"rt_set_memlimit", "rt_mem_used_bytes", "rt_ctrl_addr", "rt_set_deadline",
		// M7a: the hostfn seam + wire result readback
		"rt_hostfn", "rt_encode_value", "rt_err_line",
		"rt_deadline_flag", "rt_deadline"} {
		if !exp[name] {
			t.Errorf("prod blob export %s missing", name)
		}
	}

	sum := sha256.Sum256(lua51ProdWasm)
	if hex.EncodeToString(sum[:]) != lua51ProdSHA256 {
		t.Errorf("prod blob checksum %s != pinned %s (stale embed? rebuild + go clean -testcache, then bump the const)",
			hex.EncodeToString(sum[:]), lua51ProdSHA256)
	}

	// dev-blob sanity: the parser reads the sjlj blob's wasi imports fine
	// (17 today) — guards against a parser regression passing trivially.
	if wasi, err := wasm.HasWASIImports(lua51SjljWasm); err != nil || !wasi {
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
	skipRtSurfacesUntilU6(t)
	// The frozen surface (m6 plan §4.3, ledger row 33): everything the
	// sandbox kills, then everything it keeps. The same script must pass
	// on the prod blob (compile-time removal) and the dev blob
	// (rt_sandbox(1) runtime removal) — one list, both blobs.
	src := `
local killed = {"io","os","package","require","module","dofile","loadfile","load","loadstring","debug"}
for _, g in ipairs(killed) do
  if _G[g] ~= nil then print("LEAK "..g) end
end
local kept = {"pcall","xpcall","error","assert","select","unpack","collectgarbage",
  "tostring","tonumber","type","rawget","rawset","rawequal","setmetatable","getmetatable",
  "ipairs","pairs","next","print","_G","string","table","math","newproxy","gcinfo"}
for _, g in ipairs(kept) do
  if _G[g] == nil then print("MISSING "..g) end
end
-- the kept surface actually works, and the killed one errors
print(pcall(function() io.write("x") end))
print(type(string.format("%d", 7)))
`
	wantTail := []string{
		// gopher dialect (row 9) index message with the position prefix
		"PRINT\tfalse\tm6c_sandbox.lua:13: attempt to index a non-table object(nil) with key 'write'",
		"PRINT\tstring",
	}

	engines := []struct {
		name string
		e    Engine
	}{
		{"prod-wazero", &WazeroEngine{name: "prod-wazero", RtBin: lua51ProdWasm, Sandbox: true}},
		{"prod-wasmtime", &WasmEngine{name: "prod-wasmtime", RtBin: lua51ProdWasm, Sandbox: true}},
		{"dev-wazero-sandbox", &WazeroEngine{name: "dev-wazero-sandbox", Sandbox: true}},
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

// TestM6cSandboxGlobalsFrozen pins the exact _G key set under the prod
// sandbox — the frozen allowlist (any change to the sandbox surface or the
// kept libraries is a ledger-row-33 change and must update this constant).
func TestM6cSandboxGlobalsFrozen(t *testing.T) {
	skipRtSurfacesUntilU6(t)
	src := `
local t = {}
for k in pairs(_G) do t[#t+1] = k end
table.sort(t)
print(table.concat(t, ","))
`
	log := sandboxCase(t, &WazeroEngine{name: "prod", RtBin: lua51ProdWasm, Sandbox: true}, src)
	var got string
	for _, l := range log {
		if strings.HasPrefix(l, "PRINT\t") {
			got = strings.TrimPrefix(l, "PRINT\t")
		}
	}
	names := strings.Split(got, ",")
	sort.Strings(names)
	gotSorted := strings.Join(names, ",")
	want := "_G,_VERSION,arg,assert,collectgarbage,coroutine,error,gcinfo,getfenv,getmetatable,ipairs,math,newproxy,next,pairs,pcall,print,rawequal,rawget,rawset,select,setfenv,setmetatable,string,table,tonumber,tostring,type,unpack,xpcall"
	if gotSorted != want {
		t.Errorf("frozen _G set drifted:\n got  %q\n want %q", gotSorted, want)
	}
}

// TestM6cSandboxMatrix: the dev blob under rt_sandbox(1) must be
// indistinguishable from the prod blob over the full opcode-matrix corpus
// (GLOBALS lines are dropped by DiffLogs by contract; no corpus case uses
// the removed surface outside dead code — see the ledger row 33 notes).
func TestM6cSandboxMatrix(t *testing.T) {
	skipRtSurfacesUntilU6(t)
	cases, err := LoadCorpus("../_wasm-tests")
	if err != nil {
		t.Fatalf("load corpus: %v", err)
	}
	if len(cases) == 0 {
		t.Fatal("empty corpus")
	}
	dev := &WazeroEngine{name: "dev-sandbox", Sandbox: true, SkipUnsupported: true}
	prod := &WazeroEngine{name: "prod-sandbox", RtBin: lua51ProdWasm, Sandbox: true, SkipUnsupported: true}
	for _, c := range cases {
		a := dev.Run(c)
		b := prod.Run(c)
		if diff := DiffLogs(a, b); diff != "" {
			t.Errorf("%s: %s", c.Name, diff)
		}
	}
}
