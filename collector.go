package gofrontend

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/Wolf258/mekami-api/api/v1"
)

// goCollector carries the state a single parse worker needs to
// convert a *ast.File into the flat api.ParseResult the core
// consumes. One collector is created per file and discarded
// after the parse completes; it is not safe to share across
// files.
type goCollector struct {
	fset     *token.FileSet
	file     *ast.File
	root     string
	relPath  string
	pkgName  string
	imports  map[string]string // import path -> local name in this file
	pkgNames map[string]string // local name -> import path; subset of imports used as receivers
	cache    *importNameCache
	resolver *GomodResolver

	symbols []api.Symbol
	refs    []api.Ref
	// locals tracks the inferred type of every local variable
	// currently in scope, keyed by name. Used to resolve
	// "x := NewT(); x.Hello()" into "foo.T.Hello". A
	// known=false value means the variable is declared but its
	// type is unknown (e.g. "x := someUnknownFunc()"); we keep
	// the binding alive so a later "x = &T{}" can still
	// overwrite it.
	locals map[string]localVar
	// scopes is a stack of "what locals were added at each
	// block level". Leaving a block pops the top set and
	// removes the locals it introduced, so a name shadowed
	// inside an if/for body does not leak outward.
	scopes []map[string]struct{}
	// funcStack records the index of the enclosing owner
	// symbol in c.symbols (a *ast.FuncDecl or the synthetic
	// funclit owner). Refs attribute themselves to the top of
	// this stack.
	funcStack []int64
	// funclitCounter increments for every top-level FuncLit
	// the collector emits a synthetic owner for. Used to
	// build the __lit__<file>_<line>__ qualified name.
	funclitCounter int
	// currentScope is the innermost scope's "added locals"
	// set. Updated by addLocal and cleared by leaveScope.
	currentScope map[string]struct{}
	// inFuncBody is true while the visitor is inside a
	// *ast.FuncDecl body. A FuncLit encountered while this is
	// true is a "nested closure" and does NOT get a synthetic
	// funclit symbol (its refs attribute to the enclosing
	// function). A FuncLit at file scope (or inside a
	// top-level var) gets a synthetic funclit owner.
	inFuncBody bool
}

type localVar struct {
	known bool
	typ   string
}

// run walks the top-level decls of file and returns the parsed
// symbols and refs. The collector's internal state is reset for
// each call so the same collector can process multiple files
// (the build pipeline creates one per file in practice).
func (c *goCollector) run(fset *token.FileSet, file *ast.File, relPath string) ([]api.Symbol, []api.Ref, error) {
	c.fset = fset
	c.file = file
	c.relPath = relPath
	if file != nil && file.Name != nil {
		c.pkgName = file.Name.Name
	}
	if c.imports == nil {
		c.imports = map[string]string{}
	}
	c.locals = map[string]localVar{}
	c.scopes = c.scopes[:0]
	c.funcStack = c.funcStack[:0]
	c.funclitCounter = 0
	c.currentScope = map[string]struct{}{}

	if c.cache != nil && file != nil {
		c.imports = buildImportMap(file, c.root, c.cache)
		c.pkgNames = importLocalNames(c.imports)
	}

	if file != nil && len(file.Imports) > 0 {
		c.emitImportBlock(file)
	}

	if file == nil {
		return c.symbols, c.refs, nil
	}
	for _, decl := range file.Decls {
		c.collectDecl(decl)
	}
	return c.symbols, c.refs, nil
}

// emitImportBlock creates the synthetic __imports__ anchor
// symbol and emits one RefImport edge per import in file. The
// anchor is pushed onto funcStack while the refs are emitted and
// popped before the top-level decls are walked so call edges in
// the top-level bodies attribute to their own owner.
func (c *goCollector) emitImportBlock(file *ast.File) {
	start := c.fset.Position(file.Imports[0].Pos()).Line
	end := c.fset.Position(file.Imports[len(file.Imports)-1].End()).Line
	anchorIdx := c.emitSymbol(api.Symbol{
		Kind:          api.KindImports,
		Name:          "__imports__",
		QualifiedName: c.pkgName + ".__imports__",
		StartLine:     start,
		EndLine:       end,
	})
	c.funcStack = append(c.funcStack, anchorIdx)
	for _, imp := range file.Imports {
		if imp == nil || imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		line := c.fset.Position(imp.Pos()).Line
		c.refs = append(c.refs, api.Ref{
			FromSymbol:  anchorIdx,
			ToQualified: path,
			Kind:        api.RefImport,
			Line:        line,
		})
	}
	c.funcStack = c.funcStack[:len(c.funcStack)-1]
}

// collectDecl dispatches on the type of decl, emitting symbols
// and refs for the cases the indexer cares about. Unhandled
// decl kinds (imports, which were already handled in run) are
// silently skipped.
func (c *goCollector) collectDecl(decl ast.Decl) {
	switch d := decl.(type) {
	case *ast.FuncDecl:
		c.collectFuncDecl(d)
	case *ast.GenDecl:
		c.collectGenDecl(d)
	}
}

// collectFuncDecl emits a Symbol for the function or method,
// then walks its body for call / type-use / value-use edges.
// Receiver methods (Recv != nil) are emitted as KindMethod with
// the qualified name "pkg.Recv.Name"; top-level functions as
// KindFunc with the qualified name "pkg.Name".
func (c *goCollector) collectFuncDecl(fn *ast.FuncDecl) {
	if fn.Name == nil {
		return
	}
	start := c.fset.Position(fn.Pos()).Line
	end := c.fset.Position(fn.End()).Line
	recv := receiverName(fn.Recv)
	kind := api.KindFunc
	qname := c.pkgName + "." + fn.Name.Name
	if recv != "" {
		kind = api.KindMethod
		qname = c.pkgName + "." + recv + "." + fn.Name.Name
	}
	idx := c.emitSymbol(api.Symbol{
		Kind:          kind,
		Name:          fn.Name.Name,
		QualifiedName: qname,
		StartLine:     start,
		EndLine:       end,
		Exported:      isExported(fn.Name.Name),
		Signature:     formatSignature(fn),
	})
	c.funcStack = append(c.funcStack, idx)
	defer func() { c.funcStack = c.funcStack[:len(c.funcStack)-1] }()

	// Bind the receiver variable name to its type so the body
	// can resolve "s.Bar()" to "pkg.Recv.Bar". The receiver
	// name is always the first name of the first field, and
	// the receiver is always a pointer (if "*T") or a value
	// (if "T") of the enclosing type.
	if fn.Recv != nil && len(fn.Recv.List) > 0 && fn.Recv.List[0] != nil {
		if len(fn.Recv.List[0].Names) > 0 {
			rname := fn.Recv.List[0].Names[0].Name
			rt := typeFromReceiver(fn.Recv.List[0].Type)
			if rt != "" {
				c.locals[rname] = localVar{known: true, typ: rt}
				c.currentScope[rname] = struct{}{}
			}
		}
	}

	if fn.Type != nil {
		c.walkFieldList(fn.Type.Params)
		if fn.Type.Results != nil {
			c.walkFieldList(fn.Type.Results)
		}
	}

	if fn.Body != nil {
		c.enterScope()
		prev := c.inFuncBody
		c.inFuncBody = true
		c.visitBlockStmt(fn.Body)
		c.inFuncBody = prev
		c.leaveScope()
	}
}

// collectGenDecl handles type, var, and const decls. Imports
// are filtered at the call site; we still accept them
// defensively so a heterogeneous decl slice doesn't panic.
func (c *goCollector) collectGenDecl(decl *ast.GenDecl) {
	if decl.Tok == token.IMPORT {
		return
	}
	kind := kindFromTok(decl.Tok)
	for _, spec := range decl.Specs {
		switch s := spec.(type) {
		case *ast.TypeSpec:
			c.collectTypeSpec(s, kind)
		case *ast.ValueSpec:
			c.collectValueSpec(s, kind)
		}
	}
}

// collectTypeSpec emits a Symbol for the declared type and walks
// the underlying type expression for type-use edges. Struct
// fields and embedded type names produce ref records so queries
// like RefsTo(TypeName, kind="type-use") can show who uses a
// type as a field.
func (c *goCollector) collectTypeSpec(s *ast.TypeSpec, kind api.SymbolKind) {
	if s == nil || s.Name == nil {
		return
	}
	start := c.fset.Position(s.Pos()).Line
	end := c.fset.Position(s.End()).Line
	var sig string
	if s.Type != nil {
		switch t := s.Type.(type) {
		case *ast.StructType:
			sig = "struct { " + formatTypeFields(t.Fields) + " }"
		case *ast.InterfaceType:
			sig = "interface { " + formatTypeFields(t.Methods) + " }"
		}
	}
	idx := c.emitSymbol(api.Symbol{
		Kind:          kind,
		Name:          s.Name.Name,
		QualifiedName: c.pkgName + "." + s.Name.Name,
		StartLine:     start,
		EndLine:       end,
		Exported:      isExported(s.Name.Name),
		Signature:     sig,
	})
	c.funcStack = append(c.funcStack, idx)
	defer func() { c.funcStack = c.funcStack[:len(c.funcStack)-1] }()

	if s.Type != nil {
		c.walkTypeExpr(s.Type)
	}
}

// collectValueSpec handles var / const spec. One symbol per
// name. Type and value expressions are walked for refs.
func (c *goCollector) collectValueSpec(s *ast.ValueSpec, kind api.SymbolKind) {
	if s == nil {
		return
	}
	start := c.fset.Position(s.Pos()).Line
	end := c.fset.Position(s.End()).Line
	for _, n := range s.Names {
		idx := c.emitSymbol(api.Symbol{
			Kind:          kind,
			Name:          n.Name,
			QualifiedName: c.pkgName + "." + n.Name,
			StartLine:     start,
			EndLine:       end,
			Exported:      isExported(n.Name),
		})
		c.funcStack = append(c.funcStack, idx)
		// Each name in a multi-assign shares the same
		// expressions but is its own symbol; the visitor
		// attributes refs to the current top of funcStack.
		func() {
			defer func() { c.funcStack = c.funcStack[:len(c.funcStack)-1] }()
			if s.Type != nil {
				c.walkTypeExpr(s.Type)
			}
			for _, v := range s.Values {
				c.walkExpr(v)
			}
		}()
	}
}

// emitSymbol appends sym to c.symbols and returns its 0-based
// index, which is the value refs will store in FromSymbol.
func (c *goCollector) emitSymbol(sym api.Symbol) int64 {
	idx := int64(len(c.symbols))
	c.symbols = append(c.symbols, sym)
	return idx
}

// emitRef appends a ref attributed to the top of funcStack, or
// skips it when no owner is in scope. Refs emitted at file scope
// (no enclosing func) are dropped — a rare edge case that
// happens when the AST has top-level expressions without an
// enclosing decl.
func (c *goCollector) emitRef(kind api.RefKind, toQualified string, line int) {
	if len(c.funcStack) == 0 {
		return
	}
	c.refs = append(c.refs, api.Ref{
		FromSymbol:  c.funcStack[len(c.funcStack)-1],
		ToQualified: toQualified,
		Kind:        kind,
		Line:        line,
	})
}

// enterScope creates a fresh local-scope set so variables
// declared inside a block (if/for/func body) are forgotten when
// the block exits. The set is referenced by every local
// assignment the visitor records during the block.
func (c *goCollector) enterScope() {
	prev := c.currentScope
	c.currentScope = map[string]struct{}{}
	c.scopes = append(c.scopes, prev)
}

// leaveScope pops the innermost scope and removes the locals it
// introduced from the resolver map. Variables introduced in the
// popped scope are no longer resolvable.
func (c *goCollector) leaveScope() {
	if len(c.scopes) == 0 {
		return
	}
	for name := range c.currentScope {
		delete(c.locals, name)
	}
	c.currentScope = c.scopes[len(c.scopes)-1]
	c.scopes = c.scopes[:len(c.scopes)-1]
}

// addLocal records a name in the current scope's "added" set so
// leaveScope can clean it up. The local is always reachable via
// c.locals; the set is purely for scope-boundary bookkeeping.
func (c *goCollector) addLocal(name string) {
	c.currentScope[name] = struct{}{}
}

// typeFromReceiver returns the bare type name of a method
// receiver, stripping a leading pointer. Mirrors receiverName
// but returns the type rather than the variable name.
func typeFromReceiver(e ast.Expr) string {
	switch t := e.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}
