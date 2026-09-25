package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
)

const (
	metricIAM             = "iam.*"
	metricConfigGet       = "config.Get()"
	metricConfigPrincipal = "config.Principal"
)

// forbiddenImports are the product layers that shared-library code must not
// depend on. Entries match the import path or any subpackage of it.
var forbiddenImports = []string{
	"database/sql",
	"llmgw/internal/api",
	"llmgw/internal/config",
	"llmgw/internal/iam",
	"modernc.org/sqlite",
}

// boundUnit is a package directory or a single file, relative to the module
// root, whose code moves to the shared libraries.
type boundUnit struct {
	path string
	// allowed is today's debt: forbidden imports the unit still has. A new
	// forbidden import fails, and so does an entry that is no longer needed,
	// so every removal is locked in.
	allowed []string
	// budget caps references to product storage and configuration. Counts
	// above budget fail; counts below budget fail until the budget is
	// lowered, so progress cannot silently regress.
	budget map[string]int
}

var coreBound = []boundUnit{
	{
		path:    "internal/providers",
		allowed: []string{"llmgw/internal/config", "llmgw/internal/iam"},
		budget:  map[string]int{metricIAM: 58, metricConfigGet: 20, metricConfigPrincipal: 0},
	},
	{
		// Telemetry and the savings ledger own the SQLite imports; they stay
		// in the gateway when the routing primitives move.
		path:    "internal/router",
		allowed: []string{"database/sql", "llmgw/internal/config", "llmgw/internal/iam", "modernc.org/sqlite"},
		budget:  map[string]int{metricIAM: 6, metricConfigGet: 15, metricConfigPrincipal: 0},
	},
	{
		path:    "internal/api/transport_mode.go",
		allowed: []string{"llmgw/internal/config"},
		budget:  map[string]int{metricIAM: 0, metricConfigGet: 1, metricConfigPrincipal: 0},
	},
}

type unitFacts struct {
	imports map[string]bool
	counts  map[string]int
}

func TestCoreBoundImportsOnlyShrink(t *testing.T) {
	for _, unit := range coreBound {
		facts := inspectUnit(t, unit.path)
		var present []string
		for path := range facts.imports {
			if isForbidden(path) {
				present = append(present, path)
			}
		}
		sort.Strings(present)
		for _, path := range present {
			if !slices.Contains(unit.allowed, path) {
				t.Errorf("%s gained forbidden import %q; shared-library code must not depend on gateway storage, configuration or HTTP layers (llm-gateway#67)", unit.path, path)
			}
		}
		for _, path := range unit.allowed {
			if !slices.Contains(present, path) {
				t.Errorf("%s no longer imports %q; remove it from the allowed list to lock in the progress", unit.path, path)
			}
		}
	}
}

func TestCoreBoundCouplingBudget(t *testing.T) {
	for _, unit := range coreBound {
		facts := inspectUnit(t, unit.path)
		for _, metric := range []string{metricIAM, metricConfigGet, metricConfigPrincipal} {
			got, budget := facts.counts[metric], unit.budget[metric]
			switch {
			case got > budget:
				t.Errorf("%s has %d %s references, over its budget of %d; inject the dependency instead (llm-gateway#67)", unit.path, got, metric, budget)
			case got < budget:
				t.Errorf("%s has %d %s references, under its budget of %d; lower the budget to %d to lock in the progress", unit.path, got, metric, budget, got)
			}
		}
	}
}

func inspectUnit(t *testing.T, relative string) unitFacts {
	t.Helper()
	root := filepath.Join("..", "..")
	target := filepath.Join(root, filepath.FromSlash(relative))
	info, err := os.Stat(target)
	if err != nil {
		t.Fatalf("core-bound unit %s is missing; update the boundary list if it moved: %v", relative, err)
	}
	var files []string
	if info.IsDir() {
		entries, err := os.ReadDir(target)
		if err != nil {
			t.Fatalf("read %s: %v", relative, err)
		}
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			files = append(files, filepath.Join(target, name))
		}
	} else {
		files = []string{target}
	}
	facts := unitFacts{imports: map[string]bool{}, counts: map[string]int{}}
	fset := token.NewFileSet()
	for _, file := range files {
		parsed, err := parser.ParseFile(fset, file, nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		locals := map[string]string{}
		for _, spec := range parsed.Imports {
			path, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("import path in %s: %v", file, err)
			}
			facts.imports[path] = true
			name := path[strings.LastIndex(path, "/")+1:]
			if spec.Name != nil {
				name = spec.Name.Name
			}
			locals[path] = name
		}
		countReferences(parsed, locals["llmgw/internal/iam"], locals["llmgw/internal/config"], facts.counts)
	}
	return facts
}

// countReferences counts package-qualified references. An identifier that is
// declared anywhere in the file (a parameter or local named like the package)
// shadows the import there, so such files fall back to excluding selectors on
// declared names.
func countReferences(file *ast.File, iamName, configName string, counts map[string]int) {
	declared := declaredNames(file)
	isPackage := func(expr ast.Expr, name string) bool {
		ident, ok := expr.(*ast.Ident)
		return ok && name != "" && ident.Name == name && !declared[ident]
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch value := node.(type) {
		case *ast.CallExpr:
			if selector, ok := value.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Get" && isPackage(selector.X, configName) {
				counts[metricConfigGet]++
			}
		case *ast.SelectorExpr:
			if isPackage(value.X, iamName) {
				counts[metricIAM]++
			}
			if value.Sel.Name == "Principal" && isPackage(value.X, configName) {
				counts[metricConfigPrincipal]++
			}
		}
		return true
	})
}

// declaredNames marks identifiers that resolve to a local declaration rather
// than an imported package, using lexical scopes built from the file.
func declaredNames(file *ast.File) map[*ast.Ident]bool {
	shadowed := map[*ast.Ident]bool{}
	var walk func(node ast.Node, scope map[string]bool)
	walk = func(node ast.Node, scope map[string]bool) {
		ast.Inspect(node, func(current ast.Node) bool {
			switch value := current.(type) {
			case *ast.FuncDecl:
				inner := copyScope(scope)
				declareFields(value.Recv, inner)
				declareFields(value.Type.Params, inner)
				declareFields(value.Type.Results, inner)
				if value.Body != nil {
					walk(value.Body, inner)
				}
				return false
			case *ast.FuncLit:
				inner := copyScope(scope)
				declareFields(value.Type.Params, inner)
				declareFields(value.Type.Results, inner)
				walk(value.Body, inner)
				return false
			case *ast.AssignStmt:
				if value.Tok == token.DEFINE {
					for _, lhs := range value.Lhs {
						if ident, ok := lhs.(*ast.Ident); ok {
							scope[ident.Name] = true
						}
					}
				}
			case *ast.ValueSpec:
				for _, name := range value.Names {
					scope[name.Name] = true
				}
			case *ast.RangeStmt:
				for _, expr := range []ast.Expr{value.Key, value.Value} {
					if ident, ok := expr.(*ast.Ident); ok && value.Tok == token.DEFINE {
						scope[ident.Name] = true
					}
				}
			case *ast.SelectorExpr:
				if ident, ok := value.X.(*ast.Ident); ok && scope[ident.Name] {
					shadowed[ident] = true
				}
			}
			return true
		})
	}
	walk(file, map[string]bool{})
	return shadowed
}

func declareFields(fields *ast.FieldList, scope map[string]bool) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			scope[name.Name] = true
		}
	}
}

func copyScope(scope map[string]bool) map[string]bool {
	inner := make(map[string]bool, len(scope))
	for name, declared := range scope {
		inner[name] = declared
	}
	return inner
}

func isForbidden(path string) bool {
	for _, forbidden := range forbiddenImports {
		if path == forbidden || strings.HasPrefix(path, forbidden+"/") {
			return true
		}
	}
	return false
}
