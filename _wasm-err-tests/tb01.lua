-- U4d: a failing __close replaces the in-flight error (uncaught shape).
local c <close> = setmetatable({}, {__close = function() error('close-bad') end})
error('orig')
