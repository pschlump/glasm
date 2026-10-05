-- U5b: tailcalls across the vararg models — multret 'return f(...)'
-- forwarding in both directions, tailcalls INTO and OUT OF promoted
-- (table-model) functions, and the RETURN C=numparams+1 fixup.
local function sink(...)
  return select('#', ...), ...
end

-- hidden -> hidden: plain forwarding
local function h1(...) return sink(...) end
print(h1(1, 2, 3))
print(h1())

-- table -> hidden: '...' materializes from the table, then forwards
local function t1(a, ...t) return sink(...) end
print(t1(0, 1, 2, 3))
print(t1(0))

-- hidden -> table: the callee promotes (its own vararg is named)
local named = nil
local function t2(...u) named = u; return sink(...) end
local function h2(...) return t2(...) end
print(h2("a", "b"))
print(named.n, named[1])

-- table -> table
local function t3b(...u) return u.n, u[1] end
local function t3(a, ...t) t.n = 2; return t3b(...) end
print(t3(0, "x", "y", "z"))

-- tailcall with fixed args mixed around the multret
local function m1(a, ...) return a + select('#', ...) end
local function m2(...) return m1(10, ...) end
print(m2(1, 2, 3))
print(m2())

-- the official file's shape: unpack-fed tailcall with many extras
local call = function(f, args) return f(table.unpack(args, 1, args.n)) end
local a = {}
for i = 1, 12 do a[i] = i + 0.5 end
local function f(p, q, r, s, ...t)
  return p, q, select('#', ...), (...)
end
print(call(f, a))

-- tailcall out of a method with unnamed vararg (the k-bit regression
-- shape: a C=2 single-value use must NOT take the table arm)
local obj = {}
function obj:f(...) local more = {...}; return self[...] + #more end
obj[1] = 10; obj[2] = 20
print(obj:f(1, "ignored"))
print(obj:f(2))
print("done")
