package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"maps"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
)

const (
	corePath          = "github.com/xibodev/llmgw-core"
	coreProvidersPath = "github.com/xibodev/llmgw-core/providers"
	coreZenPath       = "github.com/xibodev/llmgw-core/providers/zen"
)

// corePackageNames are the names the core packages declare, which an import
// without a name binds; the module root's is not its last path element.
var corePackageNames = map[string]string{corePath: "core", coreProvidersPath: "providers", coreZenPath: "zen"}

// coreAPI is an identifier an llmgw-core package declares, or, with a
// receiver, a method of that type. Nothing here knows the type a selector
// reads, so a method matches by name wherever it is selected.
type coreAPI struct {
	path, receiver, name string
}

func (api coreAPI) String() string {
	if api.receiver != "" {
		return corePackageNames[api.path] + "." + api.receiver + "." + api.name
	}
	return corePackageNames[api.path] + "." + api.name
}

func coreAPIs(path, receiver string, names ...string) []coreAPI {
	apis := make([]coreAPI, 0, len(names))
	for _, name := range names {
		apis = append(apis, coreAPI{path: path, receiver: receiver, name: name})
	}
	return apis
}

// retiredCoreAPIs are what the pinned llmgw-core deprecates for a
// replacement the gateway has adopted. No gateway file, tests included, may
// use one again, so the release that removes them builds unchanged. Methods
// are listed only under names nothing else in the gateway selects: zen's
// Client.Discover is not, because the anonymous orchestrator's Catalog has
// a Discover of its own.
var retiredCoreAPIs = slices.Concat(
	// Principal: new APIs take a core.Caller.
	coreAPIs(corePath, "", "Principal"),
	// The session-source Codex provider, replaced by Codex, which the
	// gateway's Codex vertical serves.
	coreAPIs(coreProvidersPath, "", "CodexProvider", "CodexProviderConfig", "CodexSession",
		"CodexSessionSource", "CodexSessionSourceFunc", "NewCodexProvider", "NewCodexTokenSessionSource"),
	// The experimental Antigravity provider, replaced by Antigravity.
	coreAPIs(coreProvidersPath, "", "AntigravityProjectObserver", "AntigravityTokenSource",
		"AntigravityUnauthorizedHandler", "ExperimentalAntigravityProvider", "NewExperimentalAntigravityProvider"),
	// The roster probe, replaced by the anonymous orchestrator.
	coreAPIs(coreProvidersPath, "", "AutoConnectAnonymousProviders", "AutoConnectResult"),
	// zen.Client's request paths, replaced by providers.Zen.
	coreAPIs(coreZenPath, "Client", "AnonymousHeaders", "AnonymousHeadersFor", "CompleteNative"),
)

// deprecatedUse confines a deprecated API the gateway still needs to one
// file, relative to the module root, for reason.
type deprecatedUse struct {
	file, reason string
}

// deprecatedCoreAPIsInUse are the deprecated APIs the gateway still needs.
// A use outside its file fails, and so does an entry nothing uses any more,
// so a removal is locked in: move the entry to retiredCoreAPIs then.
var deprecatedCoreAPIsInUse = map[coreAPI]deprecatedUse{
	{path: coreProvidersPath, name: "NewAnonymousOpenAICompatibleAdapter"}: {
		file:   "internal/providers/provider_orchestrator.go",
		reason: "the keyless probe of the anonymous profiles whose API it speaks; core deprecates it as the orchestrator's adapter, which it no longer is",
	},
	{path: coreZenPath, receiver: "Client", name: "DiscoverVerified"}: {
		file:   "internal/providers/opencode_zen.go",
		reason: "the verified anonymous OpenCode Zen catalog, whose failures the gateway reports under its own catalog codes",
	},
}

// TestDeprecatedCoreAPIsStayRetired scans every Go file of the module, tests
// included: a test that names a removed API fails the upgrade's build as
// surely as product code does.
func TestDeprecatedCoreAPIsStayRetired(t *testing.T) {
	inUse := slices.SortedFunc(maps.Keys(deprecatedCoreAPIsInUse), func(a, b coreAPI) int {
		return strings.Compare(a.String(), b.String())
	})
	apis := slices.Concat(retiredCoreAPIs, inUse)
	uses := map[coreAPI][]string{}
	root := filepath.Join("..", "..")
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == "testdata" || name == "node_modules" || (path != root && strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") {
			return nil
		}
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		relative, _ := filepath.Rel(root, path)
		for api, lines := range coreAPIRefs(fset, file, apis) {
			for _, line := range lines {
				uses[api] = append(uses[api], filepath.ToSlash(relative)+":"+strconv.Itoa(line))
			}
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module: %v", err)
	}
	for _, api := range retiredCoreAPIs {
		for _, at := range uses[api] {
			t.Errorf("%s: uses %s, which llmgw-core deprecates for a replacement the gateway has adopted; use the replacement (llm-gateway#75)", at, api)
		}
	}
	for _, api := range inUse {
		allowed := deprecatedCoreAPIsInUse[api]
		if len(uses[api]) == 0 {
			t.Errorf("%s is no longer used; move it from deprecatedCoreAPIsInUse to retiredCoreAPIs to lock in the progress", api)
		}
		for _, at := range uses[api] {
			if !strings.HasPrefix(at, allowed.file+":") {
				t.Errorf("%s: uses %s, which llmgw-core deprecates; only %s may, as %s", at, api, allowed.file, allowed.reason)
			}
		}
	}
}

// coreAPIRefs returns the lines of file that use each of apis: for an
// identifier, a selector on the name its package is imported under, unless
// a declaration of that name shadows the import there; for a method, any
// selector of the method's name.
func coreAPIRefs(fset *token.FileSet, file *ast.File, apis []coreAPI) map[coreAPI][]int {
	locals := map[string]string{}
	for _, spec := range file.Imports {
		path, err := strconv.Unquote(spec.Path.Value)
		if err != nil || corePackageNames[path] == "" {
			continue
		}
		locals[path] = corePackageNames[path]
		if spec.Name != nil {
			locals[path] = spec.Name.Name
		}
	}
	declared := declaredNames(file)
	found := map[coreAPI][]int{}
	ast.Inspect(file, func(node ast.Node) bool {
		selector, ok := node.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		for _, api := range apis {
			if selector.Sel.Name != api.name {
				continue
			}
			ident, isIdent := selector.X.(*ast.Ident)
			qualified := isIdent && locals[api.path] != "" && ident.Name == locals[api.path] && !declared[ident]
			if api.receiver != "" || qualified {
				found[api] = append(found[api], fset.Position(selector.Pos()).Line)
			}
		}
		return true
	})
	return found
}

// The module scan passes only while nothing is left to report, so this
// fixture pins that the detector still sees an identifier under the name an
// unaliased or aliased import binds and a method by name, and not a
// parameter that shadows an import.
func TestCoreAPIUseDetection(t *testing.T) {
	const source = `package fixture

import (
	"github.com/xibodev/llmgw-core"
	legacy "github.com/xibodev/llmgw-core/providers"
)

var _ *core.Principal

func f(client interface{ CompleteNative() }) {
	_, _ = legacy.NewCodexProvider(legacy.CodexProviderConfig{})
	client.CompleteNative()
}

func g(legacy struct{ NewCodexProvider int }) int { return legacy.NewCodexProvider }
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	apis := slices.Concat(
		coreAPIs(corePath, "", "Principal"),
		coreAPIs(coreProvidersPath, "", "NewCodexProvider", "CodexProviderConfig", "AutoConnectResult"),
		coreAPIs(coreZenPath, "Client", "CompleteNative"),
	)
	got := map[string][]int{}
	for api, lines := range coreAPIRefs(fset, file, apis) {
		got[api.String()] = lines
	}
	want := map[string][]int{
		"core.Principal": {8}, "providers.NewCodexProvider": {11}, "providers.CodexProviderConfig": {11},
		"zen.Client.CompleteNative": {12},
	}
	if !maps.EqualFunc(got, want, slices.Equal[[]int]) {
		t.Fatalf("found %v, want %v", got, want)
	}
}
