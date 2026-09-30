-- U4d: two closers, both fail — the first failure is the outcome; the
-- second still runs (close order observable via print).
local c1 <close> = setmetatable({}, {__close = function() print('c1') error('e1') end})
local c2 <close> = setmetatable({}, {__close = function() print('c2') error('e2') end})
error('x')
