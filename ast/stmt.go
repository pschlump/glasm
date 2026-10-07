package ast

type Stmt interface {
	PositionHolder
	stmtMarker()
}

type StmtBase struct {
	Node
}

func (stmt *StmtBase) stmtMarker() {}

type AssignStmt struct {
	StmtBase

	Lhs []Expr
	Rhs []Expr
}

type LocalAssignStmt struct {
	StmtBase

	Names []string
	// Attribs carries the 5.4 variable attributes (U4): either nil (no
	// attribute anywhere in the statement) or parallel to Names, with ""
	// for plain names, "const", "close". The list-wide prefix attribute is
	// already applied at parse (a postfix attribute overrides it per name).
	Attribs []string
	Exprs   []Expr
}

type FuncCallStmt struct {
	StmtBase

	Expr Expr
}

// GlobalDeclStmt is a 5.5 global declaration (U5a): `global a`,
// `global a <const> = expr`, `global a, b <close>` (close rejected at
// parse), or the collective `global *` / `global <const> *` (IsStar,
// Names empty). Attribs parallels Names when any attribute appears.
type GlobalDeclStmt struct {
	StmtBase

	Names   []string
	Attribs []string
	Exprs   []Expr
	IsStar  bool
}

// GlobalFuncStmt is `global function f() end` (U5a): the name declares
// before the body compiles (body recursion sees it), the ERRNNIL
// redefinition check and store follow the body.
type GlobalFuncStmt struct {
	StmtBase

	Name string
	Func *FunctionExpr
}

type DoBlockStmt struct {
	StmtBase

	Stmts []Stmt
}

type WhileStmt struct {
	StmtBase

	Condition Expr
	Stmts     []Stmt
	// DoLine is the `do` token's line (U5f): stock codes the loop-back
	// jump at ls->lastline — the body's last token's line, or the `do`
	// line when the body is empty (the last token consumed by then).
	DoLine int
}

type RepeatStmt struct {
	StmtBase

	Condition Expr
	Stmts     []Stmt
}

type IfStmt struct {
	StmtBase

	Condition Expr
	Then      []Stmt
	Else      []Stmt
	// ThenLine is the `then` token's line (U5f): stock codes the
	// jump-over-else at ls->lastline — the then-block's last token's
	// line, or the `then` line when the block is empty.
	ThenLine int
}

type NumberForStmt struct {
	StmtBase

	Name  string
	Init  Expr
	Limit Expr
	Step  Expr
	Stmts []Stmt
}

type GenericForStmt struct {
	StmtBase

	Names []string
	Exprs []Expr
	Stmts []Stmt
}

type FuncDefStmt struct {
	StmtBase

	Name *FuncName
	Func *FunctionExpr
}

type ReturnStmt struct {
	StmtBase

	Exprs []Expr
}

type BreakStmt struct {
	StmtBase
}

type LabelStmt struct {
	StmtBase

	Name string
}

type GotoStmt struct {
	StmtBase

	Label string
}
