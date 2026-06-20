// Package gofrontend is the Go-language indexer for Mekami. It
// implements api.Frontend (from github.com/Wolf258/mekami-api/api/v1)
// and self-registers via init() so any binary that blank-imports
// the package gets the Go indexer in api.Global.
//
// The package is split into a few focused files:
//
//	parser.go    — the Frontend entry point: ResolveLayout,
//	               ResolveModules, RootModule, ResolveFile, ParseFile.
//	collector.go — the symbol/ref collector driven by ParseFile.
//	visitor.go   — statement-level AST traversal (visitBlockStmt etc.).
//	walkexpr.go  — expression-level traversal (walkExpr, handleCall).
//	imports.go   — the import-name cache and vendor/GOMODCACHE fallback.
//	resolve.go   — go.mod-driven module / package id resolution.
//	astutil.go   — small helpers (signature formatting, kind mapping).
package gofrontend

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/Wolf258/mekami-api/api/v1"
)

// Frontend is the Go-language api.Frontend implementation.
// The zero value is usable; the package keeps no per-instance
// state beyond the per-build caches rooted in parserCache.
type Frontend struct{}

// Name returns the language identifier used on the CLI (--lang)
// and persisted in the files.lang column.
func (Frontend) Name() string { return "go" }

// Extensions lists the file suffixes the build walker should
// visit. Currently the single .go extension.
func (Frontend) Extensions() []string { return []string{".go"} }

// StructuralFiles lists the basenames whose edit invalidates the
// index beyond what an incremental re-ingest can repair. The
// watcher consults this list to decide between a full and an
// incremental rebuild.
func (Frontend) StructuralFiles() []string {
	return []string{"go.mod", "go.sum", "go.work"}
}

// IsIndexable reports whether a relative path inside the build
// root should be ingested. Go's _test.go convention is honoured:
// same-package test files are skipped so the graph only contains
// the production code.
func (Frontend) IsIndexable(rel string) bool {
	return !strings.HasSuffix(rel, "_test.go")
}

// parserCache is the per-process state for Frontend. A build
// instantiates a fresh cache on demand; the Frontend methods
// allocate one lazily and share it across all method calls in
// the same build. Tests that need isolation should construct
// their own Frontend{} and the package-level helpers will still
// work because they read the cache via the methods.
//
// The cache holds:
//   - a per-build GomodResolver (per root) so the resolver cache
//     is reset on every build
//   - a per-build importNameCache (per root) so cross-file
//     alias resolution picks up the fresh imports
//   - a small string of per-build scratch space
var (
	cacheMu  sync.Mutex
	parserC  *GomodResolver
	importC  *importNameCache
	cacheKey string
)

// getOrCreateCache returns the per-build caches, building them
// the first time the given root is seen. The cache is keyed on
// the build root so consecutive builds over different roots do
// not see stale resolver entries.
func getOrCreateCache(root string) (*GomodResolver, *importNameCache) {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	if cacheKey != root || parserC == nil {
		parserC = NewGomodResolver(root)
		importC = newImportNameCache()
		cacheKey = root
	}
	return parserC, importC
}

// ResetCaches clears the per-process caches. Exposed for tests
// that need to re-trigger resolver walk-up between scenarios;
// production code never calls this.
func ResetCaches() {
	cacheMu.Lock()
	defer cacheMu.Unlock()
	parserC = nil
	importC = nil
	cacheKey = ""
}

// ResolveLayout detects whether root is part of a Go workspace
// (a go.work walk-up). When a workspace is found the returned
// api.Workspace is fully populated with the used modules and
// the primary module path; when none is found the returned
// Workspace is the zero value (IsWorkspace=false) and the error
// is nil so the build can carry on with a single-module layout.
func (Frontend) ResolveLayout(root string) (*api.Workspace, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("abs root: %w", err)
	}
	cur := abs
	for {
		candidate := filepath.Join(cur, "go.work")
		data, rerr := os.ReadFile(candidate)
		if rerr == nil {
			mods, perr := parseGoWorkUses(cur, string(data))
			if perr != nil {
				return nil, fmt.Errorf("parse %s: %w", candidate, perr)
			}
			ws := &api.Workspace{
				IsWorkspace:   true,
				WorkFile:      candidate,
				WorkspaceDir:  cur,
				WorkspaceMods: mods,
			}
			// Resolve primary module path: try the
			// workspace root first, fall back to the
			// first use'd module that has a go.mod.
			if mp, merr := readGoModPrimary("", cur); merr == nil && mp != "" {
				ws.PrimaryModPath = mp
				ws.PrimaryModuleDir = cur
			} else {
				for _, m := range mods {
					if mp, merr := readGoModPrimary("", m); merr == nil && mp != "" {
						ws.PrimaryModPath = mp
						ws.PrimaryModuleDir = m
						break
					}
				}
			}
			return ws, nil
		} else if !os.IsNotExist(rerr) {
			return nil, fmt.Errorf("read %s: %w", candidate, rerr)
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			break
		}
		cur = parent
	}
	// No go.work in the ancestry: single-module layout.
	return &api.Workspace{}, nil
}

// parseGoWorkUses reads a go.work file's `use` directives and
// returns the absolute paths of every referenced module. Both
// the line-oriented form (`use ./app`) and the block form
// (`use (\n  ./a\n  ./b\n)`) are supported. Inline `//` comments
// are tolerated.
func parseGoWorkUses(root, content string) ([]string, error) {
	var out []string
	lines := strings.Split(content, "\n")
	inUse := false
	for _, raw := range lines {
		line := strings.TrimSpace(stripLineComment(raw))
		if line == "" {
			continue
		}
		if inUse {
			// End of the use block.
			if strings.HasPrefix(line, ")") {
				inUse = false
				continue
			}
			out = append(out, resolveUsePath(root, line))
			continue
		}
		if strings.HasPrefix(line, "use ") {
			rest := strings.TrimSpace(line[len("use"):])
			if strings.HasPrefix(rest, "(") {
				inUse = true
				rest = strings.TrimSpace(strings.TrimPrefix(rest, "("))
				if rest != "" && !strings.HasPrefix(rest, ")") {
					out = append(out, resolveUsePath(root, rest))
				}
				continue
			}
			out = append(out, resolveUsePath(root, rest))
			continue
		}
	}
	return out, nil
}

// stripLineComment removes everything from "//" to end-of-line
// in s. It does not handle block comments or string literals —
// the go.work syntax is simple enough that those do not appear
// inside the directives we care about.
func stripLineComment(s string) string {
	if i := strings.Index(s, "//"); i >= 0 {
		return s[:i]
	}
	return s
}

// resolveUsePath turns a "use" directive's path into an absolute
// path anchored at root. The directive is always interpreted
// relative to the go.work file's directory; absolute paths pass
// through unchanged.
func resolveUsePath(root, p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return filepath.Clean(p)
	}
	return filepath.Clean(filepath.Join(root, filepath.FromSlash(p)))
}

// ResolveModules enumerates every module the build root
// contains. For a workspace the result is one entry per used
// module with its module path; for a single-module repo the
// result is a single entry pointing at the root module itself.
// An unreadable go.mod is not an error: the entry is still
// returned with an empty ModuleID so the build knows the dir
// is a module root but cannot pin down its import path.
func (Frontend) ResolveModules(root string) ([]api.ModuleInfo, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return nil, fmt.Errorf("abs root: %w", err)
	}
	fe := Frontend{}
	ws, err := fe.ResolveLayout(abs)
	if err != nil {
		return nil, err
	}
	if ws.IsWorkspace {
		out := make([]api.ModuleInfo, 0, len(ws.WorkspaceMods))
		for _, m := range ws.WorkspaceMods {
			mp, _ := readGoModPrimary("", m)
			out = append(out, api.ModuleInfo{Dir: m, ModuleID: mp})
		}
		return out, nil
	}
	mp, _ := readGoModPrimary("", abs)
	return []api.ModuleInfo{{Dir: abs, ModuleID: mp}}, nil
}

// RootModule returns the canonical module path for the build
// root. Empty root or absent go.mod is reported as ("", nil)
// so the caller can decide whether absence matters.
func (Frontend) RootModule(root string) (string, error) {
	if root == "" {
		return "", nil
	}
	abs, err := filepath.Abs(root)
	if err != nil {
		return "", fmt.Errorf("abs root: %w", err)
	}
	mp, err := readGoModPrimary("", abs)
	if err != nil {
		return "", nil
	}
	return mp, nil
}

// ResolveFile maps an absolute file path to the module and
// package identifiers the indexer stamps into every symbol it
// emits for that file. The DirRel field is the file's path
// relative to the package's directory (always "" for files
// at the module root, "sub/dir" for files in a subdir).
func (Frontend) ResolveFile(root, abs string) (api.FileMeta, error) {
	resolver, _ := getOrCreateCache(root)
	info, err := resolver.Resolve(abs)
	if err != nil {
		return api.FileMeta{}, err
	}
	if info == nil {
		// No go.mod reachable: best-effort fallback using
		// the file's directory basename as the package id.
		dir := filepath.Dir(abs)
		return api.FileMeta{
			ModuleID:  "",
			PackageID: filepath.Base(dir),
			DirRel:    relToRoot(root, dir),
		}, nil
	}
	rel, err := filepath.Rel(info.Dir, filepath.Dir(abs))
	if err != nil {
		return api.FileMeta{}, err
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		rel = ""
	}
	pkgID := info.ModuleID
	if rel != "" {
		pkgID = joinPackagePath(info.ModuleID, rel)
	}
	return api.FileMeta{
		ModuleID:  info.ModuleID,
		PackageID: pkgID,
		DirRel:    rel,
	}, nil
}

// relToRoot returns dir's path relative to root, using forward
// slashes. Used by ResolveFile's fallback when no go.mod is
// reachable.
func relToRoot(root, dir string) string {
	rel, err := filepath.Rel(root, dir)
	if err != nil {
		return ""
	}
	rel = filepath.ToSlash(rel)
	if rel == "." {
		return ""
	}
	return rel
}

// ParseFile reads, parses, and indexes a single Go source file.
// The result is a fully-formed api.ParseResult: ModuleID,
// PackageID, DirRel are populated from the gomod resolver; the
// symbols and refs come from the goCollector. A parse error is
// returned as-is so the build pipeline can surface it to the
// caller; an unreadable file is also an error.
func (Frontend) ParseFile(root, rel, abs, hash string, mtime, size int64) (api.ParseResult, error) {
	resolver, cache := getOrCreateCache(root)
	info, err := resolver.Resolve(abs)
	if err != nil {
		return api.ParseResult{}, err
	}
	meta, err := (Frontend{}).ResolveFile(root, abs)
	if err != nil {
		return api.ParseResult{}, err
	}
	if info != nil {
		// Override the file-level package id with the
		// fully-qualified import path so every symbol in
		// the file has a stable, file-independent
		// qualified name prefix.
		dirRel, rerr := filepath.Rel(info.Dir, filepath.Dir(abs))
		if rerr == nil {
			dirRel = filepath.ToSlash(dirRel)
			if dirRel == "." {
				dirRel = ""
			}
			meta.PackageID = info.PackageID
			if dirRel != "" {
				meta.PackageID = joinPackagePath(info.PackageID, dirRel)
			}
			meta.DirRel = dirRel
		}
	}

	data, err := os.ReadFile(abs)
	if err != nil {
		return api.ParseResult{}, err
	}
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, abs, data, parser.ParseComments)
	if err != nil {
		return api.ParseResult{}, err
	}
	col := &goCollector{
		root:     root,
		relPath:  rel,
		cache:    cache,
		resolver: resolver,
	}
	syms, refs, err := col.run(fset, file, rel)
	if err != nil {
		return api.ParseResult{}, err
	}
	return api.ParseResult{
		RelPath:   rel,
		Lang:      "go",
		ModuleID:  meta.ModuleID,
		PackageID: meta.PackageID,
		DirRel:    meta.DirRel,
		Hash:      hash,
		Mtime:     mtime,
		Size:      size,
		Symbols:   syms,
		Refs:      refs,
	}, nil
}

// joinPackagePath concatenates a module path with a relative
// sub-package directory, ensuring exactly one "/" between them.
// The result is the canonical Go import path of the file's
// package.
func joinPackagePath(mod, rel string) string {
	if rel == "" {
		return mod
	}
	if mod == "" {
		return rel
	}
	return strings.TrimRight(mod, "/") + "/" + strings.TrimLeft(rel, "/")
}

// init registers the Frontend with api.Global. The blank import
// from mekami-core/frontend/all_gen/all_gen.go triggers this
// init at process start; the binary's main() then sees a
// fully-populated registry.
func init() {
	api.Register(Frontend{})
}
