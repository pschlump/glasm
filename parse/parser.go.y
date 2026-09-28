%{
package parse

import (
  "github.com/pschlump/golua55/ast"
)
%}
%type<stmts> chunk
%type<stmts> chunk1
%type<stmts> block
%type<stmt>  stat
%type<stmts> elseifs
%type<stmt>  laststat
%type<funcname> funcname
%type<funcname> funcname1
%type<exprlist> varlist
%type<expr> var
%type<namelist> namelist
%type<namelist> attnamelist
%type<attrib> attrib
%type<exprlist> exprlist
%type<expr> expr
%type<expr> string
%type<expr> prefixexp
%type<expr> functioncall
%type<expr> afunctioncall
%type<exprlist> args
%type<expr> function
%type<funcexpr> funcbody
%type<parlist> parlist
%type<expr> tableconstructor
%type<fieldlist> fieldlist
%type<field> field
%type<fieldsep> fieldsep

%union {
  token  ast.Token

  stmts    []ast.Stmt
  stmt     ast.Stmt

  funcname *ast.FuncName
  funcexpr *ast.FunctionExpr

  exprlist []ast.Expr
  expr   ast.Expr

  fieldlist []*ast.Field
  field     *ast.Field
  fieldsep  string

  namelist []string
  attrib   string
  parlist  *ast.ParList
}

/* Reserved words */
%token<token> TAnd TBreak TDo TElse TElseIf TEnd TFalse TFor TFunction TIf TIn TLocal TNil TNot TOr TReturn TRepeat TThen TTrue TUntil TWhile TGoto

/* Literals */
%token<token> TEqeq TNeq TLte TGte T2Comma T3Comma T2Colon TIdent TNumber TString '{' '(' TIDiv TBand TBor TBXor TShl TShr

/* U3a: `global` as a contextual keyword (reserved word with enforcement
   at U5); the lexer only emits TGlobal when the following token can
   continue a global declaration (name / function / '<' / '*'). */
%token<token> TGlobal

/* Operators — precedence follows stock lparser.c priority[] (U2b):
   | < ~ < & < shifts < .. < + - < * / // % < unary < ^ */
%left TOr
%left TAnd
%left '>' '<' TGte TLte TEqeq TNeq
%left TBor
%left TBXor
%left TBand
%left TShl TShr
%right T2Comma
%left '+' '-'
%left '*' '/' '%' TIDiv
%right UNARY /* not # -(unary) ~(unary) */
%right '^'

%%

chunk: 
        chunk1 {
            $$ = $1
            if l, ok := yylex.(*Lexer); ok {
                l.Stmts = $$
            }
        } |
        chunk1 laststat {
            $$ = append($1, $2)
            if l, ok := yylex.(*Lexer); ok {
                l.Stmts = $$
            }
        } | 
        chunk1 laststat ';' {
            $$ = append($1, $2)
            if l, ok := yylex.(*Lexer); ok {
                l.Stmts = $$
            }
        }

chunk1: 
        {
            $$ = []ast.Stmt{}
        } |
        chunk1 stat {
            $$ = append($1, $2)
        } | 
        chunk1 ';' {
            $$ = $1
        }

block: 
        chunk {
            $$ = $1
        }

stat:
        varlist '=' exprlist {
            $$ = &ast.AssignStmt{Lhs: $1, Rhs: $3}
            $$.SetLine($1[0].Line())
        } |
        /* 'stat = functioncal' causes a reduce/reduce conflict */
        prefixexp {
            if _, ok := $1.(*ast.FuncCallExpr); !ok {
               /* stock exprstat: "syntax error" at the token after the
                  non-call expression (5.5 lparser.c check_condition) */
               yylex.(*Lexer).SyntaxErrorAtNext()
            } else {
              $$ = &ast.FuncCallStmt{Expr: $1}
              $$.SetLine($1.Line())
            }
        } |
        TDo block TEnd {
            $$ = &ast.DoBlockStmt{Stmts: $2}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($3.Pos.Line)
        } |
        TWhile expr TDo block TEnd {
            $$ = &ast.WhileStmt{Condition: $2, Stmts: $4}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($5.Pos.Line)
        } |
        TRepeat block TUntil expr {
            $$ = &ast.RepeatStmt{Condition: $4, Stmts: $2}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($4.Line())
        } |
        TIf expr TThen block elseifs TEnd {
            $$ = &ast.IfStmt{Condition: $2, Then: $4}
            cur := $$
            for _, elseif := range $5 {
                cur.(*ast.IfStmt).Else = []ast.Stmt{elseif}
                cur = elseif
            }
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($6.Pos.Line)
        } |
        TIf expr TThen block elseifs TElse block TEnd {
            $$ = &ast.IfStmt{Condition: $2, Then: $4}
            cur := $$
            for _, elseif := range $5 {
                cur.(*ast.IfStmt).Else = []ast.Stmt{elseif}
                cur = elseif
            }
            cur.(*ast.IfStmt).Else = $7
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($8.Pos.Line)
        } |
        TFor TIdent '=' expr ',' expr TDo block TEnd {
            $$ = &ast.NumberForStmt{Name: $2.Str, Init: $4, Limit: $6, Stmts: $8}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($9.Pos.Line)
        } |
        TFor TIdent '=' expr ',' expr ',' expr TDo block TEnd {
            $$ = &ast.NumberForStmt{Name: $2.Str, Init: $4, Limit: $6, Step:$8, Stmts: $10}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($11.Pos.Line)
        } |
        TFor namelist TIn exprlist TDo block TEnd {
            $$ = &ast.GenericForStmt{Names:$2, Exprs:$4, Stmts: $6}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($7.Pos.Line)
        } |
        TFunction funcname funcbody {
            $$ = &ast.FuncDefStmt{Name: $2, Func: $3}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($3.LastLine())
        } |
        TLocal TFunction TIdent funcbody {
            $$ = &ast.LocalAssignStmt{Names:[]string{$3.Str}, Exprs: []ast.Expr{$4}}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($4.LastLine())
        } |
        /* U3a: attnamelist carries optional attributes (<const>/<close>)
           in either grammar position; they parse and are IGNORED — the
           real attribute semantics are U4 (ledger row). */
        TLocal attnamelist '=' exprlist {
            $$ = &ast.LocalAssignStmt{Names: $2, Exprs:$4}
            $$.SetLine($1.Pos.Line)
        } |
        TLocal attnamelist {
            $$ = &ast.LocalAssignStmt{Names: $2, Exprs:[]ast.Expr{}}
            $$.SetLine($1.Pos.Line)
        } |
        /* U3a: the 5.5 global declaration, accepted-as-no-op (a reserved
           word with enforcement at U5 — ledger row). `global namelist`
           lowers to a plain global assignment when initialized; the
           declaration itself carries no observable behavior before U5.
           `global *` / `global <attrib> *` change only the (absent)
           declaration checking — nothing. */
        TGlobal attnamelist '=' exprlist {
            lhs := make([]ast.Expr, 0, len($2))
            for i, name := range $2 {
                ident := &ast.IdentExpr{Value: name}
                ident.SetLine($1.Pos.Line + i)
                lhs = append(lhs, ident)
            }
            $$ = &ast.AssignStmt{Lhs: lhs, Rhs: $4}
            $$.SetLine($1.Pos.Line)
        } |
        TGlobal attnamelist {
            $$ = &ast.DoBlockStmt{}
            $$.SetLine($1.Pos.Line)
        } |
        TGlobal TFunction TIdent funcbody {
            fn := &ast.IdentExpr{Value: $3.Str}
            fn.SetLine($3.Pos.Line)
            $$ = &ast.FuncDefStmt{Name: &ast.FuncName{Func: fn}, Func: $4}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($4.LastLine())
        } |
        TGlobal '*' {
            $$ = &ast.DoBlockStmt{}
            $$.SetLine($1.Pos.Line)
        } |
        TGlobal attrib '*' {
            $$ = &ast.DoBlockStmt{}
            $$.SetLine($1.Pos.Line)
        } |
        T2Colon TIdent T2Colon {
            $$ = &ast.LabelStmt{Name: $2.Str}
            $$.SetLine($1.Pos.Line)
        } |
        TGoto TIdent {
            $$ = &ast.GotoStmt{Label: $2.Str}
            $$.SetLine($1.Pos.Line)
        } |
        /* 5.2: break is a regular statement, no longer restricted to the
           end of a block (it lowers as a jump to the loop's end, with
           OP_CLOSE for any local scope it exits — compileBreakStmt) */
        TBreak {
            $$ = &ast.BreakStmt{}
            $$.SetLine($1.Pos.Line)
        }

elseifs: 
        {
            $$ = []ast.Stmt{}
        } | 
        elseifs TElseIf expr TThen block {
            $$ = append($1, &ast.IfStmt{Condition: $3, Then: $5})
            $$[len($$)-1].SetLine($2.Pos.Line)
        }

laststat:
        TReturn {
            $$ = &ast.ReturnStmt{Exprs:nil}
            $$.SetLine($1.Pos.Line)
        } |
        TReturn exprlist {
            $$ = &ast.ReturnStmt{Exprs:$2}
            $$.SetLine($1.Pos.Line)
        }

funcname: 
        funcname1 {
            $$ = $1
        } |
        funcname1 ':' TIdent {
            $$ = &ast.FuncName{Func:nil, Receiver:$1.Func, Method: $3.Str}
        }

funcname1:
        TIdent {
            $$ = &ast.FuncName{Func: &ast.IdentExpr{Value:$1.Str}}
            $$.Func.SetLine($1.Pos.Line)
        } | 
        funcname1 '.' TIdent {
            key:= &ast.StringExpr{Value:$3.Str}
            key.SetLine($3.Pos.Line)
            fn := &ast.AttrGetExpr{Object: $1.Func, Key: key}
            fn.SetLine($3.Pos.Line)
            $$ = &ast.FuncName{Func: fn}
        }

varlist:
        var {
            $$ = []ast.Expr{$1}
        } | 
        varlist ',' var {
            $$ = append($1, $3)
        }

var:
        TIdent {
            $$ = &ast.IdentExpr{Value:$1.Str}
            $$.SetLine($1.Pos.Line)
        } |
        prefixexp '[' expr ']' {
            $$ = &ast.AttrGetExpr{Object: $1, Key: $3}
            $$.SetLine($1.Line())
        } | 
        prefixexp '.' TIdent {
            key := &ast.StringExpr{Value:$3.Str}
            key.SetLine($3.Pos.Line)
            $$ = &ast.AttrGetExpr{Object: $1, Key: key}
            $$.SetLine($1.Line())
        }

namelist:
        TIdent {
            $$ = []string{$1.Str}
        } |
        namelist ','  TIdent {
            $$ = append($1, $3.Str)
        }

/* U3a: the 5.5 attnamelist — an attribute may prefix the first name
   (list-wide in stock's localstat: `local <const> a, b` makes both const)
   or follow any name. The attribute text parses and is discarded; real
   <const>/<close> semantics are U4. */
attnamelist:
        TIdent {
            $$ = []string{$1.Str}
        } |
        TIdent attrib {
            $$ = []string{$1.Str}
        } |
        attrib TIdent {
            $$ = []string{$2.Str}
        } |
        attrib TIdent attrib {
            $$ = []string{$2.Str}
        } |
        attnamelist ',' TIdent {
            $$ = append($1, $3.Str)
        } |
        attnamelist ',' TIdent attrib {
            $$ = append($1, $3.Str)
        }

attrib:
        '<' TIdent '>' {
            $$ = $2.Str
        }

exprlist:
        expr {
            $$ = []ast.Expr{$1}
        } |
        exprlist ',' expr {
            $$ = append($1, $3)
        }

expr:
        TNil {
            $$ = &ast.NilExpr{}
            $$.SetLine($1.Pos.Line)
        } | 
        TFalse {
            $$ = &ast.FalseExpr{}
            $$.SetLine($1.Pos.Line)
        } | 
        TTrue {
            $$ = &ast.TrueExpr{}
            $$.SetLine($1.Pos.Line)
        } | 
        TNumber {
            $$ = &ast.NumberExpr{Value: $1.Str}
            $$.SetLine($1.Pos.Line)
        } | 
        T3Comma {
            $$ = &ast.Comma3Expr{}
            $$.SetLine($1.Pos.Line)
        } |
        function {
            $$ = $1
        } | 
        prefixexp {
            $$ = $1
        } |
        string {
            $$ = $1
        } |
        tableconstructor {
            $$ = $1
        } |
        expr TOr expr {
            $$ = &ast.LogicalOpExpr{Lhs: $1, Operator: "or", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TAnd expr {
            $$ = &ast.LogicalOpExpr{Lhs: $1, Operator: "and", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '>' expr {
            $$ = &ast.RelationalOpExpr{Lhs: $1, Operator: ">", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '<' expr {
            $$ = &ast.RelationalOpExpr{Lhs: $1, Operator: "<", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TGte expr {
            $$ = &ast.RelationalOpExpr{Lhs: $1, Operator: ">=", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TLte expr {
            $$ = &ast.RelationalOpExpr{Lhs: $1, Operator: "<=", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TEqeq expr {
            $$ = &ast.RelationalOpExpr{Lhs: $1, Operator: "==", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TNeq expr {
            $$ = &ast.RelationalOpExpr{Lhs: $1, Operator: "~=", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr T2Comma expr {
            $$ = &ast.StringConcatOpExpr{Lhs: $1, Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '+' expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "+", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '-' expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "-", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '*' expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "*", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '/' expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "/", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TIDiv expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "//", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TBand expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "&", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TBor expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "|", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TBXor expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "~", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TShl expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "<<", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr TShr expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: ">>", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '%' expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "%", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        expr '^' expr {
            $$ = &ast.ArithmeticOpExpr{Lhs: $1, Operator: "^", Rhs: $3}
            $$.SetLine($1.Line())
        } |
        '-' expr %prec UNARY {
            $$ = &ast.UnaryMinusOpExpr{Expr: $2}
            $$.SetLine($2.Line())
        } |
        TBXor expr %prec UNARY {
            $$ = &ast.UnaryBNotOpExpr{Expr: $2}
            $$.SetLine($2.Line())
        } |
        TNot expr %prec UNARY {
            $$ = &ast.UnaryNotOpExpr{Expr: $2}
            $$.SetLine($2.Line())
        } |
        '#' expr %prec UNARY {
            $$ = &ast.UnaryLenOpExpr{Expr: $2}
            $$.SetLine($2.Line())
        }

string: 
        TString {
            $$ = &ast.StringExpr{Value: $1.Str}
            $$.SetLine($1.Pos.Line)
        } 

prefixexp:
        var {
            $$ = $1
        } |
        afunctioncall {
            $$ = $1
        } |
        functioncall {
            $$ = $1
        } |
        '(' expr ')' {
            if ex, ok := $2.(*ast.Comma3Expr); ok {
                ex.AdjustRet = true
            }
            $$ = $2
            $$.SetLine($1.Pos.Line)
        }

afunctioncall:
        '(' functioncall ')' {
            $2.(*ast.FuncCallExpr).AdjustRet = true
            $$ = $2
        }

functioncall:
        prefixexp args {
            $$ = &ast.FuncCallExpr{Func: $1, Args: $2}
            $$.SetLine($1.Line())
        } |
        prefixexp ':' TIdent args {
            $$ = &ast.FuncCallExpr{Method: $3.Str, Receiver: $1, Args: $4}
            $$.SetLine($1.Line())
        }

args:
        '(' ')' {
            if yylex.(*Lexer).PNewLine {
               yylex.(*Lexer).TokenError($1, "ambiguous syntax (function call x new statement)")
            }
            $$ = []ast.Expr{}
        } |
        '(' exprlist ')' {
            if yylex.(*Lexer).PNewLine {
               yylex.(*Lexer).TokenError($1, "ambiguous syntax (function call x new statement)")
            }
            $$ = $2
        } |
        tableconstructor {
            $$ = []ast.Expr{$1}
        } | 
        string {
            $$ = []ast.Expr{$1}
        }

function:
        TFunction funcbody {
            $$ = &ast.FunctionExpr{ParList:$2.ParList, Stmts: $2.Stmts}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($2.LastLine())
        }

funcbody:
        '(' parlist ')' block TEnd {
            // U3a: a named vararg param (5.5 `...t`) binds a placeholder
            // local for the name so dead-branch bodies referencing it
            // compile; the real vararg-table semantics are U5.
            if name := $2.VArgName; name != "" {
                la := &ast.LocalAssignStmt{Names: []string{name}, Exprs: []ast.Expr{&ast.TableExpr{Fields: []*ast.Field{}}}}
                la.SetLine($1.Pos.Line)
                $4 = append([]ast.Stmt{la}, $4...)
            }
            $$ = &ast.FunctionExpr{ParList: $2, Stmts: $4}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($5.Pos.Line)
        } |
        '(' ')' block TEnd {
            $$ = &ast.FunctionExpr{ParList: &ast.ParList{HasVargs: false, Names: []string{}}, Stmts: $3}
            $$.SetLine($1.Pos.Line)
            $$.SetLastLine($4.Pos.Line)
        }

parlist:
        T3Comma {
            $$ = &ast.ParList{HasVargs: true, Names: []string{}}
        } |
        T3Comma TIdent {
            $$ = &ast.ParList{HasVargs: true, Names: []string{}, VArgName: $2.Str}
        } |
        namelist {
          $$ = &ast.ParList{HasVargs: false, Names: []string{}}
          $$.Names = append($$.Names, $1...)
        } |
        namelist ',' T3Comma {
          $$ = &ast.ParList{HasVargs: true, Names: []string{}}
          $$.Names = append($$.Names, $1...)
        } |
        namelist ',' T3Comma TIdent {
          $$ = &ast.ParList{HasVargs: true, Names: []string{}}
          $$.Names = append($$.Names, $1...)
          $$.VArgName = $4.Str
        }


tableconstructor:
        '{' '}' {
            $$ = &ast.TableExpr{Fields: []*ast.Field{}}
            $$.SetLine($1.Pos.Line)
        } |
        '{' fieldlist '}' {
            $$ = &ast.TableExpr{Fields: $2}
            $$.SetLine($1.Pos.Line)
        }


fieldlist:
        field {
            $$ = []*ast.Field{$1}
        } | 
        fieldlist fieldsep field {
            $$ = append($1, $3)
        } | 
        fieldlist fieldsep {
            $$ = $1
        }

field:
        TIdent '=' expr {
            $$ = &ast.Field{Key: &ast.StringExpr{Value:$1.Str}, Value: $3}
            $$.Key.SetLine($1.Pos.Line)
        } | 
        '[' expr ']' '=' expr {
            $$ = &ast.Field{Key: $2, Value: $5}
        } |
        expr {
            $$ = &ast.Field{Value: $1}
        }

fieldsep:
        ',' {
            $$ = ","
        } | 
        ';' {
            $$ = ";"
        }

%%

func TokenName(c int) string {
	if c >= TAnd && c-TAnd < len(yyToknames) {
		if yyToknames[c-TAnd] != "" {
			return yyToknames[c-TAnd]
		}
	}
    return string([]byte{byte(c)})
}

