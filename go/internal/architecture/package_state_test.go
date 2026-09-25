package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// statefulUnits keep their mutable state in a Runtime value, which a process
// builds once and a test can build fresh (llm-gateway#71).
var statefulUnits = []string{"internal/providers", "internal/router"}

const (
	documentedSeam     = "only mutable package-level state"
	documentedConstant = "constant after init"
)

// allowedVariable is a package-level variable a stateful unit may declare
// although nothing proves it immutable. The declaration's doc comment must
// contain documented, so the code states what the entry relies on.
type allowedVariable struct {
	documented string
	reason     string
}

// allowedPackageState lists those variables. Sentinel errors, compiled
// regular expressions, embedded files and blank identifiers need no entry.
// The list only shrinks: an entry whose variable is gone fails, so a removal
// is locked in, and an addition needs a reason that survives review.
var allowedPackageState = map[string]allowedVariable{
	"providers.installed":                 {documentedSeam, "the runtime seam: the Runtime that code without one of its own reaches through Current"},
	"router.installed":                    {documentedSeam, "the runtime seam: the router Runtime that code without one of its own reaches through Current"},
	"providers.ProviderTypes":             {documentedConstant, "the runtime types the registry overlay is validated against; only read"},
	"providers.providerRegistry":          {documentedConstant, "the effective registry, built once from the embedded overlay; only read"},
	"providers.codexStructuralChatFields": {documentedConstant, "a lookup table the Codex Chat adapter only reads"},
	"router.defaultPrices":                {documentedConstant, "the built-in price table the savings ledger only reads"},
}

type packageVariable struct {
	name string // package.identifier, or "init" for an init function
	line int
	doc  string
}

func TestStatefulUnitsKeepStateInTheirRuntime(t *testing.T) {
	declared := map[string]bool{}
	for _, unit := range statefulUnits {
		dir := filepath.Join("..", "..", filepath.FromSlash(unit))
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatalf("stateful unit %s is missing; update the list if it moved: %v", unit, err)
		}
		fset := token.NewFileSet()
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.ParseComments|parser.SkipObjectResolution)
			if err != nil {
				t.Fatalf("parse %s/%s: %v", unit, name, err)
			}
			for _, found := range packageState(fset, file) {
				location := unit + "/" + name + ":" + strconv.Itoa(found.line)
				allowed, ok := allowedPackageState[found.name]
				switch {
				case found.name == "init":
					t.Errorf("%s: declares an init function; build state in the Runtime's constructor instead", location)
				case !ok:
					t.Errorf("%s: package-level variable %s is shared by every caller in the process; move it into the package's Runtime (llm-gateway#71)", location, found.name)
				case !strings.Contains(found.doc, allowed.documented):
					t.Errorf("%s: %s is allowlisted as %q; its doc comment must say so", location, found.name, allowed.documented)
				}
				declared[found.name] = true
			}
		}
	}
	names := make([]string, 0, len(allowedPackageState))
	for name := range allowedPackageState {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if !declared[name] {
			t.Errorf("%s is allowlisted but no longer declared; remove the entry to lock in the progress", name)
		}
	}
}

// packageState returns the file's init functions and each package-level
// variable nothing proves immutable: all but blank identifiers, embedded
// files, sentinel errors and compiled regular expressions.
func packageState(fset *token.FileSet, file *ast.File) []packageVariable {
	var found []packageVariable
	for _, declaration := range file.Decls {
		switch typed := declaration.(type) {
		case *ast.FuncDecl:
			if typed.Recv == nil && typed.Name.Name == "init" {
				found = append(found, packageVariable{name: "init", line: fset.Position(typed.Pos()).Line})
			}
		case *ast.GenDecl:
			if typed.Tok != token.VAR {
				continue
			}
			for _, spec := range typed.Specs {
				value := spec.(*ast.ValueSpec)
				if embedsFile(typed.Doc) || embedsFile(value.Doc) {
					continue
				}
				doc := strings.Join(strings.Fields(typed.Doc.Text()+" "+value.Doc.Text()), " ")
				for index, name := range value.Names {
					if name.Name == "_" || (index < len(value.Values) && immutableByConstruction(value.Values[index])) {
						continue
					}
					found = append(found, packageVariable{
						name: file.Name.Name + "." + name.Name, line: fset.Position(name.Pos()).Line, doc: doc,
					})
				}
			}
		}
	}
	return found
}

// The unit scan passes only while nothing is left to report, so this fixture
// pins that the detector still sees each shape of package state.
func TestPackageStateDetection(t *testing.T) {
	const source = `package fixture

import (
	_ "embed"
	"errors"
	"regexp"
	"sync"
)

var mutable = map[string]int{}

var (
	grouped sync.Mutex
	// ErrSentinel is a sentinel error.
	ErrSentinel = errors.New("sentinel")
	pattern     = regexp.MustCompile("x")
)

//go:embed fixture.txt
var embedded []byte

var _ = errors.New

// documented is constant after
// init: nothing writes it.
var documented = []string{"a"}

var first, second = errors.New("x"), 2

func init() {}
`
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, parser.ParseComments|parser.SkipObjectResolution)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	docs := map[string]string{}
	for _, found := range packageState(fset, file) {
		names = append(names, found.name)
		docs[found.name] = found.doc
	}
	want := []string{"fixture.mutable", "fixture.grouped", "fixture.documented", "fixture.second", "init"}
	if strings.Join(names, " ") != strings.Join(want, " ") {
		t.Fatalf("found %v, want %v", names, want)
	}
	if !strings.Contains(docs["fixture.documented"], documentedConstant) {
		t.Fatalf("doc comment lines were not joined: %q", docs["fixture.documented"])
	}
}

func embedsFile(doc *ast.CommentGroup) bool {
	if doc == nil {
		return false
	}
	for _, comment := range doc.List {
		if strings.HasPrefix(comment.Text, "//go:embed ") {
			return true
		}
	}
	return false
}

func immutableByConstruction(value ast.Expr) bool {
	call, ok := value.(*ast.CallExpr)
	if !ok {
		return false
	}
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := selector.X.(*ast.Ident)
	if !ok {
		return false
	}
	switch pkg.Name + "." + selector.Sel.Name {
	case "errors.New", "regexp.MustCompile":
		return true
	}
	return false
}
