-- load forms (5.2): string chunk, reader function, chunkname defaults
print(load("return 7")())
print((load("return 8", "=named"))())
local f1, m1 = load("return +")
print(f1, (m1:gsub("^.*:%d+: ", "")))
local i = 0
local function reader()
  i = i + 1
  if i == 1 then return "return " end
  if i == 2 then return "'streamed'" end
end
print(load(reader)())
print(load("return ...", "=va", "t", nil)("A", "B"))
-- a failed load leaves no globals behind
load("syntax error here =", "=x")
print(_G.syntax == nil)
-- loadfile with modes over a real file
local wf = assert(io.open("u1-load.tmp", "w")); wf:write("return 'file-chunk'\n"); wf:close()
local okfn = loadfile("u1-load.tmp", "t")
print(okfn and okfn())
print(select(2, loadfile("u1-load.tmp", "b")))
print(select(1, loadfile("u1-nonexistent.tmp")))
os.remove("u1-load.tmp")
