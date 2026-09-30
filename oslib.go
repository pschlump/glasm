package lua

import (
	"os"
	"strings"
	"syscall"
	"time"
)

var startedAt time.Time

func init() {
	startedAt = time.Now()
}

func getIntField(L *LState, tb *LTable, key string, v int) int {
	ret := tb.RawGetString(key)

	switch lv := ret.(type) {
	case LInteger:
		return int(lv)
	case LNumber:
		return int(lv)
	case LString:
		slv := string(lv)
		slv = strings.TrimLeft(slv, " ")
		if strings.HasPrefix(slv, "0") && !strings.HasPrefix(slv, "0x") && !strings.HasPrefix(slv, "0X") {
			// Standard lua interpreter only support decimal and hexadecimal
			slv = strings.TrimLeft(slv, "0")
			if slv == "" {
				return 0
			}
		}
		if num, err := parseNumber(slv); err == nil {
			return int(LVAsNumber(num))
		}
	default:
		return v
	}

	return v
}

func getBoolField(L *LState, tb *LTable, key string, v bool) bool {
	ret := tb.RawGetString(key)
	if lb, ok := ret.(LBool); ok {
		return bool(lb)
	}
	return v
}

func OpenOs(L *LState) int {
	osmod := L.RegisterModule(OsLibName, osFuncs)
	L.Push(osmod)
	return 1
}

var osFuncs = map[string]LGFunction{
	"clock":     osClock,
	"difftime":  osDiffTime,
	"execute":   osExecute,
	"exit":      osExit,
	"date":      osDate,
	"getenv":    osGetEnv,
	"remove":    osRemove,
	"rename":    osRename,
	"setenv":    osSetEnv,
	"setlocale": osSetLocale,
	"time":      osTime,
	"tmpname":   osTmpname,
}

func osClock(L *LState) int {
	L.Push(LNumber(float64(time.Now().Sub(startedAt)) / float64(time.Second)))
	return 1
}

func osDiffTime(L *LState) int {
	L.Push(LNumber(L.CheckInt64(1) - L.CheckInt64(2)))
	return 1
}

// osExecute implements the 5.2 return contract: on success
// (true, "exit", 0); on failure (nil, "exit"|"signal", status). (5.1
// returned just the exit number; the nil becomes false in 5.3+.)
func osExecute(L *LState) int {
	var procAttr os.ProcAttr
	procAttr.Files = []*os.File{os.Stdin, os.Stdout, os.Stderr}
	cmd, args := popenArgs(L.CheckString(1))
	args = append([]string{cmd}, args...)
	process, err := os.StartProcess(cmd, args, &procAttr)
	if err != nil {
		L.Push(LNil)
		L.Push(LString("exit"))
		L.Push(LNumber(1))
		return 3
	}

	ps, err := process.Wait()
	if err != nil || !ps.Success() {
		reason := "exit"
		code := 1
		if err == nil {
			if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
				reason = "signal"
				code = int(ws.Signal())
			} else {
				code = ps.ExitCode()
			}
		}
		L.Push(LNil)
		L.Push(LString(reason))
		L.Push(LNumber(code))
		return 3
	}
	L.Push(LTrue)
	L.Push(LString("exit"))
	L.Push(LNumber(0))
	return 3
}

// osExit ports stock os_exit (5.4's two-argument shape): a boolean first
// argument maps true→0 / false→1, anything else is the exit status
// (default 0); a truthy second argument runs a full state close (every
// live main-thread tbc/upvalue close, U4d) before exiting.
func osExit(L *LState) int {
	status := 0
	if b, ok := L.Get(1).(LBool); ok {
		if !bool(b) {
			status = 1
		}
	} else {
		status = L.OptInt(1, 0)
	}
	if !LVIsFalse(L.Get(2)) {
		L.Close()
	}
	os.Exit(status)
	return 0
}

// setDateFields mirrors stock loslib.c setallfields: the os.date("*t")
// table fields as integers, with yday/wday resolved and isdst a boolean.
func setDateFields(ret *LTable, t time.Time) {
	ret.RawSetString("year", LInteger(t.Year()))
	ret.RawSetString("month", LInteger(int(t.Month())))
	ret.RawSetString("day", LInteger(t.Day()))
	ret.RawSetString("hour", LInteger(t.Hour()))
	ret.RawSetString("min", LInteger(t.Minute()))
	ret.RawSetString("sec", LInteger(t.Second()))
	ret.RawSetString("yday", LInteger(t.YearDay()))
	ret.RawSetString("wday", LInteger(int(t.Weekday())+1))
	ret.RawSetString("isdst", LBool(t.IsDST()))
}

func osDate(L *LState) int {
	t := time.Now()
	isUTC := false
	cfmt := "%c"
	if L.GetTop() >= 1 {
		cfmt = L.CheckString(1)
		if strings.HasPrefix(cfmt, "!") {
			cfmt = strings.TrimLeft(cfmt, "!")
			isUTC = true
		}
		if L.GetTop() >= 2 {
			t = time.Unix(L.CheckInt64(2), 0)
		}
		if isUTC {
			t = t.UTC()
		}
		if strings.HasPrefix(cfmt, "*t") {
			// stock 5.5 setallfields: every field an integer (U3 tail),
			// real yday/wday, isdst from the time value.
			ret := L.NewTable()
			setDateFields(ret, t)
			L.Push(ret)
			return 1
		}
	}
	L.Push(LString(strftime(t, cfmt)))
	return 1
}

func osGetEnv(L *LState) int {
	v := os.Getenv(L.CheckString(1))
	if len(v) == 0 {
		L.Push(LNil)
	} else {
		L.Push(LString(v))
	}
	return 1
}

func osRemove(L *LState) int {
	err := os.Remove(L.CheckString(1))
	if err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	} else {
		L.Push(LTrue)
		return 1
	}
}

func osRename(L *LState) int {
	err := os.Rename(L.CheckString(1), L.CheckString(2))
	if err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	} else {
		L.Push(LTrue)
		return 1
	}
}

func osSetLocale(L *LState) int {
	// setlocale is not supported
	L.Push(LFalse)
	return 1
}

func osSetEnv(L *LState) int {
	err := os.Setenv(L.CheckString(1), L.CheckString(2))
	if err != nil {
		L.Push(LNil)
		L.Push(LString(err.Error()))
		return 2
	} else {
		L.Push(LTrue)
		return 1
	}
}

func osTime(L *LState) int {
	if L.GetTop() == 0 {
		L.Push(LInteger(time.Now().Unix()))
	} else {
		lv := L.CheckAny(1)
		if lv == LNil {
			L.Push(LInteger(time.Now().Unix()))
		} else {
			tbl, ok := lv.(*LTable)
			if !ok {
				L.TypeError(1, LTTable)
			}
			sec := getIntField(L, tbl, "sec", 0)
			min := getIntField(L, tbl, "min", 0)
			hour := getIntField(L, tbl, "hour", 12)
			day := getIntField(L, tbl, "day", -1)
			month := getIntField(L, tbl, "month", -1)
			year := getIntField(L, tbl, "year", -1)
			isdst := getBoolField(L, tbl, "isdst", false)
			_ = isdst // stock lets isdst hint mktime; Go has no such hint
			t := time.Date(year, time.Month(month), day, hour, min, sec, 0, time.Local)
			// stock os.time normalizes the table (setallfields writeback)
			setDateFields(tbl, t)
			L.Push(LInteger(t.Unix()))
		}
	}
	return 1
}

func osTmpname(L *LState) int {
	file, err := os.CreateTemp("", "")
	if err != nil {
		L.RaiseError("unable to generate a unique filename")
	}
	file.Close()
	os.Remove(file.Name()) // ignore errors
	L.Push(LString(file.Name()))
	return 1
}

//
