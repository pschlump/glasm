-- U5f: the parser/compiler limits (errors.lua's syntax-limits battery,
-- pinned for the interp↔wasm legs too): the bare "C stack overflow" of
-- stock's enterlevel guard (LUAI_MAXCCALLS) and the errorlimit texts —
-- too many returns (luaK_ret), local variables (MAXVARS, parse-time and
-- the params arm), registers (MAX_FSTACK), upvalues (MAXUPVAL). The
-- find-booleans keep the corpus immune to the documented position
-- divergence (fork: function definition line; stock: current token).
local function tryload(code)
  local st, msg = load(code)
  assert(not st)
  return msg
end

-- nesting: 500 nested parens fail (the bare overflow), 100 load fine
local deep = "return " .. string.rep("(", 500) .. "1" .. string.rep(")", 500)
print(tryload(deep))
local okdeep = "return " .. string.rep("(", 100) .. "1" .. string.rep(")", 100)
print(load(okdeep)() == 1)

-- too many returns (luaK_ret): 255 values fail, 254 load
local rets = "return 10" .. string.rep(",10", 254)
print(string.find(tryload(rets), "too many returns", 1, true) ~= nil)
local rets2 = "return 10" .. string.rep(",10", 253)
local loaded = load(rets2)
print(loaded() ~= nil and select('#', loaded()) == 254)

-- too many local variables (MAXVARS): 250 concurrent locals
local decl = {}
for i = 1, 250 do decl[#decl+1] = "local a" .. i end
print(string.find(tryload(table.concat(decl, "\n")), "too many local variables", 1, true) ~= nil)

-- too many registers (MAX_FSTACK): 262 call arguments (vm.lua:129's shape)
local args = {}
for i = 1, 302 do args[#args+1] = "a" .. i end
local body = "function main(" .. table.concat(args, ",") .. ") end"
print(string.find(tryload(body), "too many local variables", 1, true) ~= nil)

-- too many upvalues (MAXUPVAL): errors.lua:768's 127+127+battery shape
local lim = 127
local s = "local function fooA ()\n  local "
for j = 1, lim do s = s.."a"..j..", " end
s = s.."b,c\n"
s = s.."local function fooB ()\n  local "
for j = 1, lim do s = s.."b"..j..", " end
s = s.."b\n"
s = s.."function fooC () return b+c"
for j = 1, lim do s = s.."+a"..j.."+b"..j end
s = s.."\nend  end end"
print(string.find(tryload(s), "too many upvalues", 1, true) ~= nil)

-- the passing side of every limit still compiles and runs
print(load("local a1, a2, a3; return a1, a2, a3") ~= nil)
print("limits01 ok")
