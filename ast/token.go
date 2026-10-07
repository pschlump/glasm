package ast

import (
	"fmt"
)

type Position struct {
	Source string
	Line   int
	Column int
}

type Token struct {
	Type int
	Name string
	Str  string
	Pos  Position
	// Raw is the string token's exact SOURCE text (delimiters included,
	// untranslated), stock's scanner buffer — txtToken renders it in
	// "near '<token>'" so `[[a]]` reports near '[[a]]', not the decoded
	// value (U5f; errors.lua's checksyntax battery). Empty for non-string
	// tokens, whose Str is already the raw text.
	Raw string
}

func (self *Token) String() string {
	return fmt.Sprintf("<type:%v, str:%v>", self.Name, self.Str)
}
