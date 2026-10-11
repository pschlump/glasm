-- h_hostfn: the Redis-call shape (U7c) — many round trips through the
-- row-96 hostfn seam: a string argument down (wire encode), an integer
-- result back (wire decode). bench.call is registered by the harness on
-- both the interpreter leg (a Go LState function) and the host/ legs
-- (host.RegisterGlobal); the differential-engine leg skips this script.
local total = 0
for i = 1, 3000 do
  total = total + bench.call("key:" .. i)
end
return string.format("%d", total)
