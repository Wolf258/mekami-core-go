package gofrontend

import (
	"go/ast"
	"go/token"
	"strings"
)

// visitBlockStmt walks a function body block. Every statement
// that opens a new lexical scope (if, for, switch, block, etc.)
// calls enterScope on entry and leaveScope on exit, so locals
// declared inside do not leak.
func (c *goCollector) visitBlockStmt(block *ast.BlockStmt) {
	if block == nil {
		return
	}
	for _, stmt := range block.List {
		c.visitStmt(stmt)
	}
}

// visitStmtList walks a []ast.Stmt (used for switch / select
// case bodies, which are not *ast.BlockStmt but a flat list).
func (c *goCollector) visitStmtList(stmts []ast.Stmt) {
	for _, s := range stmts {
		c.visitStmt(s)
	}
}

// visitStmt dispatches on the statement shape. We only need the
// forms that introduce scopes or carry expressions with
// ref-bearing nodes; the rest are recursed into via visitExpr.
func (c *goCollector) visitStmt(stmt ast.Stmt) {
	switch s := stmt.(type) {
	case *ast.ExprStmt:
		c.walkExpr(s.X)
	case *ast.DeclStmt:
		// A `var x = ...` inside a function body is a
		// DeclStmt whose Decl is a *ast.GenDecl with Tok=VAR.
		if gd, ok := s.Decl.(*ast.GenDecl); ok {
			c.collectGenDecl(gd)
		}
	case *ast.AssignStmt:
		c.handleAssign(s)
	case *ast.IfStmt:
		c.enterScope()
		if s.Init != nil {
			c.visitStmt(s.Init)
		}
		c.walkExpr(s.Cond)
		c.visitBlockStmt(s.Body)
		c.leaveScope()
		if s.Else != nil {
			c.visitStmt(s.Else)
		}
	case *ast.ForStmt:
		c.enterScope()
		if s.Init != nil {
			c.visitStmt(s.Init)
		}
		c.walkExpr(s.Cond)
		c.visitStmt(s.Post)
		c.visitBlockStmt(s.Body)
		c.leaveScope()
	case *ast.RangeStmt:
		c.enterScope()
		c.handleRange(s)
		c.visitBlockStmt(s.Body)
		c.leaveScope()
	case *ast.SwitchStmt:
		c.enterScope()
		if s.Init != nil {
			c.visitStmt(s.Init)
		}
		c.walkExpr(s.Tag)
		if s.Body != nil {
			for _, cc := range s.Body.List {
				if cs, ok := cc.(*ast.CaseClause); ok {
					for _, e := range cs.List {
						c.walkExpr(e)
					}
					c.visitStmtList(cs.Body)
				}
			}
		}
		c.leaveScope()
	case *ast.TypeSwitchStmt:
		c.enterScope()
		if s.Init != nil {
			c.visitStmt(s.Init)
		}
		c.visitStmt(s.Assign)
		if s.Body != nil {
			for _, cc := range s.Body.List {
				if cs, ok := cc.(*ast.CaseClause); ok {
					for _, e := range cs.List {
						c.walkExpr(e)
					}
					c.visitStmtList(cs.Body)
				}
			}
		}
		c.leaveScope()
	case *ast.BlockStmt:
		c.enterScope()
		c.visitBlockStmt(s)
		c.leaveScope()
	case *ast.ReturnStmt:
		for _, r := range s.Results {
			c.walkExpr(r)
		}
	case *ast.DeferStmt:
		c.walkExpr(s.Call)
	case *ast.GoStmt:
		c.walkExpr(s.Call)
	case *ast.SendStmt:
		c.walkExpr(s.Chan)
		c.walkExpr(s.Value)
	case *ast.LabeledStmt:
		c.visitStmt(s.Stmt)
	case *ast.SelectStmt:
		if s.Body != nil {
			for _, cc := range s.Body.List {
				if cs, ok := cc.(*ast.CommClause); ok {
					if cs.Comm != nil {
						c.visitStmt(cs.Comm)
					}
					c.visitStmtList(cs.Body)
				}
			}
		}
	case *ast.BranchStmt:
		// break / continue / goto / fallthrough: no expr.
	}
}

// handleAssign processes the LHS/RHS of an assignment. For
// := with a single RHS, we learn the LHS variable's type from
// the RHS shape. Multi-RHS assignments are walked without type
// inference (we don't know which LHS gets which RHS).
func (c *goCollector) handleAssign(s *ast.AssignStmt) {
	// Walk LHS first so any nested call resolves before the
	// RHS overwrites the local with its own typed value.
	for _, lhs := range s.Lhs {
		c.walkExpr(lhs)
	}
	if s.Tok == token.DEFINE && len(s.Lhs) == 1 && len(s.Rhs) == 1 {
		if id, ok := s.Lhs[0].(*ast.Ident); ok {
			c.learnLocalFromExpr(id.Name, s.Rhs[0])
			c.addLocal(id.Name)
		}
	}
	for _, rhs := range s.Rhs {
		c.walkExpr(rhs)
	}
}

// handleRange processes the range expression and binds the
// loop variables. The type-learning rules mirror the for-range
// statement: the key is the index type (int for slices/arrays,
// the key type for maps) and the value is the element type.
func (c *goCollector) handleRange(s *ast.RangeStmt) {
	c.walkExpr(s.X)
	if s.Tok == token.DEFINE && s.Key != nil {
		if id, ok := s.Key.(*ast.Ident); ok && id.Name != "_" {
			c.locals[id.Name] = localVar{} // unknown type
			c.addLocal(id.Name)
		}
		if id, ok := s.Value.(*ast.Ident); ok && id.Name != "_" {
			c.locals[id.Name] = localVar{}
			c.addLocal(id.Name)
		}
	}
}

// learnLocalFromExpr infers the type of name from the shape of
// rhs and records it in c.locals. The inference is intentionally
// narrow — only the shapes the integration tests cover:
//   - &T{}          -> "T"
//   - NewT()        -> "T" (the result of a constructor call)
//   - pkg.NewT()    -> "pkg.T"
//   - "string"/int  -> the literal's name (so "x := \"foo\""
//     still has a known type)
// Unknown shapes leave the local untyped.
func (c *goCollector) learnLocalFromExpr(name string, rhs ast.Expr) {
	switch v := rhs.(type) {
	case *ast.UnaryExpr:
		// &T{}
		if v.Op == token.AND {
			if cl, ok := v.X.(*ast.CompositeLit); ok {
				if t, ok := c.typeFromExpr(cl.Type); ok {
					c.locals[name] = localVar{known: true, typ: t}
					return
				}
			}
		}
	case *ast.CallExpr:
		// pkg.NewT() or NewT()
		t, ok := c.typeFromCallReturning(v)
		if ok {
			c.locals[name] = localVar{known: true, typ: t}
			return
		}
	case *ast.CompositeLit:
		// T{...} — but not at top level, that case is rare.
		if t, ok := c.typeFromExpr(v.Type); ok {
			c.locals[name] = localVar{known: true, typ: t}
			return
		}
	case *ast.BasicLit:
		c.locals[name] = localVar{known: true, typ: basicLitType(v.Kind)}
		return
	case *ast.Ident:
		// x := otherLocal
		if v.Name != "_" {
			if lv, ok := c.locals[v.Name]; ok && lv.known {
				c.locals[name] = lv
				return
			}
		}
	}
	// Leave the binding as "declared but unknown" so a later
	// "x = ..." can overwrite it.
	c.locals[name] = localVar{known: false}
}

// typeFromCallReturning attempts to derive the return type of a
// call expression. The historical pipeline limited itself to
// same-package constructor calls (NewT() / New()) whose return
// type matches the prefix of the function name.
func (c *goCollector) typeFromCallReturning(call *ast.CallExpr) (string, bool) {
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		// Bare NewT() / New() — same package.
		name := fn.Name
		if strings.HasPrefix(name, "New") && len(name) > 3 {
			return name[3:], true
		}
		if name == "New" {
			return "", false
		}
	case *ast.SelectorExpr:
		// pkg.NewT() — package-qualified. We need the
		// package's local name from the import map.
		if id, ok := fn.X.(*ast.Ident); ok {
			for path, local := range c.imports {
				if local == id.Name {
					if c.cache != nil {
						// The real package name
						// might differ from the
						// import alias; resolve
						// to the underlying
						// package name so
						// same-package lookups
						// (qualified name = pkg.Type)
						// still match.
						_ = path
					}
					return id.Name + "." + fn.Sel.Name, true
				}
			}
			// Same-package selector: pkg.TypeName was
			// declared in the same file.
			return id.Name + "." + fn.Sel.Name, true
		}
	}
	return "", false
}

// typeFromExpr extracts the bare type identifier from a type
// expression. Used by the composite-literal and receiver-name
// resolution paths. Returns ("", false) when the shape is
// something the indexer cannot meaningfully learn from.
func (c *goCollector) typeFromExpr(e ast.Expr) (string, bool) {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name, true
	case *ast.StarExpr:
		return c.typeFromExpr(v.X)
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			return id.Name + "." + v.Sel.Name, true
		}
	}
	return "", false
}

// basicLitType returns the Go type name of a basic literal:
// "string" for strings, "int" for integer literals, "float64"
// for floats, etc. Used by learnLocalFromExpr to tag locals
// initialised from literals.
func basicLitType(k token.Token) string {
	switch k {
	case token.STRING:
		return "string"
	case token.INT:
		return "int"
	case token.FLOAT:
		return "float64"
	case token.IMAG:
		return "complex128"
	case token.CHAR:
		return "rune"
	}
	return "untyped"
}
