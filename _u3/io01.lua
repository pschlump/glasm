-- U3h: io read/lines 5.3/5.4 shapes (temp files in the corpus dir).
local path = "u3io.tmp"
local f = assert(io.open(path, "w"))
f:write("10 3.5 hello\nline2\n")
f:close()
local g = assert(io.open(path, "r"))
print(g:read("n"))
print(g:read("n"))
print(g:read("l"))
print(g:read("L"))
print(g:read("a") == "")
print(g:read(0) == "")
g:seek("set")
local a, b = g:read("l", "l")
print(a, b)
g:seek("set")
print(math.type(g:read("n")))
g:close()
local n = 0
for l in io.lines(path) do n = n + 1 end
print(n)
print(select('#', io.lines(path)))
g = assert(io.open(path, "r"))
local acc = {}
for l in g:lines() do acc[#acc+1] = (l ~= "" and "x" or "-") end
print(table.concat(acc))
g:close()
os.remove(path)
