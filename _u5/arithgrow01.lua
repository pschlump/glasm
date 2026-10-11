-- arithgrow01: rt_arith/rt_len scratch-slot rebasing across a stack
-- growth inside the metamethod dispatch (row 102 — the U7d soak's
-- c0027655/c0076725/c0080533 family). arith_body and len_body held a
-- raw StkId across luaO_arith/luaV_objlen; a metamethod (or string
-- coercion) that grows — and moves — the Lua stack left the final
-- *dst = *s2v(res) read in the freed old stack, where the staged
-- (f, p1, p2) window's leftovers sat (row 31's law, missed on these
-- two bodies): the arith destination kept whatever the register held
-- before — the operand table, the coerced-first-operand float, the
-- un-negated numeral string. The deep vararg chains below push the
-- fresh state's initial stack past its first growth exactly at the
-- dispatch, which is why shallow call shapes never showed it.
print("arithgrow01")
local function f0(a)
  local mt0 = { __unm = function(a) return -7 end,
                __len = function(a) return 33 end,
                __newindex = function(t, k, v) rawset(t, k, v * 2) end,
                __lt = function(x, y) return false end }
  local t0 = setmetatable({false, "10"}, mt0)
  print((-t0), (function() t0.brand_new_q = 21 return t0.brand_new_q end)(), (t0 < t1))
  print(#t0)
  print((math.ceil(2.9) * "10"), v1, (7 < (-("0x10"))), (-("0x10")))
end
local function f1(a, b, ...)
  local function f2(a, b, ...)
    local function f3(a, b)
      for i0 = 7, 2, -1 do
        local v1 = f0((2 % 1.5))
      end
    end
    local r1, r2, r3 = f3(nil, math.fmod(-2.5, 3))
  end
  local r1, r2, r3 = f2(("-3" + 3), nil, 1, "s")
end
print(f1(2, nil, 1, "s"))
