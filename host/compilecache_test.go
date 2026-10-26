package host

// Compile-cache gates (wazero-performance-001): the on-disk posture —
// wazero machine-code cache under the dir, compiled script artifacts
// under dir/scripts (pluto two-tier lru_disk_ts) — round-trips across
// engines, WithCompileMemCache bounds the memory tier in both the disk
// and the in-memory posture, and the option's failure modes are
// constructor errors.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pschlump/pluto/lru_disk_ts"
)

// rawEnc/rawDec are the identity codecs of a byte-typed view over the
// script cache dir — reading the exact payload the engine wrote.
func rawEnc(b []byte) []byte          { return b }
func rawDec(b []byte) ([]byte, error) { return b, nil }

func TestCompileCacheDirRoundTrip(t *testing.T) {
	dir := t.TempDir()
	src := []byte("local t = {} for i = 1, 100 do t[i] = i * 2 end return #t")

	e1, err := NewEngine(WithCompileCacheDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	s1, err := e1.Compile(src, "=disk")
	if err != nil {
		t.Fatal(err)
	}
	// the artifact persisted under dir/scripts and decodes back complete
	sd, err := lru_disk_ts.NewLruDisk[[]byte](8, filepath.Join(dir, "scripts"), rawEnc, rawDec)
	if err != nil {
		t.Fatal(err)
	}
	payload, ok := sd.Get(s1.SHA1 + "\x00=disk\x00" + versionSalt())
	if !ok {
		t.Fatal("script artifact not on disk after Compile")
	}
	got, err := decScript(payload)
	if err != nil {
		t.Fatalf("on-disk payload does not decode: %v", err)
	}
	if string(got.Wasm) != string(s1.Wasm) || got.SHA1 != s1.SHA1 || got.name != "=disk" {
		t.Fatal("disk artifact differs from the compiled script")
	}
	// and the engine that compiled it still runs it
	if _, err := e1.Run(context.Background(), s1, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	e1.Close()

	// a fresh engine over the same directory serves the same artifact
	// bytes and the same run results (the restart / daemon-redeploy path)
	e2, err := NewEngine(WithCompileCacheDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	s2, err := e2.Compile(src, "=disk")
	if err != nil {
		t.Fatal(err)
	}
	if s2.SHA1 != s1.SHA1 || string(s2.Wasm) != string(s1.Wasm) {
		t.Fatal("second engine's artifact differs")
	}
	if res, err := e2.Run(context.Background(), s2, RunOptions{}); err != nil || len(res.Values) != 1 || res.Values[0].Int != 100 {
		t.Fatalf("run = %+v %v", res, err)
	}
}

func TestCompileCacheDirComposesWithPool(t *testing.T) {
	dir := t.TempDir()
	e, err := NewEngine(WithCompileCacheDir(dir), WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s := mustCompile(t, e, "return #KEYS")
	if got := runInt(t, e, s, RunOptions{Keys: []string{"a", "b"}}); got != 2 {
		t.Fatalf("run 1 = %d", got)
	}
	if got := runInt(t, e, s, RunOptions{Keys: []string{"a"}}); got != 1 {
		t.Fatalf("pooled run = %d", got)
	}
	if e.vms != 1 {
		t.Fatalf("vms = %d (pool broken under the disk cache)", e.vms)
	}
}

func TestCompileCacheDirBadPath(t *testing.T) {
	// a path whose parent is a regular file cannot become a cache dir
	dead := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(dead, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := NewEngine(WithCompileCacheDir(filepath.Join(dead, "cache"))); err == nil {
		t.Fatal("NewEngine accepted an unusable cache dir")
	}
}

func TestCompileCacheDefaultInMemory(t *testing.T) {
	e, err := NewEngine()
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.scriptDisk != nil || e.scriptLru != nil {
		t.Fatal("default engine has a bounded script cache")
	}
	if e.cache == nil {
		t.Fatal("default engine lost the unbounded map")
	}
}

// TestCompileMemCacheTwoTier pins the disk posture's memory tier: the
// configured bound holds, resident sources keep their pointer, a
// demoted source decodes from disk with identical bytes.
func TestCompileMemCacheTwoTier(t *testing.T) {
	dir := t.TempDir()
	e, err := NewEngine(WithCompileCacheDir(dir), WithCompileMemCache(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got := e.scriptDisk.MemCapacity(); got != 2 {
		t.Fatalf("mem capacity = %d", got)
	}
	s1 := mustCompile(t, e, "return 1")
	mustCompile(t, e, "return 2")
	s3 := mustCompile(t, e, "return 3") // demotes s1
	if got := e.scriptDisk.MemLen(); got > 2 {
		t.Fatalf("mem tier = %d entries (unbounded)", got)
	}
	// a resident source is served as the same pointer
	if again := mustCompile(t, e, "return 3"); again != s3 {
		t.Fatal("resident source minted a fresh pointer")
	}
	// the demoted source decodes from disk — a fresh pointer, identical
	// bytes, and it still runs
	s1b := mustCompile(t, e, "return 1")
	if s1b == s1 {
		t.Fatal("demoted source kept its pointer")
	}
	if s1b.SHA1 != s1.SHA1 || string(s1b.Wasm) != string(s1.Wasm) {
		t.Fatal("decoded artifact differs from the compiled one")
	}
	if got := runInt(t, e, s1b, RunOptions{}); got != 1 {
		t.Fatalf("decoded script run = %d", got)
	}
}

// TestCompileMemCacheDefault pins the unset knob's disk-posture default:
// the memory tier on at the disk capacity — the old unbounded front
// map's posture, now bounded.
func TestCompileMemCacheDefault(t *testing.T) {
	e, err := NewEngine(WithCompileCacheDir(t.TempDir()))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if got := e.scriptDisk.MemCapacity(); got != defaultScriptMemCache {
		t.Fatalf("default mem capacity = %d, want %d", got, defaultScriptMemCache)
	}
}

// TestCompileMemCachePureDisk pins n=0: no retained artifacts at all —
// every Compile hit re-reads its file, and the restart path still
// serves identical bytes.
func TestCompileMemCachePureDisk(t *testing.T) {
	dir := t.TempDir()
	src := []byte("return 7")
	e1, err := NewEngine(WithCompileCacheDir(dir), WithCompileMemCache(0))
	if err != nil {
		t.Fatal(err)
	}
	s1 := mustCompile(t, e1, string(src))
	if got := e1.scriptDisk.MemCapacity(); got != 0 {
		t.Fatalf("pure-disk mem capacity = %d", got)
	}
	if got := runInt(t, e1, s1, RunOptions{}); got != 7 {
		t.Fatalf("run = %d", got)
	}
	s1b := mustCompile(t, e1, string(src)) // disk hit, no tier
	if string(s1b.Wasm) != string(s1.Wasm) {
		t.Fatal("disk re-read differs")
	}
	e1.Close()

	e2, err := NewEngine(WithCompileCacheDir(dir), WithCompileMemCache(0))
	if err != nil {
		t.Fatal(err)
	}
	defer e2.Close()
	if got := runInt(t, e2, mustCompile(t, e2, string(src)), RunOptions{}); got != 7 {
		t.Fatalf("restart run = %d", got)
	}
}

// TestCompileMemCacheInMemory pins the no-dir posture: the bound
// replaces the unbounded map, eviction drops pointers, and an evicted
// source recompiles to identical bytes.
func TestCompileMemCacheInMemory(t *testing.T) {
	e, err := NewEngine(WithCompileMemCache(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	if e.cache != nil || e.scriptLru == nil {
		t.Fatal("bounded in-memory posture not selected")
	}
	s1 := mustCompile(t, e, "return 1")
	mustCompile(t, e, "return 2")
	mustCompile(t, e, "return 3") // evicts s1
	if got := e.scriptLru.Len(); got != 2 {
		t.Fatalf("lru len = %d", got)
	}
	s1b := mustCompile(t, e, "return 1") // recompiles through the frontend
	if s1b == s1 {
		t.Fatal("evicted source kept its pointer")
	}
	if s1b.SHA1 != s1.SHA1 || string(s1b.Wasm) != string(s1.Wasm) {
		t.Fatal("recompiled artifact differs")
	}
	if got := runInt(t, e, s1b, RunOptions{}); got != 1 {
		t.Fatalf("recompiled run = %d", got)
	}
}

// TestCompileCacheLegacyPayloadSelfHeals pins the migration from the
// pre-two-tier raw-Wasm payload: such an entry fails decode, is treated
// as a miss (file removed), and the recompile overwrites it.
func TestCompileCacheLegacyPayloadSelfHeals(t *testing.T) {
	dir := t.TempDir()
	src := []byte("return 9")

	// a legacy engine shape: raw-Wasm payloads, no memory tier
	legacy, err := NewEngine(WithCompileCacheDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	s1, err := legacy.Compile(src, "=legacy")
	if err != nil {
		t.Fatal(err)
	}
	raw, err := lru_disk_ts.NewLruDisk[[]byte](8, filepath.Join(dir, "scripts"), rawEnc, rawDec)
	if err != nil {
		t.Fatal(err)
	}
	key := s1.SHA1 + "\x00=legacy\x00" + versionSalt()
	if err := raw.Put(key, s1.Wasm); err != nil { // clobber the entry with the old payload form
		t.Fatal(err)
	}
	legacy.Close()

	// a new engine over the dir: the legacy payload self-heals
	e, err := NewEngine(WithCompileCacheDir(dir))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	s2, err := e.Compile(src, "=legacy")
	if err != nil {
		t.Fatal(err)
	}
	if string(s2.Wasm) != string(s1.Wasm) {
		t.Fatal("self-healed artifact differs")
	}
	if got := runInt(t, e, s2, RunOptions{}); got != 9 {
		t.Fatalf("self-healed run = %d", got)
	}
	// ...and the healed entry is in the new payload form
	payload, ok := raw.Get(key)
	if !ok {
		t.Fatal("healed entry missing")
	}
	if _, err := decScript(payload); err != nil {
		t.Fatalf("healed payload does not decode: %v", err)
	}
}

// TestScriptIdentityAfterEviction pins the one-script law under cache
// eviction: a re-minted pointer of the SAME source runs on an image the
// source already bound (manual VM and pooled path), a different script
// is still refused.
func TestScriptIdentityAfterEviction(t *testing.T) {
	e, err := NewEngine(WithCompileMemCache(1), WithVMCacheSize(2))
	if err != nil {
		t.Fatal(err)
	}
	defer e.Close()
	src := "return 41"

	a1 := mustCompile(t, e, src) // mem tier: {a1}
	vm, err := e.NewVM()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := vm.Run(context.Background(), a1, RunOptions{}); err != nil {
		t.Fatal(err)
	}
	b := mustCompile(t, e, "return 42") // evicts src
	if _, err := vm.Run(context.Background(), b, RunOptions{}); err != ErrScriptBound {
		t.Fatalf("different script on bound VM: %v", err)
	}
	a2 := mustCompile(t, e, src) // re-minted pointer of the same source
	if a2 == a1 {
		t.Fatal("expected a fresh pointer after eviction")
	}
	if _, err := vm.Run(context.Background(), a2, RunOptions{}); err != nil {
		t.Fatalf("re-minted pointer of the same source refused: %v", err)
	}
	vm.Close()

	// the pooled path: a pooled image bound to a1 runs a2 by identity
	if got := runInt(t, e, a1, RunOptions{}); got != 41 { // image pooled under src's key
		t.Fatalf("run a1 = %d", got)
	}
	if got := runInt(t, e, b, RunOptions{}); got != 42 { // second pooled image
		t.Fatalf("run b = %d", got)
	}
	if got := runInt(t, e, a2, RunOptions{}); got != 41 {
		t.Fatalf("pooled image refused the re-minted pointer: %d", got)
	}
}
