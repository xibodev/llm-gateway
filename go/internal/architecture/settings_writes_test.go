package architecture

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"io/fs"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// settingsSources are the config functions that hand out settings. Get and
// Snapshot return the published value every concurrent reader shares, so a
// write through it races requests ranging over the same maps; Provider
// returns a copy, where a write is silently lost. Either way the change
// belongs in config.Update or config.UpdateAndSave.
var settingsSources = map[string]bool{"Get": true, "Snapshot": true, "Provider": true}

// containerBuiltins write into their first argument.
var containerBuiltins = map[string]bool{"append": true, "clear": true, "copy": true, "delete": true}

// TestPublishedSettingsAreNeverWritten scans every package outside
// internal/config, tests included, since only config's writers may produce
// a new settings value.
func TestPublishedSettingsAreNeverWritten(t *testing.T) {
	root := filepath.Join("..", "..")
	owner := filepath.Join(root, "internal", "config")
	scanned := 0
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() {
			name := entry.Name()
			if path == owner || name == "testdata" || name == "node_modules" || (path != root && strings.HasPrefix(name, ".")) {
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
		configName := importedName(file, "llmgw/internal/config")
		if configName == "" {
			return nil
		}
		scanned++
		for _, finding := range settingsWrites(fset, file, configName) {
			relative, _ := filepath.Rel(root, finding.Filename)
			t.Errorf("%s:%d: %s writes into settings published by config.Get, config.Snapshot or config.Provider; change settings through config.Update or config.UpdateAndSave",
				filepath.ToSlash(relative), finding.Line, finding.target)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("scan module: %v", err)
	}
	if scanned == 0 {
		t.Fatal("no scanned file imports llmgw/internal/config; the scan root is wrong")
	}
}

func importedName(file *ast.File, path string) string {
	for _, spec := range file.Imports {
		if imported, err := strconv.Unquote(spec.Path.Value); err != nil || imported != path {
			continue
		}
		if spec.Name != nil {
			return spec.Name.Name
		}
		return path[strings.LastIndex(path, "/")+1:]
	}
	return ""
}

type settingsWrite struct {
	token.Position
	target string
}

// settingsWrites finds assignments, increments and container builtins whose
// target reaches through a selector, index or dereference into the result of
// a settings source, or into a variable bound directly from one. Rebinding
// such a variable writes nothing shared and is allowed. Parameters and
// declarations of the same name shadow a binding inside their block, and a
// closure inherits the bindings around it.
func settingsWrites(fset *token.FileSet, file *ast.File, configName string) []settingsWrite {
	declared := declaredNames(file)
	isSource := func(expr ast.Expr) bool {
		call, ok := ast.Unparen(expr).(*ast.CallExpr)
		if !ok {
			return false
		}
		selector, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return false
		}
		pkg, ok := selector.X.(*ast.Ident)
		return ok && pkg.Name == configName && !declared[pkg] && settingsSources[selector.Sel.Name]
	}
	// boundFrom reports whether the index-th name of a binding receives a
	// settings source's result; Snapshot and Provider return it first.
	boundFrom := func(index, names int, values []ast.Expr) bool {
		if len(values) == names {
			return isSource(values[index])
		}
		return len(values) == 1 && index == 0 && isSource(values[0])
	}
	var findings []settingsWrite
	reported := map[int]bool{}
	report := func(node ast.Node, target ast.Expr) {
		position := fset.Position(node.Pos())
		if !reported[position.Line] {
			reported[position.Line] = true
			findings = append(findings, settingsWrite{Position: position, target: types.ExprString(target)})
		}
	}
	var walk func(node ast.Node, bound map[string]bool)
	walk = func(node ast.Node, bound map[string]bool) {
		// reaches reports whether writing to target lands in published
		// settings. bare accepts the source or bound variable itself, as a
		// container builtin writes into its argument; an assignment must
		// reach through it.
		reaches := func(target ast.Expr, bare bool) bool {
			through := bare
			for {
				switch value := target.(type) {
				case *ast.ParenExpr:
					target = value.X
					continue
				case *ast.SelectorExpr:
					target = value.X
				case *ast.IndexExpr:
					target = value.X
				case *ast.IndexListExpr:
					target = value.X
				case *ast.StarExpr:
					target = value.X
				case *ast.SliceExpr:
					target = value.X
				case *ast.TypeAssertExpr:
					target = value.X
				case *ast.CallExpr:
					return through && isSource(value)
				case *ast.Ident:
					return through && bound[value.Name]
				default:
					return false
				}
				through = true
			}
		}
		ast.Inspect(node, func(current ast.Node) bool {
			if current != node {
				switch current.(type) {
				case *ast.FuncDecl, *ast.FuncLit, *ast.BlockStmt, *ast.IfStmt, *ast.ForStmt, *ast.RangeStmt,
					*ast.SwitchStmt, *ast.TypeSwitchStmt, *ast.SelectStmt, *ast.CaseClause, *ast.CommClause:
					walk(current, copyScope(bound))
					return false
				}
			}
			switch value := current.(type) {
			case *ast.FuncDecl:
				unbindFields(value.Recv, bound)
				unbindFields(value.Type.Params, bound)
				unbindFields(value.Type.Results, bound)
			case *ast.FuncLit:
				unbindFields(value.Type.Params, bound)
				unbindFields(value.Type.Results, bound)
			case *ast.RangeStmt:
				for _, target := range []ast.Expr{value.Key, value.Value} {
					if ident, ok := target.(*ast.Ident); ok && value.Tok == token.DEFINE {
						bound[ident.Name] = false
					} else if target != nil && reaches(target, false) {
						report(target, target)
					}
				}
			case *ast.AssignStmt:
				for index, target := range value.Lhs {
					ident, isIdent := target.(*ast.Ident)
					switch {
					case isIdent && value.Tok == token.DEFINE:
						bound[ident.Name] = boundFrom(index, len(value.Lhs), value.Rhs)
					case isIdent && value.Tok == token.ASSIGN:
						if boundFrom(index, len(value.Lhs), value.Rhs) {
							bound[ident.Name] = true
						}
					case reaches(target, false):
						report(value, target)
					}
				}
			case *ast.ValueSpec:
				for index, name := range value.Names {
					bound[name.Name] = boundFrom(index, len(value.Names), value.Values)
				}
			case *ast.IncDecStmt:
				if reaches(value.X, false) {
					report(value, value.X)
				}
			case *ast.CallExpr:
				if name, ok := value.Fun.(*ast.Ident); ok && containerBuiltins[name.Name] && len(value.Args) > 0 && reaches(value.Args[0], true) {
					report(value, value.Args[0])
				}
			}
			return true
		})
	}
	walk(file, map[string]bool{})
	return findings
}

func unbindFields(fields *ast.FieldList, bound map[string]bool) {
	if fields == nil {
		return
	}
	for _, field := range fields.List {
		for _, name := range field.Names {
			bound[name.Name] = false
		}
	}
}

// The module scan passes only while nothing writes into published settings,
// so these fixtures pin that the detector still sees each shape of write.
func TestSettingsWritesDetection(t *testing.T) {
	cases := []struct {
		name  string
		body  string
		found int
	}{
		{"index through Get", `func f(p *cfg.ProviderConfig) { cfg.Get().Providers["x"] = p }`, 1},
		{"field through bound Get", `func f() { s := cfg.Get(); s.OpenAICodexClientID = "x" }`, 1},
		{"var bound Get", `func f() { var s = cfg.Get(); s.APIKey = "x" }`, 1},
		{"increment", `func f() { cfg.Get().RateLimitPerMinute++ }`, 1},
		{"delete through Snapshot", `func f() { s, _ := cfg.Snapshot(); delete(s.Providers, "x") }`, 1},
		{"append assign", `func f() { s := cfg.Get(); s.APIKeys = append(s.APIKeys, "k") }`, 1},
		{"append into shared array", `func f() []string { return append(cfg.Get().APIKeys, "k") }`, 1},
		{"Provider in if", `func f() { if p, ok := cfg.Provider("x"); ok { p.Disabled = true } }`, 1},
		{"dereference", `func f() { p, _ := cfg.Provider("x"); *p.Timeout = 1 }`, 1},
		{"closure inherits", `func f() { s := cfg.Get(); go func() { s.APIKey = "x" }() }`, 1},
		{"rebinding", `func f() { s := cfg.Get(); s = nil; _ = s }`, 0},
		{"reading", `func f() int { s := cfg.Get(); n := len(s.Providers); n++; return n }`, 0},
		{"Update parameter shadows", `func f() { s := cfg.Get(); _ = s; cfg.Update(func(s *cfg.Settings) { s.APIKey = "x" }) }`, 0},
		{"inner block shadows", `func f() { s := cfg.Get(); _ = s; { s := &cfg.Settings{}; s.APIKey = "x" } }`, 0},
		{"if scope ends", `func f() { s := &cfg.Settings{}; if s := cfg.Get(); s != nil { _ = s }; s.APIKey = "x" }`, 0},
		{"other function", "func f() { s := cfg.Get(); _ = s }\nfunc g() { s := &cfg.Settings{}; s.APIKey = \"x\" }", 0},
		{"package name shadowed", `func f(cfg interface{ Get() *T }) { cfg.Get().Name = "x" }`, 0},
	}
	for _, tc := range cases {
		source := "package fixture\n\nimport cfg \"llmgw/internal/config\"\n\n" + tc.body + "\n"
		fset := token.NewFileSet()
		file, err := parser.ParseFile(fset, "fixture.go", source, parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: parse: %v", tc.name, err)
		}
		if found := settingsWrites(fset, file, importedName(file, "llmgw/internal/config")); len(found) != tc.found {
			t.Errorf("%s: found %d writes %v, want %d", tc.name, len(found), found, tc.found)
		}
	}
}
