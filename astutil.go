package gofrontend

import (
	"go/ast"
	"go/token"
	"strings"

	"github.com/Wolf258/mekami-api/api/v1"
)

// receiverName returns the bare type name of a function's receiver,
// stripping a leading pointer so "func (s *Store)" yields "Store".
// Returns "" for a missing receiver (defensive: a well-formed
// FuncDecl with Recv != nil always has at least one field).
func receiverName(recv *ast.FieldList) string {
	if recv == nil || len(recv.List) == 0 {
		return ""
	}
	switch t := recv.List[0].Type.(type) {
	case *ast.Ident:
		return t.Name
	case *ast.StarExpr:
		if id, ok := t.X.(*ast.Ident); ok {
			return id.Name
		}
	}
	return ""
}

// formatSignature renders a function declaration as a single line
// "func Name(params) returns", where params is the comma-joined
// field-list representation and returns is the parenthesised
// result list or "" when there is none. The format is intentionally
// short: the symbol's qualified name already carries the package
// and receiver context.
func formatSignature(fn *ast.FuncDecl) string {
	var b strings.Builder
	b.WriteString("func ")
	b.WriteString(fn.Name.Name)
	b.WriteByte('(')
	b.WriteString(formatFieldList(fn.Type.Params))
	b.WriteByte(')')
	if fn.Type.Results != nil && len(fn.Type.Results.List) > 0 {
		res := formatFieldList(fn.Type.Results)
		// Single unnamed result: drop the parens for a tighter look.
		if len(fn.Type.Results.List) == 1 && len(fn.Type.Results.List[0].Names) == 0 {
			b.WriteByte(' ')
			b.WriteString(res)
		} else {
			b.WriteString(" (")
			b.WriteString(res)
			b.WriteByte(')')
		}
	}
	return b.String()
}

// formatFieldList renders a (params / results) field list as a
// comma-joined list of "name type" or just "type" for unnamed
// fields. Names and types are taken literally from the AST, so
// composite types like "map[string]int" round-trip verbatim.
func formatFieldList(fl *ast.FieldList) string {
	if fl == nil || len(fl.List) == 0 {
		return ""
	}
	parts := make([]string, 0, len(fl.List))
	for _, f := range fl.List {
		t := exprText(f.Type)
		if len(f.Names) == 0 {
			parts = append(parts, t)
			continue
		}
		names := make([]string, len(f.Names))
		for i, n := range f.Names {
			names[i] = n.Name
		}
		parts = append(parts, strings.Join(names, ", ")+" "+t)
	}
	return strings.Join(parts, ", ")
}

// exprText is a best-effort stringifier for the few expression
// shapes the formatter encounters: identifiers, selectors,
// pointers, arrays, maps, channels, function types, and parens.
// The fallback uses Go's printer-free form, which Go's own format
// package would emit, but we avoid the import because it pulls in
// go/printer for one helper. Unknown shapes degrade to a single
// underscore so the signature still parses.
func exprText(e ast.Expr) string {
	switch v := e.(type) {
	case *ast.Ident:
		return v.Name
	case *ast.StarExpr:
		return "*" + exprText(v.X)
	case *ast.SelectorExpr:
		return exprText(v.X) + "." + v.Sel.Name
	case *ast.ArrayType:
		if v.Len == nil {
			return "[]" + exprText(v.Elt)
		}
		return "[" + exprText(v.Len) + "]" + exprText(v.Elt)
	case *ast.MapType:
		return "map[" + exprText(v.Key) + "]" + exprText(v.Value)
	case *ast.ChanType:
		switch v.Dir {
		case ast.RECV:
			return "<-chan " + exprText(v.Value)
		case ast.SEND:
			return "chan<- " + exprText(v.Value)
		default:
			return "chan " + exprText(v.Value)
		}
	case *ast.FuncType:
		return "func(" + formatFieldList(v.Params) + ")"
	case *ast.Ellipsis:
		return "..." + exprText(v.Elt)
	case *ast.InterfaceType:
		return "interface{}"
	case *ast.StructType:
		return "struct{ " + formatFieldList(v.Fields) + " }"
	case *ast.ParenExpr:
		return "(" + exprText(v.X) + ")"
	}
	return "_"
}

// formatTypeFields renders the inner field list of a struct or
// interface declaration. Used to populate the Signature column of
// type symbols so the MCP can show the shape without a re-read.
func formatTypeFields(fields *ast.FieldList) string {
	if fields == nil {
		return ""
	}
	return formatFieldList(fields)
}

// qualifiedName concatenates the optional package, receiver, and
// function name into the dotted qualified name used as the
// stable identifier for the symbol across files. The receiver
// and pkg segments are dropped when empty.
func qualifiedName(pkg, recv, name string) string {
	switch {
	case pkg != "" && recv != "":
		return pkg + "." + recv + "." + name
	case pkg != "":
		return pkg + "." + name
	case recv != "":
		return recv + "." + name
	}
	return name
}

// isExported is a thin wrapper so callers don't have to import
// go/ast for the single check. The behaviour is the canonical
// "starts with an upper-case letter" rule.
func isExported(name string) bool {
	if name == "" {
		return false
	}
	return ast.IsExported(name)
}

// kindFromTok maps the go/ast token of a GenDecl to the indexer's
// SymbolKind. VAR and CONST share the value path; TYPE uses its
// own branch. An unrecognised token falls back to KindVar so the
// symbol still shows up in the graph.
func kindFromTok(tok token.Token) api.SymbolKind {
	switch tok {
	case token.CONST:
		return api.KindConst
	case token.TYPE:
		return api.KindType
	case token.IMPORT:
		return api.KindImports
	case token.VAR:
		return api.KindVar
	}
	return api.KindVar
}
