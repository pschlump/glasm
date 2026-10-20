print("dd02")
-- error() with level >= 2 across a direct-dispatched frame: the ruled
-- row-104 divergence. A direct frame has no CallInfo and pushes no
-- shadow-stack entry, so luaL_where's level walk cannot count it --
-- level 2 lands on the wrong frame (bare "m" here) where the oracles
-- attribute the call site. Runs on the interp<->clua55 oracle column
-- (TestU9Gate); skipped on the wasm-family legs citing row 104 (the
-- same ruling as _wasm-err-tests' e003/e005/e011).
local function g() error("m", 2) end
local function f() g() end
print(pcall(f))
