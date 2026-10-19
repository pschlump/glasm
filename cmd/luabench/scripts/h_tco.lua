-- h_tco: tail-recursive countdown, 200k iterations — the M5c staged
-- tailcall lane (rt_tail_stage restages at the adapter level, O(1)
-- wasm stack). Pairs with h_callret: the same per-call seam minus the
-- caller-frame push/pop, and the callee-discrimination machinery the
-- U9 direct-dispatch design builds on.
local function loop(n, acc)
  if n == 0 then
    return acc
  end
  return loop(n - 1, acc + n)
end
return string.format("%d", loop(200000, 0))
