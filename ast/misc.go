package ast

type Field struct {
	Key   Expr
	Value Expr
}

type ParList struct {
	HasVargs bool
	Names    []string
	// VArgName is the 5.5 named-vararg form (`function f(...t)`) — the
	// vararg-table name. U3a parses it and binds a placeholder local; the
	// real vararg-table semantics are U5 (ledger row).
	VArgName string
}

type FuncName struct {
	Func     Expr
	Receiver Expr
	Method   string
}
