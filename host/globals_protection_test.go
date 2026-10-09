package host

// Globals-protection tests (WithGlobalsProtection): the Redis-classic
// script environment lockdown — undefined-global reads raise via the _G
// error metatable, and globals + all reachable tables are readonly (the
// runtime's deps/lua-parity patch). Byte texts probed against Redis 7.2.7.

import (
	"testing"
)

func runProtected(t *testing.T, e *Engine, src string) (Result, error) {
	t.Helper()
	s := mustCompile(t, e, src)
	vm, err := e.NewVM()
	if err != nil {
		t.Fatalf("NewVM: %v", err)
	}
	t.Cleanup(func() { _ = vm.Close() })
	return vm.Run(t.Context(), s, RunOptions{Keys: []string{"k1"}, Argv: []string{"a1"}})
}

func wantScriptError(t *testing.T, src string, res Result, err error, wantMsg string) {
	t.Helper()
	if err == nil {
		t.Errorf("%q: got values %v, want error %q", src, res.Values, wantMsg)
		return
	}
	se, ok := err.(*ScriptError)
	if !ok {
		t.Errorf("%q: got %T %v, want *ScriptError", src, err, err)
		return
	}
	if got := se.ErrValue.String(); got != wantMsg {
		t.Errorf("%q:\n got %q\nwant %q", src, got, wantMsg)
	}
	if se.Line != 1 {
		t.Errorf("%q: line = %d, want 1", src, se.Line)
	}
}

func TestGlobalsProtectionErrors(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	cases := []struct{ src, want string }{
		{"return undefined_global",
			"=test:1: Script attempted to access nonexistent global variable 'undefined_global'"},
		{"g = 5 return g", "=test:1: Attempt to modify a readonly table"},
		{"redis = nil return 1", "=test:1: Attempt to modify a readonly table"},
		{"string.foo = 1 return 1", "=test:1: Attempt to modify a readonly table"},
		{"_G.pairs = nil return 1", "=test:1: Attempt to modify a readonly table"},
		{"return _G[nil]",
			"=test:1: Second argument to luaProtectedTableError must be a string or number"},
		// rawset/rawseti: caught INSIDE the script (pcall) the raise is
		// bare — the m6c corpus pin (row 94; the raise happens under the
		// rawset C function frame, no position anywhere). UNCAUGHT (this
		// shape), the v4 seam re-pins it prefixed: the guard raises via
		// the luaG_runerror replica (nopos cleared — the core-raise
		// rule), so the catching rt_run bolts the raise site's
		// chunk:line on (row 85b family; v3/Redis's bare form was an
		// isLua(ci) artifact of C-VM execution with no compiled-frame
		// analog)
		{"rawset(_G, 'g3', 1) return 1", "=test:1: Attempt to modify a readonly table"},
	}
	for _, c := range cases {
		res, err := runProtected(t, e, c.src)
		wantScriptError(t, c.src, res, err, c.want)
	}
}

func TestGlobalsProtectionAllows(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	cases := []struct {
		src  string
		want []Value
	}{
		{"return rawget(_G, 'undefined_global') == nil", []Value{Bool(true)}},
		{"local t = {} rawset(t, 'k', 1) return t.k", []Value{Int(1)}},
		{"KEYS[1] = 'x' return KEYS[1]", []Value{String("x")}},
		{"ARGV[1] = 'y' return ARGV[1]", []Value{String("y")}},
		{"local t = setmetatable({}, {__index=function() return 7 end}) return t.x", []Value{Int(7)}},
		{"for k in pairs(_G) do if k=='nosuch' then return 1 end end return 0", []Value{Int(0)}},
		{"return getmetatable(_G) ~= nil", []Value{Bool(true)}},
	}
	for _, c := range cases {
		res, err := runProtected(t, e, c.src)
		if err != nil {
			t.Errorf("%q: %v", c.src, err)
			continue
		}
		if len(res.Values) != len(c.want) || !valuesEqual(res.Values[0], c.want[0]) {
			t.Errorf("%q: got %v, want %v", c.src, res.Values, c.want)
		}
	}
}

// the caught message carries the position prefix (Redis parity), and the
// run that follows a killed/errored script is unaffected
func TestGlobalsProtectionPcall(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	res, err := runProtected(t, e,
		"local ok,e = pcall(function() return nosuchglobal end) return {ok, e}")
	if err != nil {
		t.Fatalf("pcall of guard raise must not escape: %v", err)
	}
	msg := res.Values[0].Pairs[1].Val.Str
	want := "=test:1: Script attempted to access nonexistent global variable 'nosuchglobal'"
	if msg != want {
		t.Errorf("caught message:\n got %q\nwant %q", msg, want)
	}
	res, err = runProtected(t, e,
		"local ok,e = pcall(function() g = 5 end) return {ok, e}")
	if err != nil {
		t.Fatalf("pcall of readonly raise must not escape: %v", err)
	}
	if msg = res.Values[0].Pairs[1].Val.Str; msg != "=test:1: Attempt to modify a readonly table" {
		t.Errorf("caught message: %q", msg)
	}
}

// default unchanged: without the option, scripts create/read globals freely
func TestGlobalsProtectionOffByDefault(t *testing.T) {
	e := mustEngine(t)
	res, err := runProtected(t, e, "g = 5 return g + #KEYS")
	if err != nil {
		t.Fatalf("protection off: %v", err)
	}
	if !valuesEqual(res.Values[0], Int(6)) {
		t.Errorf("got %v, want 6", res.Values)
	}
	res, err = runProtected(t, e, "return undefined_global == nil")
	if err != nil {
		t.Fatalf("protection off: %v", err)
	}
	if !valuesEqual(res.Values[0], Bool(true)) {
		t.Errorf("got %v, want true", res.Values)
	}
}

// staging still works through the readonly window: constants registered
// before VM creation are staged before protection, KEYS/ARGV inside it
func TestGlobalsProtectionStaging(t *testing.T) {
	e := mustEngine(t, WithGlobalsProtection(true))
	if err := e.RegisterValue("redis", "ANSWER", Int(42)); err != nil {
		t.Fatalf("RegisterValue: %v", err)
	}
	res, err := runProtected(t, e, "return redis.ANSWER + #KEYS")
	if err != nil {
		t.Fatalf("staging under protection: %v", err)
	}
	if !valuesEqual(res.Values[0], Int(43)) {
		t.Errorf("got %v, want 43", res.Values)
	}
	res, err = runProtected(t, e, "return #KEYS..'/'..#ARGV")
	if err != nil {
		t.Fatalf("KEYS/ARGV staging under protection: %v", err)
	}
	if !valuesEqual(res.Values[0], String("1/1")) {
		t.Errorf("got %v, want 1/1", res.Values)
	}
}
