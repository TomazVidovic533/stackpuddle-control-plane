package architecture_test

import (
	"bytes"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets/local"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/validation"
)

const (
	modulePath = "github.com/TomazVidovic533/stackpuddle-control-plane"
	repoRoot   = "../.."
)

func TestEveryProductionPackageExceptEngineIsInternal(t *testing.T) {
	t.Parallel()
	cmd := exec.Command("go", "list", "-f", "{{.ImportPath}}", "./...")
	cmd.Dir = repoRoot
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("go list: %v: %s", err, stderr.String())
	}
	var public []string
	for _, pkg := range strings.Fields(string(out)) {
		rest := strings.TrimPrefix(pkg, modulePath)
		if rest == pkg || strings.HasPrefix(rest, "/tests") || strings.HasPrefix(rest, "/internal") {
			continue
		}
		public = append(public, strings.TrimPrefix(rest, "/"))
	}
	if !slices.Equal(public, []string{"engine"}) {
		t.Errorf("the public packages are %v, want only engine", public)
	}
}

// productionFiles lists the non-test Go files under dirs, relative to the repository root.
func productionFiles(t *testing.T, dirs ...string) []string {
	t.Helper()
	var files []string
	for _, dir := range dirs {
		err := filepath.WalkDir(filepath.Join(repoRoot, dir), func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if !d.IsDir() && strings.HasSuffix(path, ".go") && !strings.HasSuffix(path, "_test.go") {
				rel, relErr := filepath.Rel(repoRoot, path)
				if relErr != nil {
					return relErr
				}
				files = append(files, filepath.ToSlash(rel))
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

// catalogFile is the one file outside a module folder that may name a module: the explicit catalog.
const catalogFile = "internal/modules/catalog.go"

func TestNoFileOutsideAModuleFolderNamesAModuleProduct(t *testing.T) {
	t.Parallel()
	modules := validation.Modules()
	if len(modules) == 0 {
		t.Fatal("validation.Modules() lists no module, so the census would check nothing")
	}
	for _, file := range productionFiles(t, "engine", "internal") {
		if strings.HasPrefix(file, "internal/modules/") && strings.Count(file, "/") >= 3 {
			continue // internal/modules/<module>/ is where a product name belongs
		}
		if file == catalogFile {
			continue // the one line per driver that registers it names the driver's package
		}
		data, err := os.ReadFile(filepath.Join(repoRoot, file))
		if err != nil {
			t.Fatal(err)
		}
		lower := strings.ToLower(string(data))
		for _, m := range modules {
			if strings.Contains(lower, strings.ToLower(m.String())) {
				t.Errorf("%s names the module product %q; only internal/modules/%s/ may", file, m, m)
			}
		}
	}
}

func TestOnlyTheStateStoreSetsTheWriteFaultSeam(t *testing.T) {
	t.Parallel()
	fset := token.NewFileSet()
	for _, file := range productionFiles(t, "engine", "internal") {
		if strings.HasPrefix(file, "internal/state/") || strings.HasPrefix(file, "internal/durable/") {
			continue
		}
		parsed, err := parser.ParseFile(fset, filepath.Join(repoRoot, file), nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(parsed, func(n ast.Node) bool {
			switch node := n.(type) {
			case *ast.KeyValueExpr:
				if ident, ok := node.Key.(*ast.Ident); ok && ident.Name == "AfterStage" {
					t.Errorf("%s sets AfterStage, a seam only tests may set", fset.Position(node.Pos()))
				}
			case *ast.SelectorExpr:
				if node.Sel.Name == "AfterStage" {
					t.Errorf("%s uses AfterStage, a seam only tests may set", fset.Position(node.Pos()))
				}
			}
			return true
		})
	}
}

// TestAdvertiseEqualsTheCatalogTheRuntimeAndTheProvidersInBothDirections builds the engine core from
// independent sources and requires that Advertise lists exactly their capabilities: nothing a source
// lacks, and nothing one provides left out.
func TestAdvertiseEqualsTheCatalogTheRuntimeAndTheProvidersInBothDirections(t *testing.T) {
	t.Parallel()
	const runtimeCapability = "runtime.compose/1"
	provider := local.New(local.Options{Dir: t.TempDir(), KeySource: nil})
	driver := drivers.KV{}
	tests := []struct {
		name      string
		catalog   bool
		runtime   string
		providers map[string]secrets.Provider
		want      []string
	}{
		{"all three sources", true, runtimeCapability, map[string]secrets.Provider{provider.ID(): provider}, append(driver.Describe().Capabilities, runtimeCapability, "secrets.local/1")},
		{"no catalog", false, runtimeCapability, map[string]secrets.Provider{provider.ID(): provider}, []string{runtimeCapability, "secrets.local/1"}},
		{"no provider", true, runtimeCapability, nil, append(driver.Describe().Capabilities, runtimeCapability)},
		{"another runtime", true, "runtime.other/1", map[string]secrets.Provider{provider.ID(): provider}, append(driver.Describe().Capabilities, "runtime.other/1", "secrets.local/1")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			deps := operation.Deps{Runtime: testkit.NewFakeRuntime(), Providers: test.providers, Random: testkit.NewRandom("arch"), Version: "x", RuntimeCapability: test.runtime}
			if test.catalog {
				deps.Catalog = drivers.Catalog()
			}
			eng, err := operation.New(deps)
			if err != nil {
				t.Fatal(err)
			}
			info, err := eng.Advertise()
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, c := range info.Capabilities {
				got = append(got, c.String())
			}
			slices.Sort(got)
			want := slices.Clone(test.want)
			slices.Sort(want)
			if !slices.Equal(got, want) {
				t.Errorf("Advertise = %v, want %v", got, want)
			}
		})
	}
}

func TestTwoDriversForOneModuleAreRefused(t *testing.T) {
	t.Parallel()
	_, err := operation.New(operation.Deps{Catalog: append(drivers.Catalog(), drivers.KV{}), Random: testkit.NewRandom("dup")})
	if err == nil {
		t.Error("two drivers for one module were accepted")
	}
}
