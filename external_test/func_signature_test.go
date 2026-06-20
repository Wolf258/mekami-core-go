// Package external_test holds black-box tests of the
// gofrontend.Frontend. The tests live in their own package
// (external_test) and import the production code via its
// public API only, mirroring the integration style the
// mekami-core suite uses to verify the mekami-core-go
// package.
package external_test

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/Wolf258/mekami-api/api/v1"
	gofrontend "github.com/Wolf258/mekami-core-go"
)

func TestFrontend_Name(t *testing.T) {
	if got := (gofrontend.Frontend{}).Name(); got != "go" {
		t.Fatalf("Name(): got %q, want %q", got, "go")
	}
}

func TestFrontend_Extensions(t *testing.T) {
	got := (gofrontend.Frontend{}).Extensions()
	if len(got) != 1 || got[0] != ".go" {
		t.Fatalf("Extensions(): got %v, want [\".go\"]", got)
	}
}

func TestFrontend_StructuralFiles(t *testing.T) {
	got := (gofrontend.Frontend{}).StructuralFiles()
	for _, want := range []string{"go.mod", "go.sum", "go.work"} {
		if !slices.Contains(got, want) {
			t.Errorf("StructuralFiles() missing %q; got %v", want, got)
		}
	}
}

func TestFrontend_IsIndexable(t *testing.T) {
	fe := gofrontend.Frontend{}
	cases := []struct {
		rel  string
		want bool
	}{
		{"main.go", true},
		{"foo/bar.go", true},
		{"main_test.go", false},
		{"sub/foo_test.go", false},
		{"README.md", true},
	}
	for _, c := range cases {
		if got := fe.IsIndexable(c.rel); got != c.want {
			t.Errorf("IsIndexable(%q): got %v, want %v", c.rel, got, c.want)
		}
	}
}

func TestFrontend_ResolveLayout_SingleModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/solo\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	ws, err := fe.ResolveLayout(dir)
	if err != nil {
		t.Fatal(err)
	}
	if ws == nil {
		t.Fatal("expected non-nil Workspace for single-module layout")
	}
	if ws.IsWorkspace {
		t.Errorf("single module: IsWorkspace should be false, got true")
	}
	if len(ws.WorkspaceMods) != 0 {
		t.Errorf("single module: WorkspaceMods should be empty, got %v", ws.WorkspaceMods)
	}
}

func TestFrontend_ResolveLayout_Workspace(t *testing.T) {
	dir := t.TempDir()
	appDir := filepath.Join(dir, "app")
	coreDir := filepath.Join(dir, "core")
	if err := os.MkdirAll(appDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(coreDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "go.work"),
		[]byte("go 1.22\n\nuse ./app\nuse ./core\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(appDir, "go.mod"),
		[]byte("module example.com/app\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(coreDir, "go.mod"),
		[]byte("module example.com/core\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	ws, err := fe.ResolveLayout(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !ws.IsWorkspace {
		t.Fatal("workspace: IsWorkspace should be true")
	}
	if len(ws.WorkspaceMods) != 2 {
		t.Fatalf("workspace: expected 2 mods, got %d (%v)", len(ws.WorkspaceMods), ws.WorkspaceMods)
	}
	if ws.PrimaryModPath != "example.com/app" && ws.PrimaryModPath != "example.com/core" {
		t.Errorf("PrimaryModPath should be one of the workspace modules, got %q", ws.PrimaryModPath)
	}
}

func TestFrontend_ParseFile_Simple(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/parser\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package parser\n\nfunc Bar() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "foo.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	res, err := fe.ParseFile(dir, "foo.go", filepath.Join(dir, "foo.go"),
		"hash", 0, int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if res.Lang != "go" {
		t.Errorf("Lang: got %q, want go", res.Lang)
	}
	if res.ModuleID != "example.com/parser" {
		t.Errorf("ModuleID: got %q, want example.com/parser", res.ModuleID)
	}
	if res.PackageID != "example.com/parser" {
		t.Errorf("PackageID: got %q, want example.com/parser", res.PackageID)
	}
	var bar *api.Symbol
	for i := range res.Symbols {
		if res.Symbols[i].Name == "Bar" {
			bar = &res.Symbols[i]
			break
		}
	}
	if bar == nil {
		t.Fatalf("expected a symbol named Bar, got %+v", res.Symbols)
	}
	if bar.Kind != api.KindFunc {
		t.Errorf("Bar.Kind: got %q, want func", bar.Kind)
	}
	if bar.QualifiedName != "parser.Bar" {
		t.Errorf("Bar.QualifiedName: got %q, want parser.Bar", bar.QualifiedName)
	}
}

// TestFrontend_ParseFile_CallRef verifies that ParseFile
// emits a RefCall edge for a function-to-function call
// inside the same file. The ref's FromSymbol points at
// the caller symbol, ToQualified is the caller's
// "<pkg>.<name>" of the callee.
func TestFrontend_ParseFile_CallRef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/call\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package call\n\nfunc A() { B() }\nfunc B() {}\n"
	if err := os.WriteFile(filepath.Join(dir, "x.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	res, err := fe.ParseFile(dir, "x.go", filepath.Join(dir, "x.go"),
		"hash", 0, int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	var aIdx int64 = -1
	for i, s := range res.Symbols {
		if s.Name == "A" {
			aIdx = int64(i)
			break
		}
	}
	if aIdx < 0 {
		t.Fatalf("expected symbol A, got %+v", res.Symbols)
	}
	found := false
	for _, r := range res.Refs {
		if r.FromSymbol == aIdx && r.Kind == api.RefCall && r.ToQualified == "call.B" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a call ref from A to call.B, got %+v", res.Refs)
	}
}

// TestFrontend_RootModule verifies that RootModule returns
// the module path declared in the build root's go.mod.
func TestFrontend_RootModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module root.example/foo\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	got, err := fe.RootModule(dir)
	if err != nil {
		t.Fatal(err)
	}
	if got != "root.example/foo" {
		t.Errorf("RootModule: got %q, want root.example/foo", got)
	}
}

// TestFrontend_RootModule_Missing pins the "no go.mod"
// behaviour: RootModule returns ("", nil) so the caller can
// treat the absence as a soft error and skip the meta
// write.
func TestFrontend_RootModule_Missing(t *testing.T) {
	dir := t.TempDir()
	fe := gofrontend.Frontend{}
	got, err := fe.RootModule(dir)
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if got != "" {
		t.Errorf("expected empty module path, got %q", got)
	}
}

// TestFrontend_ResolveFile_SingleModule pins the
// ResolveFile path for a single-module build: the file's
// ModuleID is the root module path, the PackageID is the
// module path itself (the file lives at the module root),
// and DirRel is empty.
func TestFrontend_ResolveFile_SingleModule(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/root\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "main.go"),
		[]byte("package root\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	meta, err := fe.ResolveFile(dir, filepath.Join(dir, "main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.ModuleID != "example.com/root" {
		t.Errorf("ModuleID: got %q, want example.com/root", meta.ModuleID)
	}
	if meta.PackageID != "example.com/root" {
		t.Errorf("PackageID: got %q, want example.com/root", meta.PackageID)
	}
	if meta.DirRel != "" {
		t.Errorf("DirRel: got %q, want empty", meta.DirRel)
	}
}

// TestFrontend_ResolveFile_SubDir verifies that a file
// inside a subdirectory has its PackageID extended with
// the subdir path. This is the shape the core's
// UpsertPackage call relies on.
func TestFrontend_ResolveFile_SubDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/root\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "s.go"),
		[]byte("package sub\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	meta, err := fe.ResolveFile(dir, filepath.Join(dir, "sub", "s.go"))
	if err != nil {
		t.Fatal(err)
	}
	if meta.PackageID != "example.com/root/sub" {
		t.Errorf("PackageID: got %q, want example.com/root/sub", meta.PackageID)
	}
	if meta.DirRel != "sub" {
		t.Errorf("DirRel: got %q, want sub", meta.DirRel)
	}
}

// TestFrontend_Register is a smoke test for the init
// pathway: a fresh registry that has not seen the package
// before should accept the frontend under Name()="go".
func TestFrontend_Register(t *testing.T) {
	r := api.NewRegistry()
	r.Register(gofrontend.Frontend{})
	got, err := r.Get("go")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name() != "go" {
		t.Errorf("registered frontend name: got %q, want go", got.Name())
	}
}

// TestFrontend_ParseFile_TypeUseRef verifies that a
// type-use ref is emitted for a function whose parameter
// or return type references a same-package type. The
// expected kind is RefTypeUse; the ToQualified uses the
// "<pkg>.<Type>" qualified name.
func TestFrontend_ParseFile_TypeUseRef(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "go.mod"),
		[]byte("module example.com/tu\n\ngo 1.22\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	src := "package tu\n\ntype T struct{}\nfunc Use(t T) { _ = t }\n"
	if err := os.WriteFile(filepath.Join(dir, "t.go"), []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	fe := gofrontend.Frontend{}
	res, err := fe.ParseFile(dir, "t.go", filepath.Join(dir, "t.go"),
		"hash", 0, int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, r := range res.Refs {
		if r.Kind == api.RefTypeUse && r.ToQualified == "tu.T" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected a RefTypeUse to tu.T, got %+v", res.Refs)
	}
}

// _ keeps the context import live in case the test set
// grows and starts using it for cancellation hooks. Cheap
// insurance against accidental pruning.
var _ = context.Background
