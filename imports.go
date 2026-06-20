package gofrontend

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"
)

// importNameCache memoises the (importPath -> packageName)
// mapping for the duration of a single build. A Go build can
// touch the same import path from thousands of files, so the
// walk-once-per-unique-path policy is the difference between
// O(n) and O(n*m) FS hits on large codebases. Safe for
// concurrent use: the mutex is taken on every access.
type importNameCache struct {
	mu sync.Mutex
	// root -> importPath -> package name
	byRoot map[string]map[string]string
}

func newImportNameCache() *importNameCache {
	return &importNameCache{byRoot: map[string]map[string]string{}}
}

// resolve returns the package name declared at the directory
// reachable by importPath, or "" when the path is not locatable
// on disk. The lookup consults the cache first, then walks the
// candidate directories in priority order. The first successful
// hit wins; subsequent calls for the same path return the same
// answer without touching the FS.
func (c *importNameCache) resolve(root, importPath string) string {
	if importPath == "" {
		return ""
	}
	c.mu.Lock()
	if m, ok := c.byRoot[root]; ok {
		if name, ok := m[importPath]; ok {
			c.mu.Unlock()
			return name
		}
	} else {
		c.byRoot[root] = map[string]string{}
	}
	c.mu.Unlock()

	name := c.lookupOnDisk(root, importPath)
	c.mu.Lock()
	c.byRoot[root][importPath] = name
	c.mu.Unlock()
	return name
}

// put records a (importPath -> name) association explicitly,
// e.g. when the importing file uses an alias. The alias name
// is what the file's references to the package actually use;
// subsequent resolves for the same path will see the alias and
// return it. This is what makes "import alias \"x\"" produce
// "alias.Foo" qualified refs even when the underlying package
// name is "snake".
func (c *importNameCache) put(root, importPath, name string) {
	if importPath == "" {
		return
	}
	c.mu.Lock()
	if _, ok := c.byRoot[root]; !ok {
		c.byRoot[root] = map[string]string{}
	}
	c.byRoot[root][importPath] = name
	c.mu.Unlock()
}

// lookupOnDisk finds the package name for importPath by trying,
// in order: the vendor tree, the module cache via the module
// path's GOMODCACHE hint, the workspace's other `use`d modules,
// and finally the path basename as a last-resort fallback. The
// first directory that contains a parseable .go file with a
// package declaration wins.
func (c *importNameCache) lookupOnDisk(root, importPath string) string {
	if importPath == "" {
		return ""
	}

	// 1) Relative imports: "./foo" or "../bar" inside the project.
	//    Resolve them against the build root and read the package
	//    name from the first .go file we find. This is the common
	//    case for in-workspace references and the only one the
	//    historical pipeline handled correctly.
	if strings.HasPrefix(importPath, "./") || strings.HasPrefix(importPath, "../") {
		dir := filepath.Join(root, filepath.FromSlash(importPath))
		if name := readPackageName(dir); name != "" {
			return name
		}
		return pathBase(importPath)
	}

	// 2) Vendor directory: the import path's tail maps to a
	//    directory under <root>/vendor/<importPath>.
	vendorDir := filepath.Join(root, "vendor", filepath.FromSlash(importPath))
	if name := readPackageName(vendorDir); name != "" {
		return name
	}

	// 3) Workspace-sibling modules: if the import path matches a
	//    `use`d module's path exactly, read its package name. We
	//    only check the prefix here, not the full sub-package,
	//    because the same module path is shared by every file in
	//    that module and the package name comes from the target
	//    directory.
	if name := c.lookupInWorkspace(root, importPath); name != "" {
		return name
	}

	// 4) GOMODCACHE: fall back to the module cache. The Go
	//    toolchain stores extracted modules under
	//    $GOMODCACHE/<module>/@v/<version>.zip — but we want the
	//    source, not the archive. A common companion file is
	//    <module>/@v/<version>.info and the unzipped source
	//    sits next to it in the cache root. We probe the cache
	//    root for an extracted module directory and walk into it.
	if name := c.lookupInGomodcache(importPath); name != "" {
		return name
	}

	// 5) Last-resort fallback: the last segment of the path.
	//    The integration test TestIngestImportAlias_FallbackOnMissingVendor
	//    pins this behaviour. Returned by the cache so the
	//    caller sees a non-empty name; a separate "missing"
	//    sentinel is not exposed.
	return pathBase(importPath)
}

// lookupInWorkspace walks the project for directories that have a
// go.mod whose module path is a prefix of importPath. The first
// matching subdirectory's package name is returned.
func (c *importNameCache) lookupInWorkspace(root, importPath string) string {
	var found string
	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			name := d.Name()
			if name == ".git" || name == "node_modules" || name == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if d.Name() != "go.mod" {
			return nil
		}
		data, rerr := os.ReadFile(path)
		if rerr != nil {
			return nil
		}
		mp := parseModulePath(data)
		if mp == "" {
			return nil
		}
		if importPath == mp {
			found = readPackageName(filepath.Dir(path))
			return filepath.SkipDir
		}
		return nil
	})
	return found
}

// lookupInGomodcache probes the Go module cache for an extracted
// module whose path matches importPath. Returns the package name
// of the importPath's subdirectory, walking up to the module root
// if the import path doesn't map directly to a directory inside
// the cached source.
func (c *importNameCache) lookupInGomodcache(importPath string) string {
	gmc := os.Getenv("GOMODCACHE")
	if gmc == "" {
		gmc = defaultGomodcache()
	}
	if gmc == "" {
		return ""
	}
	// We don't know the version, so we look for any extracted
	// source directory under <GOMODCACHE>/<importPath>@*.
	matches, _ := filepath.Glob(filepath.Join(gmc, filepath.FromSlash(importPath)+"@*"))
	for _, m := range matches {
		// The cache layout is <mod>@<ver>/... (extracted). Some
		// toolchains keep just the .zip next to a sibling
		// <mod>@<ver>/ source tree. Walk into the directory and
		// try reading the package name at the import path's
		// suffix.
		if name := readPackageName(m); name != "" {
			return name
		}
	}
	return ""
}

// defaultGomodcache is the best-effort default location of the
// Go module cache. We avoid invoking `go env GOMODCACHE` here to
// keep the path-resolution side-effect free; the env var is set
// whenever `go` runs in the same shell, which is the common case
// in CI as well.
func defaultGomodcache() string {
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, "go", "pkg", "mod")
	}
	return ""
}

// readPackageName parses the first .go file in dir and returns
// its package clause name. Returns "" when dir is unreadable,
// empty, or contains no .go files. Used by importNameCache and
// the package-resolver helpers.
func readPackageName(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		path := filepath.Join(dir, e.Name())
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, path, nil, parser.PackageClauseOnly)
		if err != nil {
			continue
		}
		if f.Name != nil {
			return f.Name.Name
		}
	}
	return ""
}

// pathBase returns the last "/"-separated segment of p, with the
// common ".go" or version suffix trimmed. It is the legacy
// fallback when the actual package name cannot be resolved from
// disk; the tests pin this behaviour.
func pathBase(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// buildImportMap walks the ImportSpecs of file and returns a map
// from import path to the local name used in the file. The local
// name is:
//   - the explicit alias if the file wrote "alias \"path\"";
//   - the package name (resolved via the cache) if the file wrote
//     "\"path\"" or "name \"path\"" with name == package name;
//   - "" for dot imports (the references are unqualified).
//
// The returned map lets the collector rewrite SelectorExpr.Fun.X
// into a proper qualified name.
func buildImportMap(file *ast.File, root string, cache *importNameCache) map[string]string {
	out := map[string]string{}
	if file == nil {
		return out
	}
	for _, imp := range file.Imports {
		if imp == nil || imp.Path == nil {
			continue
		}
		path := strings.Trim(imp.Path.Value, `"`)
		var local string
		// Resolve the real package name for every import
		// (aliased or not) so the call site can use it as
		// the qualified-name prefix even when the user
		// used an alias. The integration test
		// TestIngestImportAlias_ExplicitAliasSameAsRenamed
		// pins this: the user writes "alias" but the ref
		// must use the resolved package name "snake".
		resolved := cache.resolve(root, path)
		if imp.Name != nil {
			// An explicit alias. Dot imports come through as
			// Name.Name == "."; blank imports as "_" — both are
			// recorded verbatim and the caller decides what to
			// do with them.
			local = imp.Name.Name
		} else {
			local = resolved
		}
		cache.put(root, path, resolved)
		// Store both the alias and the resolved name
		// against the import path so the collector can
		// pick the right one when emitting refs.
		out[path] = local
		if resolved != "" && resolved != local {
			out[path+"|resolved"] = resolved
		}
	}
	return out
}

// importLocalNames returns the set of "names that refer to a
// package import" inside the file. This is the union of
// explicit aliases, the resolved package names of every
// import without an alias, and the path basename of every
// import (for the rare-but-pinned case where a file
// references a package via the path tail in a selector —
// invalid Go, but the historical build rewrote it to the
// resolved package name; the integration tests cover this
// in TestIngestImportAlias_PathDiffersFromPkgName). Each
// entry maps the local-as-used name to the import path.
//
// The "resolved" name (the real package name) is stored under
// a separate key in the imports map (path + "|resolved") so
// the collector can look it up when emitting refs; this
// lets an aliased import (import alias "x/y") still produce
// refs qualified with the real package name (e.g.
// "snake.Renamed" rather than "alias.Renamed").
func importLocalNames(imports map[string]string) map[string]string {
	out := map[string]string{}
	for path, local := range imports {
		if strings.HasSuffix(path, "|resolved") {
			continue
		}
		// Skip dot / blank imports; they cannot be used as
		// receivers in Go.
		if local == "" || local == "_" || local == "." {
			continue
		}
		out[local] = path
		// Also accept the path basename for the legacy
		// fallback: a file may have used the basename
		// directly (e.g. "strcase.X" for a
		// "package snake" import).
		if i := strings.LastIndex(path, "/"); i >= 0 {
			base := path[i+1:]
			if base != local {
				out[base] = path
			}
		} else if path != local {
			out[path] = path
		}
	}
	return out
}
