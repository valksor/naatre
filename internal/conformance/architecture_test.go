package conformance_test

import (
	"go/build"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const modulePath = "github.com/valksor/naatre"

// layers records the documented dependency direction. A package may import
// only packages in a strictly lower layer, which enforces the README's
// one-way boundary and makes an import cycle unrepresentable rather than
// merely unlikely.
//
// The portable protocol and schema packages sit innermost precisely so they
// can never reach a transport or an application runtime.
var layers = []struct {
	prefix string
	layer  int
}{
	{"tooling/lsp", 4},
	{"internal/slicesx", -1},
	{"protocol", 0},
	{"schema", 1},
	{"client", 2},
	{"collectionquery", 2},
	{"mutation", 2},
	{"normalizedcache", 2},
	{"event", 2},
	{"generator", 2},
	{"remoteworker", 2},
	{"runtime", 2},
	{"tooling", 3},
	{"generatorplugin", 3},
	{"sdk", 3},
	{"observability", 3},
	{"asyncoperation", 3},
	{"reflectadapter", 3},
	{"interopadapter", 3},
	{"playground", 4},
	{"graphqladapter", 4},
	{"openapiadapter", 4},
	{"openrpcadapter", 4},
	{"mcpadapter", 4},
	{"transport", 4},
	{"largevalue", 5},
	{"examples", 5},
	{"internal/qualityharness", 5},
	{"internal", 6},
	{"cmd", 7},
}

func layerOf(t *testing.T, relative string) int {
	t.Helper()
	for _, candidate := range layers {
		if relative == candidate.prefix || strings.HasPrefix(relative, candidate.prefix+"/") {
			return candidate.layer
		}
	}
	t.Fatalf("package %q belongs to no documented layer; add it to the README boundary and to this table", relative)
	return 0
}

// The dependency direction documented in the README is enforced, not assumed.
// Go rejects an import cycle on its own, but nothing otherwise stops a portable
// package from reaching inward-to-outward.
func TestPackageDependenciesFollowTheDocumentedDirection(t *testing.T) {
	t.Parallel()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolve module root: %v", err)
	}
	packages := modulePackages(t, root)
	if len(packages) < 4 {
		t.Fatalf("found %d packages, want the documented module layout", len(packages))
	}
	for _, relative := range packages {
		t.Run(relative, func(t *testing.T) {
			imported := build.Default
			// Importing the directory is enough to read its declared imports;
			// this check does not need to resolve external dependencies.
			pkg, err := imported.ImportDir(filepath.Join(root, relative), 0)
			if err != nil {
				t.Fatalf("read package %s: %v", relative, err)
			}
			own := layerOf(t, relative)
			assertImportDirection(t, relative, own, pkg.Imports, false)
			// Test files may reach their own layer, since an external test
			// package imports the package under test.
			assertImportDirection(t, relative, own, append(pkg.TestImports, pkg.XTestImports...), true)
		})
	}
}

func assertImportDirection(t *testing.T, relative string, own int, imports []string, test bool) {
	t.Helper()
	for _, imported := range imports {
		if !strings.HasPrefix(imported, modulePath) {
			continue
		}
		target := strings.TrimPrefix(strings.TrimPrefix(imported, modulePath), "/")
		if target == "" || target == relative {
			continue
		}
		other := layerOf(t, target)
		if other < own || (test && other == own) {
			continue
		}
		kind := "package"
		if test {
			kind = "test"
		}
		t.Errorf("%s %s imports %s: layer %d must not depend on layer %d, which reverses the documented direction",
			relative, kind, target, own, other)
	}
}

// modulePackages lists every directory in the module that holds Go source,
// so a new package cannot silently escape the boundary check.
func modulePackages(t *testing.T, root string) []string {
	t.Helper()
	var packages []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.IsDir() {
			return nil
		}
		name := entry.Name()
		if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
			return filepath.SkipDir
		}
		entries, readErr := os.ReadDir(path)
		if readErr != nil {
			return readErr
		}
		for _, candidate := range entries {
			if !candidate.IsDir() && strings.HasSuffix(candidate.Name(), ".go") {
				relative, relErr := filepath.Rel(root, path)
				if relErr != nil {
					return relErr
				}
				packages = append(packages, filepath.ToSlash(relative))
				break
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk module: %v", err)
	}
	return packages
}
