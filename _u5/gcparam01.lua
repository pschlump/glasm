-- U5c (D10): collectgarbage("param", name[, value]) — protocol-exact
-- get/set over the six gcparams with luaO_codeparam's quantization
-- round-trip. Tuning is a storage no-op in the fork (no real GC
-- surface), but the protocol — old-value return, optional set, the
-- arg-error battery — is byte-exact against stock.
local function strip(m) return (tostring(m):gsub("^.-:%d+: ", "")) end

-- 1: defaults (wasm32 sizing: 70 -> 68, stepsize = 200*sizeof(Table))
print(collectgarbage("param", "minormul"),
      collectgarbage("param", "majorminor"),
      collectgarbage("param", "minormajor"),
      collectgarbage("param", "pause"),
      collectgarbage("param", "stepmul"),
      collectgarbage("param", "stepsize"))

-- 2: set returns the OLD value; the next read answers the quantized new
print(collectgarbage("param", "pause", 200))
print(collectgarbage("param", "pause"))
print(collectgarbage("param", "minormul", 1))
print(collectgarbage("param", "minormul"))
print(collectgarbage("param", "stepmul", 300))
print(collectgarbage("param", "stepmul"))
-- saturation: an absurd value saturates the code, applyparam answers it
print(collectgarbage("param", "pause", 2^53))
print(collectgarbage("param", "pause"))
-- a float with an exact integer value is fine
print(collectgarbage("param", "pause", 300.0))
print(collectgarbage("param", "pause"))

-- 3: the error battery (positions stripped: raised from the C function)
local ok, msg = pcall(collectgarbage, "param", "bogus")
print(ok, strip(msg))
ok, msg = pcall(collectgarbage, "param")
print(ok, strip(msg))
ok, msg = pcall(collectgarbage, "param", "pause", "x")
print(ok, strip(msg))
ok, msg = pcall(collectgarbage, "param", "pause", 1.5)
print(ok, strip(msg))
ok, msg = pcall(collectgarbage, "param", 42)
print(ok, strip(msg))
print("done")
