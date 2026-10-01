package lua

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

/* load lib {{{ */

// U4g re-pin (attrib.lua:67): the stock searcher set — preload, Lua files
// (package.path), C files (package.cpath), and the C root (name up to its
// first '.', searched in cpath; no message when the name IS the root).
var loLoaders = []LGFunction{loLoaderPreload, loLoaderLua, loLoaderC, loLoaderCroot}

func loGetPath(env string, defpath string) string {
	path := os.Getenv(env)
	if len(path) == 0 {
		path = defpath
	}
	path = strings.Replace(path, ";;", ";"+defpath+";", -1)
	if os.PathSeparator != '/' {
		dir, err := filepath.Abs(filepath.Dir(os.Args[0]))
		if err != nil {
			panic(err)
		}
		path = strings.Replace(path, "!", dir, -1)
	}
	return path
}

// loSearchPathCore is stock searchpath() (loadlib.c:487): every ? in the
// ;-separated template list substituted with name (each sep in name
// replaced by dirsep first), returning the first existing file — or ""
// plus pusherrornotfound's message, built TEXTUALLY from the whole
// substituted path so every template leaves a line, empty templates
// included (the U4g re-pin; package.searchpath and require's findfile
// share it — stock findfile IS searchpath(name, path, ".", dirsep)).
func loSearchPathCore(name, path, sep, dirsep string) (string, string) {
	if sep != "" && strings.Contains(name, sep) {
		name = strings.Replace(name, sep, dirsep, -1)
	}
	substituted := strings.Replace(path, "?", name, -1)
	last := false
	for rest := substituted; !last; {
		fn := rest
		if i := strings.IndexByte(rest, ';'); i >= 0 {
			fn, rest = rest[:i], rest[i+1:]
		} else {
			last = true
		}
		if _, err := os.Stat(fn); err == nil {
			return fn, ""
		}
	}
	return "", "no file '" + strings.Replace(substituted, ";", "'\n\tno file '", -1) + "'"
}

// loFindFile is stock findfile: package.searchpath(name, package[pname],
// ".", LUA_DIRSEP). The miss message is the searchpath trail (each cpath
// line a "no file '...'" — the old fork leaked raw os.Stat error texts).
func loFindFile(L *LState, name, pname string) (string, string) {
	lv := L.GetField(L.GetField(L.Get(EnvironIndex), "package"), pname)
	path, ok := lv.(LString)
	if !ok {
		L.RaiseError("'package.%s' must be a string", pname)
	}
	return loSearchPathCore(name, string(path), ".", LuaDirSep)
}

func OpenPackage(L *LState) int {
	packagemod := L.RegisterModule(LoadLibName, loFuncs)

	L.SetField(packagemod, "preload", L.NewTable())

	// 5.2 (U1): package.loaders renamed package.searchers (D8 hard cut)
	searchers := L.CreateTable(len(loLoaders), 0)
	for i, loader := range loLoaders {
		L.RawSetInt(searchers, i+1, L.NewFunction(loader))
	}
	L.SetField(packagemod, "searchers", searchers)
	L.SetField(L.Get(RegistryIndex), "_LOADERS", searchers)

	// stock luaopen_package binds package.loaded to the REGISTRY's _LOADED
	// table (luaL_getsubtable), never a fresh one: replacing it orphaned
	// the "package" entry RegisterModule had just seeded, and nextvar.lua's
	// clear-globals loop (keyed on package.loaded) then deleted the package
	// global itself mid-iteration (nextvar.lua:422).
	loaded, _ := L.GetField(L.Get(RegistryIndex), "_LOADED").(*LTable)
	L.SetField(packagemod, "loaded", loaded)

	L.SetField(packagemod, "path", LString(loGetPath(LuaPath, LuaPathDefault)))
	L.SetField(packagemod, "cpath", emptyLString)

	L.SetField(packagemod, "config", LString(LuaDirSep+"\n"+LuaPathSep+
		"\n"+LuaPathMark+"\n"+LuaExecDir+"\n"+LuaIgMark+"\n"))

	L.Push(packagemod)
	return 1
}

var loFuncs = map[string]LGFunction{
	"loadlib": loLoadLib,
	"seeall":  loSeeAll,
	// 5.2 (U1): searchpath — the pure search half of require's path walk
	"searchpath": loSearchPath,
}

func loLoaderPreload(L *LState) int {
	name := L.CheckString(1)
	preload := L.GetField(L.GetField(L.Get(EnvironIndex), "package"), "preload")
	if _, ok := preload.(*LTable); !ok {
		L.RaiseError("package.preload must be a table")
	}
	lv := L.GetField(preload, name)
	if lv == LNil {
		L.Push(LString(fmt.Sprintf("no field package.preload['%s']", name)))
		return 1
	}
	L.Push(lv)
	L.Push(LString(":preload:")) // stock: the loader-data sentinel
	return 2
}

func loLoaderLua(L *LState) int {
	name := L.CheckString(1)
	path, msg := loFindFile(L, name, "path")
	if len(path) == 0 {
		L.Push(LString(msg))
		return 1
	}
	fn, err1 := L.LoadFile(path)
	if err1 != nil {
		// stock checkload's text
		L.RaiseError("error loading module '%s' from file '%s':\n\t%s", name, path, err1.Error())
	}
	L.Push(fn)
	L.Push(LString(path)) // loader data = the file name (stock)
	return 2
}

// loLoaderC is stock searcher_C: package.cpath walk; a miss returns the
// searchpath trail, a hit would need dynamic loading — the fork has none
// (stock's no-DL build raises DLMSG from loadfunc; no corpus reaches it).
func loLoaderC(L *LState) int {
	name := L.CheckString(1)
	path, msg := loFindFile(L, name, "cpath")
	if len(path) == 0 {
		L.Push(LString(msg))
		return 1
	}
	L.RaiseError("dynamic libraries not enabled; check your Lua installation")
	return 0
}

// loLoaderCroot is stock searcher_Croot: for a dotted name, search the
// root module in cpath; a miss returns the trail, the root itself returns
// nothing (findloader skips a non-string, non-function first result).
func loLoaderCroot(L *LState) int {
	name := L.CheckString(1)
	i := strings.IndexByte(name, '.')
	if i < 0 {
		return 0 // is root
	}
	path, msg := loFindFile(L, name[:i], "cpath")
	if len(path) == 0 {
		L.Push(LString(msg))
		return 1
	}
	L.RaiseError("dynamic libraries not enabled; check your Lua installation")
	return 0
}

// loSearchPath implements 5.2+ package.searchpath(name, path [, sep [,
// dirsep]]): walk a ;-separated template path substituting ? for name
// (each occurrence of sep in name replaced by dirsep, "/" by default);
// return the first existing file, or nil + the "no file" trail.
//
// U4g re-pin (the official attrib.lua:33 battery): the message is built
// from the WHOLE substituted path exactly like stock 5.5.1's
// pusherrornotfound — "no file '" + path-with-every-;-replaced-by-
// "'\n\tno file '" + "'" — so every template leaves a line, EMPTY
// templates included (a trailing ';' in the path is an empty template
// with its own "no file ”" line), and a template with several '?'s
// names the name substituted several times (the string.rep finds).
func loSearchPath(L *LState) int {
	fn, msg := loSearchPathCore(L.CheckString(1), L.CheckString(2), L.OptString(3, "."), L.OptString(4, "/"))
	if fn != "" {
		L.Push(LString(fn))
		return 1
	}
	L.Push(LNil)
	L.Push(LString(msg))
	return 2
}

func loLoadLib(L *LState) int {
	L.RaiseError("loadlib is not supported")
	return 0
}

func loSeeAll(L *LState) int {
	mod := L.CheckTable(1)
	mt := L.GetMetatable(mod)
	if mt == LNil {
		mt = L.CreateTable(0, 1)
		L.SetMetatable(mod, mt)
	}
	L.SetField(mt, "__index", L.Get(GlobalsIndex))
	return 0
}

/* }}} */

//
