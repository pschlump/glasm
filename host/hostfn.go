package host

// The host side of the four trampoline kinds the blob calls. All of them
// fire on the goroutine that holds vm.mu (the running script's own
// goroutine — the A9 law), which is what makes redis.call deadlock-free:
// the daemon's bridge runs synchronously inside the guest's call frame.

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// ValueError lets a HostFunc raise a non-string Lua error value — e.g. a
// table {err="..."}, the error class redis.call raises in Redis (the
// script's pcall then sees a table and the daemon renders the text
// verbatim, instead of the error gaining an engine-added prefix).
type ValueError struct{ V Value }

func (e *ValueError) Error() string { return "host function error: " + e.V.String() }

// EVT_PRINT is the event kind g_print emits (runtime/luawasm55.c — the
// 5.5 driver; same tag-first wire as the 5.1 one, plus PT_INTEGER).
const EVT_PRINT = 1

// hostEvent receives the guest's event stream (print). The payload is
// one typed frame: u32 count + count wire values.
func (vm *VM) hostEvent(kind, ptr, length int32) {
	if kind != EVT_PRINT || length <= 0 || vm.eventSink == nil {
		return
	}
	raw, ok := vm.mem.Read(uint32(ptr), uint32(length))
	if !ok {
		return
	}
	args, err := decodeArgs(raw)
	if err != nil {
		return // a malformed print frame is dropped, never fatal
	}
	vm.eventSink(vm, args)
}

// hostDispatch is the M5a re-entrancy seam: the C precall adapter
// dispatches a compiled proto through the host into the script module's
// lua_dispatch — a nested wasm Call on this same goroutine. A trap inside
// the dispatch must not unwind through wasm: surface as a refused
// dispatch (-3); the adapter re-raises it as a Lua error. A
// stack-exhaustion trap maps to RTW_STACKTRAP (-4) instead — the blob
// stages its clean catchable "stack overflow" (U5e, row 20 family).
func (vm *VM) hostDispatch(idx, frame, cl, nargs, want int32) int32 {
	if vm.scriptInst == nil {
		return -3
	}
	fn := vm.scriptInst.ExportedFunction("lua_dispatch")
	if fn == nil {
		return -3
	}
	res, err := fn.Call(vm.runCtx(), u32v(idx), u32v(frame), u32v(cl), u32v(nargs), u32v(want))
	if err != nil {
		if strings.Contains(err.Error(), "call stack exhausted") {
			return -4
		}
		return -3
	}
	return int32(res[0])
}

// runCtx is the in-flight Run's context (nested wasm calls ride it so a
// cancelled parent tears dispatches down with it); Background between
// runs.
func (vm *VM) runCtx() context.Context {
	if vm.curCtx != nil {
		return vm.curCtx
	}
	return context.Background()
}

// hostCall is the Lua→Go function seam (M7a): decode the argument frame,
// run the registered HostFunc, encode the result frame (or one error
// value + -1). A panicking HostFunc is contained here — the guest sees a
// clean Lua error, never a trap. Return framing mirrors the guest's
// g_hostfn decoder: u32 count + values.
func (vm *VM) hostCall(fnidx, argsPtr, argsLen, retPtr, retCap int32) int32 {
	fail := func(msg string) int32 {
		frame := encodeError(msg)
		if int64(len(frame)) <= int64(uint32(retCap)) {
			_ = vm.mem.Write(uint32(retPtr), frame)
		}
		return -1
	}
	if int(fnidx) < 0 || int(fnidx) >= len(vm.hostFns) {
		return fail("host function call: unknown function index")
	}
	if argsLen < 0 {
		return fail("host function call: bad argument frame")
	}
	raw, ok := vm.mem.Read(uint32(argsPtr), uint32(argsLen))
	if !ok {
		return fail("host function call: argument frame out of bounds")
	}
	args, err := decodeArgs(raw)
	if err != nil {
		return fail("host function call: " + err.Error())
	}

	hf := vm.hostFns[fnidx]
	var vals []Value
	func() {
		defer func() {
			if p := recover(); p != nil {
				err = fmt.Errorf("host function panic: %v", p)
				vals = nil
			}
		}()
		vals, err = hf.fn(vm, args)
	}()
	if err != nil {
		var ve *ValueError
		if errors.As(err, &ve) {
			if frame, e2 := encodeValue(nil, ve.V); e2 == nil &&
				int64(len(frame)) <= int64(uint32(retCap)) &&
				vm.mem.Write(uint32(retPtr), frame) {
				return -1
			}
		}
		return fail(err.Error())
	}
	frame, err := encodeResults(vals)
	if err != nil {
		return fail("host function call: " + err.Error())
	}
	if int64(len(frame)) > int64(uint32(retCap)) {
		return fail("host function call: results too large for the guest staging buffer")
	}
	if !vm.mem.Write(uint32(retPtr), frame) {
		return -1 // staging out of bounds: the guest falls back to its own error
	}
	return int32(len(frame))
}
