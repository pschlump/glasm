-- h_fib: recursive fib(30) — the call-seam worst case (1.6M Lua->Lua
-- calls; the README "fib(30)" row). Every OP_CALL in compiled code
-- crosses script -> rt (rt_call_addr) -> CClosure trampoline -> host
-- dispatch -> back into the module, so this lane tracks the per-call
-- seam cost the U9 direct-dispatch work targets. Returns the integer
-- (no print: the prod blob has no stdout) so the interp and wasm legs
-- output-check against each other.
local function fib(n)
  if n < 2 then
    return n
  end
  return fib(n - 1) + fib(n - 2)
end
return fib(30)
