package host

// WithCompileCacheDir gates (wazero-performance-001): the on-disk
// posture — wazero machine-code cache under the dir, compiled script
// artifacts under dir/scripts (pluto lru_disk_ts) — round-trips across
// engines, and the option's failure modes are constructor errors.

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/pschlump/pluto/lru_disk_ts"
)

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
	// the artifact persisted under dir/scripts and reads back identical
	sd, err := lru_disk_ts.NewLruDisk[[]byte](8, filepath.Join(dir, "scripts"), encBytes, decBytes)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := sd.Get(s1.SHA1 + "\x00=disk\x00" + versionSalt())
	if !ok {
		t.Fatal("script artifact not on disk after Compile")
	}
	if string(got) != string(s1.Wasm) {
		t.Fatal("disk artifact differs from the compiled Wasm")
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
	if e.scriptDisk != nil {
		t.Fatal("default engine has a disk script cache")
	}
}
