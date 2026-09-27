package lua

import (
	"testing"
)

// The ivABC k-flag paths (NEWTABLE/SETLIST, and LOADKX) read their
// extra operand from the FOLLOWING EXTRAARG word, exactly like stock
// lvm.c (GETARG_Ax(*pc)) — U2e found the interpreter decoding it from
// the instruction's own bits, which decodes the destination register
// as the index. These tests pin the fixed decode through observable
// behavior: constructors with more than MAXARG_vC (1023) array items
// exercise NEWTABLE k=1 and SETLIST k=1 + EXTRAARG end-to-end.
func TestExtraargBigConstructors(t *testing.T) {
	// >1023 positioned items: SETLIST flushes with k=1 + EXTRAARG, and
	// NEWTABLE presizes with the high array-size word
	src := "local t = {"
	for i := 1; i <= 2500; i++ {
		if i > 1 {
			src += ","
		}
		src += "2"
	}
	src += "} r = #t .. ' ' .. t[1] .. ' ' .. t[1024] .. ' ' .. t[2000] .. ' ' .. t[2500]"
	L := NewState()
	defer L.Close()
	if err := L.DoString(src); err != nil {
		t.Fatalf("dostring: %v", err)
	}
	if got := L.GetGlobal("r").String(); got != "2500 2 2 2 2" {
		t.Fatalf("big constructor: got %q", got)
	}
}

// A mixed constructor past the array threshold: hash part + array part
// with the k=1 flush boundary (vC wraps at 1024).
func TestExtraargConstructorBoundary(t *testing.T) {
	src := "local t = {"
	for i := 1; i <= 1024; i++ {
		if i > 1 {
			src += ","
		}
		src += "7"
	}
	src += ", x=9} r = #t .. ' ' .. t[1] .. ' ' .. t[1024] .. ' ' .. t.x"
	L := NewState()
	defer L.Close()
	if err := L.DoString(src); err != nil {
		t.Fatalf("dostring: %v", err)
	}
	if got := L.GetGlobal("r").String(); got != "1024 7 7 9" {
		t.Fatalf("boundary constructor: got %q", got)
	}
}
