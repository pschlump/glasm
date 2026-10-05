-- U5b: the main chunk is a vararg function (unnamed, hidden model) and
-- the implicit 'arg' LOCAL is gone everywhere: script arguments are only
-- '...', functions no longer get a compat arg table, and load()ed chunks
-- get their own empty vararg frame. (The HARNESS still seeds a global
-- arg = {[0]=chunkname} as a host surface in every engine identically;
-- clear it first so the language surface below is the real one.)
print(select('#', ...), ...)
arg = nil
print(arg, _G.arg)

-- '...' at main level flows into calls, tables, and returns
local more = {...}
print(#more)
print((select(1, ...)))
local function id(...) return ... end
print(id(...))
print(id())

-- a loaded chunk has its own vararg main (hidden, zero extras here)
local f = load("return select('#', ...), ...")
print(f("a", "b"))
print(f())

-- functions closing over main's '...' cannot (unnamed); the pack idiom
-- is the carrier
local packed = table.pack(...)
local function cnt() return packed.n end
print(cnt())

-- no compat arg local anywhere: indexing the now-nil global raises with
-- the global-name hint (level-1 pcall wrap, position stripped)
local ok, err = pcall(function() return arg.x end)
print(ok, (err:gsub("^.*:%d+: ", "")))
arg = "set-as-global"
print(arg, _G.arg)
arg = nil
print("done")
