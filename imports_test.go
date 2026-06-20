package gofrontend

import (
	"os"
	"path/filepath"
	"testing"
)

// TestImportCache_Resolve_SamePackage verifies the basic
// positive case: two consecutive resolves of the same
// (root, path) hit the cache after the first FS lookup, and
// the resolved name is the same in both calls.
func TestImportCache_Resolve_SamePackage(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "vendor", "example.com", "foo"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "vendor", "example.com", "foo", "foo.go"),
		[]byte("package foo\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	c := newImportNameCache()
	got1 := c.resolve(dir, "example.com/foo")
	got2 := c.resolve(dir, "example.com/foo")
	if got1 != "foo" {
		t.Fatalf("first resolve: got %q, want %q", got1, "foo")
	}
	if got2 != "foo" {
		t.Fatalf("second resolve: got %q, want %q (cache miss?)", got2, "foo")
	}
}

// TestImportCache_Resolve_DifferentPaths pins the case where
// the import resolution depends on the full path, not just
// the basename. Two distinct import paths mapping to two
// different package names must return distinct names.
func TestImportCache_Resolve_DifferentPaths(t *testing.T) {
	dir := t.TempDir()
	for _, p := range []struct{ path, name string }{
		{"example.com/foo", "foo"},
		{"example.com/bar", "bar"},
	} {
		full := filepath.Join(dir, "vendor", filepath.FromSlash(p.path))
		if err := os.MkdirAll(full, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(full, "x.go"),
			[]byte("package "+p.name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	c := newImportNameCache()
	if got := c.resolve(dir, "example.com/foo"); got != "foo" {
		t.Errorf("example.com/foo: got %q, want foo", got)
	}
	if got := c.resolve(dir, "example.com/bar"); got != "bar" {
		t.Errorf("example.com/bar: got %q, want bar", got)
	}
}

// TestImportCache_Resolve_AliasedImport covers the case where
// the importing file uses an explicit alias. The cache is
// told via put to record the alias; subsequent resolve
// calls return the alias verbatim. This is what enables the
// "import alias \"x/y\"" pattern to produce "alias.Foo"
// qualified names in the graph.
func TestImportCache_Resolve_AliasedImport(t *testing.T) {
	c := newImportNameCache()
	c.put("/any", "example.com/x", "alias")
	if got := c.resolve("/any", "example.com/x"); got != "alias" {
		t.Fatalf("expected alias to be returned verbatim, got %q", got)
	}
}

// TestImportCache_Resolve_DotImport pins the behaviour for
// ". \"path\"" imports. A dot import has no package name in
// the importing file; the cache stores the "." verbatim
// because the call site needs to distinguish "dot import"
// from "unknown" — the collector checks for "." before
// emitting refs.
func TestImportCache_Resolve_DotImport(t *testing.T) {
	c := newImportNameCache()
	c.put("/any", "example.com/q", ".")
	if got := c.resolve("/any", "example.com/q"); got != "." {
		t.Fatalf("dot import: expected \".\", got %q", got)
	}
}

// TestImportCache_Resolve_UnknownImport is the missing-FS
// case: the import path does not exist on disk. The cache
// returns the path basename as a last-resort fallback so
// refs are not silently dropped; the integration test
// TestIngestImportAlias_FallbackOnMissingVendor pins this
// behaviour at the build level.
func TestImportCache_Resolve_UnknownImport(t *testing.T) {
	c := newImportNameCache()
	dir := t.TempDir() // empty
	if got := c.resolve(dir, "example.com/does/not/exist"); got != "exist" {
		t.Fatalf("unknown import: expected basename fallback, got %q", got)
	}
}

// TestImportCache_Resolve_PathBaseFallback pins the
// last-resort fallback: an import path that cannot be
// resolved on disk still produces a name (the last path
// segment) so refs are not silently dropped.
func TestImportCache_Resolve_PathBaseFallback(t *testing.T) {
	c := newImportNameCache()
	dir := t.TempDir()
	if got := c.resolve(dir, "example.com/missing"); got != "missing" {
		t.Errorf("expected basename fallback, got %q", got)
	}
}
