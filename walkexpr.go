package gofrontend

import (
	"fmt"
	"go/ast"
	"strings"

	"github.com/Wolf258/mekami-api/api/v1"
)

// walkExpr is the main AST walker for expression-level nodes.
// Every ref-producing shape (call, selector, type assertion,
// composite literal, type conversion, etc.) is funnelled through
// here so the emitting logic stays in one place. The function
// recurses into sub-expressions when the shape warrants it.
func (c *goCollector) walkExpr(e ast.Expr) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *ast.Ident:
		// Bare identifier in expression position. If the name
		// is a known local whose type is "T" we know what
		// type-use ref to record. We never emit a call ref
		// here; bare identifiers in expression position are
		// values, not calls.
		if v.Name == "_" || v.Name == "" {
			return
		}
		if v.Name == c.pkgName {
			return
		}
		if lv, ok := c.locals[v.Name]; ok && lv.known {
			// The identifier is a local of a known type.
			// No ref needed here; the type-use ref is
			// emitted at the point a method is invoked on
			// the local (selector) or a value is assigned
			// to a typed slot.
			_ = lv
		}
	case *ast.CallExpr:
		c.handleCall(v)
	case *ast.SelectorExpr:
		c.handleSelector(v)
	case *ast.CompositeLit:
		c.handleCompositeLit(v)
	case *ast.TypeAssertExpr:
		c.walkExpr(v.X)
		if id, ok := v.Type.(*ast.Ident); ok {
			c.emitRef(api.RefTypeUse, c.pkgName+"."+id.Name, c.fset.Position(v.Type.Pos()).Line)
		} else {
			c.walkExpr(v.Type)
		}
	case *ast.StarExpr:
		c.walkExpr(v.X)
	case *ast.UnaryExpr:
		c.walkExpr(v.X)
	case *ast.BinaryExpr:
		c.walkExpr(v.X)
		c.walkExpr(v.Y)
	case *ast.IndexExpr:
		c.walkExpr(v.X)
		c.walkExpr(v.Index)
	case *ast.SliceExpr:
		c.walkExpr(v.X)
		c.walkExpr(v.Low)
		c.walkExpr(v.High)
		c.walkExpr(v.Max)
	case *ast.ArrayType:
		c.walkExpr(v.Len)
		c.walkExpr(v.Elt)
	case *ast.MapType:
		c.walkExpr(v.Key)
		c.walkExpr(v.Value)
	case *ast.ChanType:
		c.walkExpr(v.Value)
	case *ast.FuncLit:
		c.handleFuncLit(v)
	case *ast.ParenExpr:
		c.walkExpr(v.X)
	case *ast.InterfaceType:
		if v.Methods != nil {
			c.walkFieldList(v.Methods)
		}
	case *ast.StructType:
		if v.Fields != nil {
			c.walkFieldList(v.Fields)
		}
	}
}

// walkTypeExpr recurses into a type expression and emits a
// RefTypeUse edge for every named type encountered. Used for
// struct field types, function parameter types, return types,
// and var / const declarations.
func (c *goCollector) walkTypeExpr(e ast.Expr) {
	if e == nil {
		return
	}
	switch v := e.(type) {
	case *ast.Ident:
		if v.Name == "" || v.Name == "_" {
			return
		}
		c.emitRef(api.RefTypeUse, c.pkgName+"."+v.Name, c.fset.Position(v.Pos()).Line)
	case *ast.StarExpr:
		c.walkTypeExpr(v.X)
	case *ast.SelectorExpr:
		if id, ok := v.X.(*ast.Ident); ok {
			line := c.fset.Position(v.Pos()).Line
			c.emitRef(api.RefTypeUse, id.Name+"."+v.Sel.Name, line)
		}
	case *ast.ArrayType:
		c.walkTypeExpr(v.Elt)
		if v.Len != nil {
			c.walkTypeExpr(v.Len)
		}
	case *ast.MapType:
		c.walkTypeExpr(v.Key)
		c.walkTypeExpr(v.Value)
	case *ast.ChanType:
		c.walkTypeExpr(v.Value)
	case *ast.FuncType:
		c.walkFieldList(v.Params)
		if v.Results != nil {
			c.walkFieldList(v.Results)
		}
	case *ast.InterfaceType:
		if v.Methods != nil {
			c.walkFieldList(v.Methods)
		}
	case *ast.StructType:
		if v.Fields != nil {
			c.walkFieldList(v.Fields)
		}
	case *ast.Ellipsis:
		c.walkTypeExpr(v.Elt)
	case *ast.ParenExpr:
		c.walkTypeExpr(v.X)
	}
}

// walkFieldList emits a RefTypeUse edge for every named type
// that appears in a parameter / result / struct-field /
// interface-method list. The position of the ref is the line of
// the field declaration; bare (un-named) types use the type
// expression's start line.
func (c *goCollector) walkFieldList(fl *ast.FieldList) {
	if fl == nil {
		return
	}
	for _, f := range fl.List {
		c.walkTypeExpr(f.Type)
	}
}

// handleCall processes a *ast.CallExpr. The function position
// (Call.Fun) decides the ref kind:
//   - SelectorExpr on an imported package's local name
//     ("lib.Hello") -> RefCall to "<import_local>.Hello"
//   - SelectorExpr on a local of known type ("x.Hello") ->
//     RefCall to "<local_type>.Hello"
//   - SelectorExpr on a struct field or pointer to struct
//     -> recurse into the receiver expression
//   - Bare Ident ("Bar()") -> RefCall to "pkg.Bar"
//   - FunLit directly -> RefCall to the synthetic funclit
//
// We also recurse into every argument expression so calls
// inside a call's args still produce their own refs.
func (c *goCollector) handleCall(call *ast.CallExpr) {
	if call == nil {
		return
	}
	line := c.fset.Position(call.Pos()).Line
	switch fn := call.Fun.(type) {
	case *ast.Ident:
		// Bare name. Could be a same-package function,
		// a builtin, or a local function variable.
		if fn.Name == "" || fn.Name == "_" {
			return
		}
		// If the name matches a local variable of function
		// type, we can't know the underlying function; skip.
		if _, isLocal := c.locals[fn.Name]; isLocal {
			return
		}
		c.emitRef(api.RefCall, c.pkgName+"."+fn.Name, line)
	case *ast.SelectorExpr:
		c.handleCallSelector(fn, line)
	case *ast.FuncLit:
		// Direct invocation: func(){...}(). The IIFE
		// still owns the call edges inside its body, so
		// push the funclit symbol onto the stack and walk
		// the body. Tests pin this for
		// TestFuncLit_GoStatementAtFileScope.
		c.handleFuncLit(fn)
	default:
		// Anything else (indexing, parens) is walked.
		c.walkExpr(call.Fun)
	}
	for _, arg := range call.Args {
		c.walkExpr(arg)
	}
}

// handleCallSelector implements the receiver-method and
// package-call logic for "x.Method()" and "pkg.Method()".
// Receiver resolution relies on the locals map populated by
// handleAssign; the package case relies on the import map.
func (c *goCollector) handleCallSelector(sel *ast.SelectorExpr, line int) {
	x := sel.X
	method := sel.Sel.Name
	switch base := x.(type) {
	case *ast.Ident:
		// Could be: an imported package alias, a local of
		// known type, a same-package constant, or a
		// package-level variable.
		if realPkg, ok := c.lookupPackageLocal(base.Name); ok {
			// pkg.Method -> emit call to realPkg.Method
			c.emitRef(api.RefCall, realPkg+"."+method, line)
			return
		}
		if lv, ok := c.locals[base.Name]; ok && lv.known && lv.typ != "" {
			// Local with a known type -> method on that
			// type. The local's typ is the bare type
			// name (e.g. "Store") set by typeFromReceiver
			// or typeFromCallReturning. The qualified
			// name prefix is the enclosing package for
			// same-package types; cross-package types
			// already carry their own prefix in typ.
			qname := lv.typ
			if !strings.Contains(qname, ".") {
				qname = c.pkgName + "." + qname
			}
			c.emitRef(api.RefCall, qname+"."+method, line)
			return
		}
		// Same-package package-level var / const of
		// known type: try the symbol table we have
		// built in this file.
		if t, ok := c.lookupLocalSymbolType(base.Name); ok {
			c.emitRef(api.RefCall, t+"."+method, line)
			return
		}
		// Last resort: a value method on a same-package
		// type whose name we couldn't resolve. Emit a
		// call to "<baseName>.<method>" so the graph
		// still has the edge; the call site can decide
		// whether to filter it.
		c.emitRef(api.RefCall, base.Name+"."+method, line)
	case *ast.SelectorExpr:
		// x.y.Method() where x.y is itself a selector:
		// handle the inner first so the local-type map
		// can pick up the intermediate type if the
		// indexer knows how to follow it.
		c.walkExpr(base)
		// Emit a conservative call ref to "<inner>.<method>"
		// so the edge is recorded.
		if id, ok := base.X.(*ast.Ident); ok {
			c.emitRef(api.RefCall, id.Name+"."+method, line)
		}
	case *ast.CallExpr:
		// f().Method() — chain. Walk the inner call.
		c.walkExpr(base)
	case *ast.StarExpr:
		// (*x).Method() — recurse on the dereferenced
		// pointer; the method-set is the same.
		c.walkExpr(base.X)
	default:
		c.walkExpr(base)
	}
}

// isPackageName reports whether name is a known package-level
// alias from the file's import block. A selector on a package
// name is treated as a call into that package; a selector on a
// non-package name is treated as a value method call.
func (c *goCollector) isPackageName(name string) bool {
	_, ok := c.pkgNames[name]
	return ok
}

// lookupPackageLocal returns the resolved package name (the
// canonical local-name for the import) for the given
// selector base. The input is whatever the user wrote after
// the import: an alias, a resolved package name, or a path
// basename (the legacy fallback). The return value is
// always the resolved package name, which the call ref
// uses as the qualified-name prefix. The bool reports
// whether name referred to a known import.
func (c *goCollector) lookupPackageLocal(name string) (string, bool) {
	path, ok := c.pkgNames[name]
	if !ok {
		return "", false
	}
	// The "true" local name of the import in this file is
	// the value of c.imports[path]. For an aliased import
	// that is the alias; for a non-aliased import it is the
	// resolved package name. We prefer the resolved name
	// (stored under path+"|resolved") so the ref target
	// uses the canonical Go import name, not the alias.
	if real, ok := c.imports[path+"|resolved"]; ok && real != "" {
		return real, true
	}
	if real, ok := c.imports[path]; ok && real != "" {
		return real, true
	}
	return name, true
}

// packageQualifiedName returns the qualified target of a
// selector whose base is a known package import. The
// integration tests pin this for the
// TestIngestImportAlias_PathDiffersFromPkgName case: an import
// with a real package name different from the path basename
// must be resolved to the real name. The result is the
// resolved package name + "." + method; the call site
// attributes the call to the resolved name.
func (c *goCollector) packageQualifiedName(name, method string) string {
	// The imports map has already been populated by
	// buildImportMap. For a bare "import \"path/to/x\""
	// without an alias, the local name was set to the real
	// package name resolved via the cache. For an aliased
	// import (import foo "path/to/x"), the local name is
	// "foo". Either way, the local name is the right prefix
	// for the qualified call.
	for _, local := range c.imports {
		if local == name {
			return name + "." + method
		}
	}
	return name + "." + method
}

// handleSelector processes a non-call selector expression
// ("x.Field" without an invocation). The same receiver rules
// apply as for handleCallSelector but the ref kind is RefValue
// (or RefTypeUse when the selector names a package-qualified
// type), and we do not recurse into args.
func (c *goCollector) handleSelector(sel *ast.SelectorExpr) {
	if sel == nil {
		return
	}
	line := c.fset.Position(sel.Pos()).Line
	if id, ok := sel.X.(*ast.Ident); ok {
		if realPkg, ok := c.lookupPackageLocal(id.Name); ok {
			// pkg.Type — a type-use ref.
			c.emitRef(api.RefTypeUse, realPkg+"."+sel.Sel.Name, line)
			return
		}
	}
	c.walkExpr(sel.X)
}

// handleCompositeLit processes a composite literal
// ("&T{}", "T{}"). The element type produces a RefTypeUse edge
// to the type's qualified name, attributed to the literal's
// line. The integration test TestAddCompositeRef_StarExprLineNumber
// pins this: the ref line must be the literal's line, not the
// type expression's.
func (c *goCollector) handleCompositeLit(lit *ast.CompositeLit) {
	if lit == nil {
		return
	}
	line := c.fset.Position(lit.Pos()).Line
	if lit.Type != nil {
		switch t := lit.Type.(type) {
		case *ast.Ident:
			c.emitRef(api.RefTypeUse, c.pkgName+"."+t.Name, line)
		case *ast.StarExpr:
			if id, ok := t.X.(*ast.Ident); ok {
				c.emitRef(api.RefTypeUse, c.pkgName+"."+id.Name, line)
			} else {
				c.walkExpr(t.X)
			}
		case *ast.SelectorExpr:
			if id, ok := t.X.(*ast.Ident); ok {
				c.emitRef(api.RefTypeUse, id.Name+"."+t.Sel.Name, line)
			}
		default:
			c.walkTypeExpr(lit.Type)
		}
	}
	for _, el := range lit.Elts {
		if kv, ok := el.(*ast.KeyValueExpr); ok {
			c.walkExpr(kv.Value)
		} else {
			c.walkExpr(el)
		}
	}
}

// handleFuncLit emits a synthetic funclit owner for a
// top-level *ast.FuncLit and pushes it onto funcStack so the
// calls inside the closure attribute to the funclit symbol
// rather than to the enclosing function. Nested FuncLits (i.e.
// those inside another func body) are NOT given a synthetic
// owner: the integration test
// TestFuncLit_NestedClosureAttributedToEnclosingFunc pins that
// behaviour explicitly.
func (c *goCollector) handleFuncLit(fl *ast.FuncLit) {
	if fl == nil {
		return
	}
	if c.inFuncBody {
		// Nested closure: attribute to the enclosing
		// owner.
		c.walkFuncLitBody(fl)
		return
	}
	// Top-level: emit a synthetic funclit symbol.
	basename := c.relPath
	if i := strings.LastIndex(basename, "/"); i >= 0 {
		basename = basename[i+1:]
	}
	basename = strings.TrimSuffix(basename, ".go")
	start := c.fset.Position(fl.Pos()).Line
	end := c.fset.Position(fl.End()).Line
	c.funclitCounter++
	qname := fmt.Sprintf("%s.__lit__%s_%d__", c.pkgName, basename, start)
	idx := c.emitSymbol(api.Symbol{
		Kind:          api.KindFuncLit,
		Name:          fmt.Sprintf("__lit__%s_%d__", basename, start),
		QualifiedName: qname,
		StartLine:     start,
		EndLine:       end,
	})
	c.funcStack = append(c.funcStack, idx)
	c.enterScope()
	c.walkFuncLitBody(fl)
	c.leaveScope()
	c.funcStack = c.funcStack[:len(c.funcStack)-1]
}

// walkFuncLitBody walks a function literal's body for refs.
// Parameter types are recorded as RefTypeUse so the literal
// shows up correctly in query results that filter on kind.
func (c *goCollector) walkFuncLitBody(fl *ast.FuncLit) {
	if fl.Type != nil {
		c.walkFieldList(fl.Type.Params)
		if fl.Type.Results != nil {
			c.walkFieldList(fl.Type.Results)
		}
	}
	if fl.Body != nil {
		prev := c.inFuncBody
		c.inFuncBody = true
		c.visitBlockStmt(fl.Body)
		c.inFuncBody = prev
	}
}

// lookupLocalSymbolType returns the qualified name of the type
// of a package-level symbol declared in the same file. Used to
// resolve "var x = &T{}; x.Method()" when x is a package-level
// variable and the indexer has already seen its declaration.
func (c *goCollector) lookupLocalSymbolType(name string) (string, bool) {
	for _, s := range c.symbols {
		if s.Name == name {
			// The symbol's kind is var/const. We don't
			// carry a type tag in the Symbol struct, so
			// best-effort: same-package name -> no
			// resolution here. The historical pipeline
			// didn't either.
			_ = s
			return "", false
		}
	}
	return "", false
}
