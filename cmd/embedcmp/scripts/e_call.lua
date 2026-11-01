-- e_call: hostfn round-trip (redis.call shape) — bench.call takes a
-- string down and returns its byte-length integer (same contract on
-- both engines: a C closure in embedbench55.c, RegisterGlobal in
-- cmd/embedcmp). 200 seam crossings per run.
local total = 0
for i = 1, 200 do
  total = total + bench.call(KEYS[1] .. ":" .. i)
end
return string.format("%d", total)
