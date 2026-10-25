-- warm55: steady-state reruns of a chunk inside ONE native Lua state —
-- cmd/perfcmp's native-C warm posture (native Lua has no in-process
-- pooling surface, so the closest steady-state analog is loadfile+call
-- reruns, matching the interp leg's fresh-state-per-run and keeping the
-- parse in the timed region on both sides).
--
-- Usage: lua warm55.lua <script.lua> <iters>
-- Prints: ITER <ns> per timed iteration (os.clock is CPU time; these
-- benches are CPU-bound), then RET <return values> from the last run and
-- HEAPKB <collectgarbage "count"> at exit.
local path, n = ...
n = assert(tonumber(n))

for _ = 1, 2 do -- warmup
  local f = assert(loadfile(path))
  f()
end

local t = {}
for i = 1, n do
  local t0 = os.clock()
  local f = assert(loadfile(path))
  local r = table.pack(f())
  t[i] = (os.clock() - t0) * 1e9
  if i == n then
    local vals = {}
    for k = 1, r.n do
      vals[k] = tostring(r[k])
    end
    -- newlines escaped so the harness's line parser sees one RET line
    io.write("RET ", (table.concat(vals, " "):gsub("\n", "\\n")), "\n")
  end
end
for i = 1, n do
  io.write(string.format("ITER %d\n", math.floor(t[i] + 0.5)))
end
io.write(string.format("HEAPKB %.1f\n", collectgarbage("count")))
