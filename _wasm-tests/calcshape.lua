-- row 31: calc-shaped programs (recursive-descent parser with
-- forward-declared local parse_expr captured as an upvalue, table-based
-- parser state, pcall'd evaluation) die on the wasm backend in
-- layout-dependent modes: silent rc=0 with no output, memory-fault trap
-- at a garbage address, or an infinite hang before the first print.
-- Reduced from _cli-tests/calc.lua (args hardcoded; interp and clua
-- both print the six results).
-- calc.lua — a small expression calculator (recursive descent).
-- Usage: calc.lua 'expr'... | calc.lua -f file
-- Grammar (Lua precedence): + - * / % ^ ( ), unary +/-, right-assoc ^,
-- decimal and exponent-notation numbers. Results print as %.14g.

local function fail(msg)
    error(msg, 0)
end

local function newparser(s)
    local p = { s = s, i = 1 }
    return p
end

local function skipws(p)
    while p.i <= #p.s do
        local c = string.sub(p.s, p.i, p.i)
        if c == " " or c == "\t" or c == "\n" or c == "\r" then
            p.i = p.i + 1
        else
            break
        end
    end
end

local function peek(p)
    skipws(p)
    if p.i > #p.s then
        return ""
    end
    return string.sub(p.s, p.i, p.i)
end

local function take(p)
    local c = peek(p)
    p.i = p.i + 1
    return c
end

local function number(p)
    local start = p.i
    while p.i <= #p.s do
        local c = string.sub(p.s, p.i, p.i)
        local b = string.byte(c)
        if (b >= 48 and b <= 57) or c == "." then
            p.i = p.i + 1
        else
            break
        end
    end
    local mant = string.sub(p.s, start, p.i - 1)
    -- optional exponent. Applied by repeated *// rather than
    -- tonumber("1e2"): golua55's tonumber rejects exponent
    -- notation (divergence ledger row 25), C accepts it.
    local exp = 0
    local c = string.sub(p.s, p.i, p.i)
    if c == "e" or c == "E" then
        local save = p.i
        p.i = p.i + 1
        local neg = false
        local c2 = string.sub(p.s, p.i, p.i)
        if c2 == "+" then
            p.i = p.i + 1
        elseif c2 == "-" then
            neg = true
            p.i = p.i + 1
        end
        local any, ev = false, 0
        while p.i <= #p.s do
            local b = string.byte(string.sub(p.s, p.i, p.i))
            if b >= 48 and b <= 57 then
                ev = ev * 10 + (b - 48)
                any = true
                p.i = p.i + 1
            else
                break
            end
        end
        if not any then
            p.i = save -- not an exponent after all
        elseif neg then
            exp = -ev
        else
            exp = ev
        end
    end
    local v = tonumber(mant)
    if v == nil then
        fail("calc: bad number: '" .. mant .. "'")
    end
    while exp > 0 do
        v = v * 10
        exp = exp - 1
    end
    while exp < 0 do
        v = v / 10
        exp = exp + 1
    end
    return v
end

local parse_expr

local function parse_atom(p)
    local c = peek(p)
    if c == "" then
        fail("calc: unexpected end of input")
    end
    local b = string.byte(c)
    if c == "(" then
        take(p)
        local v = parse_expr(p)
        if take(p) ~= ")" then
            fail("calc: expected ')'")
        end
        return v
    end
    if b >= 48 and b <= 57 or c == "." then
        return number(p)
    end
    fail("calc: unexpected character '" .. c .. "'")
end

local function parse_factor(p)
    local c = peek(p)
    if c == "-" then
        take(p)
        return -parse_factor(p)
    end
    if c == "+" then
        take(p)
        return parse_factor(p)
    end
    local v = parse_atom(p)
    if peek(p) == "^" then
        take(p)
        local e = parse_factor(p) -- right-assoc, exponent may be signed
        if v == 0 and e < 0 then
            fail("calc: zero to a negative power")
        end
        return v ^ e
    end
    return v
end

parse_expr = function(p)
    local v = parse_factor(p)
    while true do
        local c = peek(p)
        if c == "+" then
            take(p)
            v = v + parse_factor(p)
        elseif c == "-" then
            take(p)
            v = v - parse_factor(p)
        elseif c == "*" then
            take(p)
            v = v * parse_factor(p)
        elseif c == "/" then
            take(p)
            local d = parse_factor(p)
            if d == 0 then
                fail("calc: division by zero")
            end
            v = v / d
        elseif c == "%" then
            take(p)
            local d = parse_factor(p)
            if d == 0 then
                fail("calc: modulo by zero")
            end
            v = v % d
        else
            break
        end
    end
    return v
end

local function evaluate(text)
    local p = newparser(text)
    local v = parse_expr(p)
    if peek(p) ~= "" then
        fail("calc: trailing junk at '" .. string.sub(p.s, p.i) .. "'")
    end
    if v ~= v or v == math.huge or v == -math.huge then
        fail("calc: result is not finite")
    end
    return string.format("%.14g", v)
end

local exprs = {"(3+4)*2", "2^0.5", "-(2)", "10 % 3", "1e2+0.5", "4"}
if false and arg[1] == "-f" then
    if arg[2] == nil then
        fail("calc: -f needs a file")
    end
    local f = io.open(arg[2], "r")
    if not f then
        print("calc: " .. arg[2] .. ": No such file or directory")
        return
    end
    for line in f:lines() do
        line = string.gsub(line, "%s+$", "")
        if #line > 0 and string.sub(line, 1, 1) ~= "#" then
            exprs[#exprs + 1] = line
        end
    end
    f:close()
else
    for i = 1, #arg do
        exprs[#exprs + 1] = arg[i]
    end
end

if #exprs == 0 then
    print("usage: calc.lua 'expr'... | calc.lua -f file")
end
for i = 1, #exprs do
    local ok, res = pcall(evaluate, exprs[i])
    if ok then
        print(exprs[i] .. " = " .. res)
    else
        print(exprs[i] .. " ! " .. tostring(res))
    end
end
