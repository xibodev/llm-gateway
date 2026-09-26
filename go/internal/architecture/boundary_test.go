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

// productLayers are the gateway's storage, configuration and HTTP layers.
// Entries match the import path or any subpackage of it.
var productLayers = []string{
	"database/sql",
	"llmgw/internal/api",
	"llmgw/internal/config",
	"llmgw/internal/iam",
	"modernc.org/sqlite",
}

// adapterUnit is a package directory or a single file, relative to the module
// root, of the gateway's product adapter layer, where the gateway's settings,
// IAM and stores meet llmgw-core. What the libraries own has moved to them
// (llm-gateway#75); a unit keeps the gateway's side of each seam, so it may
// import the product layers it lists and no others.
type adapterUnit struct {
	path string
	// allowed are the product layers the unit may import. A new one fails,
	// and so does an entry the unit no longer imports, so every removal is
	// locked in.
	allowed []string
	// budget caps references to product storage and configuration. Counts
	// above budget fail; counts below budget fail until the budget is
	// lowered, so the coupling only shrinks.
	budget map[string]int
}

// productAdapters are the adapter units, each with the reason it keeps the
// references its budget counts. None reads config.Principal: a caller
// reaches them as a core.Caller.
var productAdapters = []adapterUnit{
	{
		// The gateway's side of llmgw-core's provider stack: the vertical
		// table and the Runtime's assembly, the facades, the stores over
		// IAM, the catalog file store, the OAuth drivers, probes and evidence.
		//
		// iam.*: the credential store the core Runtime resolves and
		// refreshes in (core_runtime.go) and the factory's resolution order
		// and authorization check (factory.go); the console's OAuth
		// contracts and refreshes, which hand IAM envelopes and connections
		// to the API layer (auth_adapter.go, codex.go,
		// antigravity_credentials.go); the catalog's credential-revision
		// fence (catalog.go, credential_observation.go); the publication
		// gate of anonymous models over IAM evidence
		// (anonymous_publication.go); checks and quota snapshots in IAM's
		// shapes (provider_contract_adapter.go, quota_adapter.go); and a
		// caller's IAM principal (caller.go).
		//
		// config.Get(): one snapshot per facade build, read after the
		// instance cache's epoch, and the disabled check ahead of that cache
		// (factory.go, proxy.go); the exported predicates and reads the API
		// layer and the router call without a snapshot (catalog.go,
		// factory.go, auth_adapter.go, copilot_client.go); and the reads
		// that must see a change at runtime: the Copilot client's settings
		// and session headers (copilot_client.go, copilot_provider.go) and
		// the Antigravity OAuth client of sign-in and of the gateway's own
		// refresh (auth_adapter.go, antigravity_credentials.go).
		path:    "internal/providers",
		allowed: []string{"llmgw/internal/config", "llmgw/internal/iam"},
		budget:  map[string]int{metricIAM: 27, metricConfigGet: 8, metricConfigPrincipal: 0},
	},
	{
		// The gateway's routing policy over llmgw-core's execution
		// primitives. Telemetry and the savings ledger own the SQLite
		// imports.
		//
		// iam.*: project policy and model evidence in route and alias
		// resolution (failover.go), and the IAM usage event each request
		// records (savings.go).
		//
		// config.Get(): one snapshot per resolution, per alias table and per
		// ledger operation, and the per-target provider lookups of
		// capability filtering and of the Anthropic and Responses fallbacks
		// (failover.go, savings.go).
		path:    "internal/router",
		allowed: []string{"database/sql", "llmgw/internal/config", "llmgw/internal/iam", "modernc.org/sqlite"},
		budget:  map[string]int{metricIAM: 6, metricConfigGet: 10, metricConfigPrincipal: 0},
	},
	{
		// Transparent-mode planning, composed from llmgw-core's transport
		// helpers. config.Get(): the check that the exact target names a
		// configured provider, a refusal the gateway words itself.
		path:    "internal/api/transport_mode.go",
		allowed: []string{"llmgw/internal/config"},
		budget:  map[string]int{metricIAM: 0, metricConfigGet: 1, metricConfigPrincipal: 0},
	},
}

type unitFacts struct {
	imports map[string]bool
	counts  map[string]int
}

func TestProductAdaptersImportOnlyTheirLayers(t *testing.T) {
	for _, unit := range productAdapters {
		facts := inspectUnit(t, unit.path)
		var present []string
		for path := range facts.imports {
			if isProductLayer(path) {
				present = append(present, path)
			}
		}
		sort.Strings(present)
		for _, path := range present {
			if !slices.Contains(unit.allowed, path) {
				t.Errorf("%s gained product import %q; keep storage behind internal/iam and HTTP in internal/api, or record why the unit needs the layer in productAdapters (llm-gateway#75)", unit.path, path)
			}
		}
		for _, path := range unit.allowed {
			if !slices.Contains(present, path) {
				t.Errorf("%s no longer imports %q; remove it from the allowed list to lock in the progress", unit.path, path)
			}
		}
	}
}

func TestProductAdapterCouplingOnlyShrinks(t *testing.T) {
	for _, unit := range productAdapters {
		facts := inspectUnit(t, unit.path)
		for _, metric := range []string{metricIAM, metricConfigGet, metricConfigPrincipal} {
			got, budget := facts.counts[metric], unit.budget[metric]
			switch {
			case got > budget:
				t.Errorf("%s has %d %s references, over its budget of %d; pass the value or the operation's settings snapshot in instead (llm-gateway#75)", unit.path, got, metric, budget)
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
		t.Fatalf("adapter unit %s is missing; update productAdapters if it moved: %v", relative, err)
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

func isProductLayer(path string) bool {
	for _, layer := range productLayers {
		if path == layer || strings.HasPrefix(path, layer+"/") {
			return true
		}
	}
	return false
}
