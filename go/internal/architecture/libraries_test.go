package architecture

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// libraries are the modules the gateway builds on: llmgw-core, which
// depends on the other two, and llm-provider-auth and llm-translate, whose
// public packages the gateway also imports. Go keeps their internal
// packages out of reach, so what is left to check is which release of each
// the gateway builds.
var libraries = []string{
	"github.com/xibodev/llm-provider-auth",
	"github.com/xibodev/llm-translate",
	"github.com/xibodev/llmgw-core",
}

// releaseTag is a release version, vMAJOR.MINOR.PATCH without a prerelease
// or build suffix. A pseudo-version names an untagged commit and does not
// match.
var releaseTag = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)

// TestLibrariesArePinnedAtReleaseTags reads go.mod: production builds run
// with GOWORK=off, so a replace directive or an untagged version would ship
// a checkout nobody released (llm-gateway#75).
func TestLibrariesArePinnedAtReleaseTags(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("..", "..", "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}
	requires, replaces, err := goModDirectives(string(source))
	if err != nil {
		t.Fatalf("go.mod: %v", err)
	}
	for _, replace := range replaces {
		t.Errorf("go.mod:%s; a replace directive must not be committed, so require a released version instead", replace)
	}
	for _, library := range libraries {
		require, found := requires[library]
		switch {
		case !found:
			t.Errorf("go.mod does not require %s", library)
		case require.indirect:
			t.Errorf("go.mod requires %s only indirectly; the gateway imports it, so require it directly", library)
		case !releaseTag.MatchString(require.version):
			t.Errorf("go.mod requires %s at %s; pin a release tag vMAJOR.MINOR.PATCH", library, require.version)
		}
	}
}

type requirement struct {
	version  string
	indirect bool
}

// goModDirectives returns the require and replace directives of a go.mod
// file in the line-oriented form the go command writes: a directive on a
// line of its own or in a parenthesized block. Each replace is reported as
// its line and the module it replaces.
func goModDirectives(source string) (map[string]requirement, []string, error) {
	requires := map[string]requirement{}
	var replaces []string
	block := ""
	for index, raw := range strings.Split(source, "\n") {
		line, comment, _ := strings.Cut(raw, "//")
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		verb := block
		if block != "" && fields[0] == ")" {
			block = ""
			continue
		}
		if block == "" {
			verb, fields = fields[0], fields[1:]
			if len(fields) == 1 && fields[0] == "(" {
				block = verb
				continue
			}
		}
		switch verb {
		case "require":
			if len(fields) != 2 {
				return nil, nil, fmt.Errorf("line %d: malformed require %q", index+1, strings.TrimSpace(raw))
			}
			indirect := strings.HasPrefix(strings.TrimSpace(comment), "indirect")
			requires[fields[0]] = requirement{version: fields[1], indirect: indirect}
		case "replace":
			if len(fields) == 0 {
				return nil, nil, fmt.Errorf("line %d: malformed replace", index+1)
			}
			replaces = append(replaces, fmt.Sprintf("%d replaces %s", index+1, fields[0]))
		}
	}
	return requires, replaces, nil
}

// The go.mod check passes only while nothing is wrong, so this fixture pins
// that the parser still reads each shape of directive and that a release
// tag is told from the versions that are not one.
func TestGoModDirectivesDetection(t *testing.T) {
	const source = `module fixture

go 1.26

require example.com/single v1.2.3

require (
	example.com/tagged v0.14.1
	example.com/pseudo v0.0.0-20260101000000-abcdef123456
	example.com/indirect v1.0.0 // indirect
)

replace example.com/single => ../single

replace (
	example.com/tagged v0.14.1 => example.com/fork v0.14.2
)
`
	requires, replaces, err := goModDirectives(strings.ReplaceAll(source, "\n", "\r\n"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]requirement{
		"example.com/single":   {version: "v1.2.3"},
		"example.com/tagged":   {version: "v0.14.1"},
		"example.com/pseudo":   {version: "v0.0.0-20260101000000-abcdef123456"},
		"example.com/indirect": {version: "v1.0.0", indirect: true},
	}
	if !maps.Equal(requires, want) {
		t.Errorf("requires = %v, want %v", requires, want)
	}
	if got := strings.Join(replaces, "; "); got != "13 replaces example.com/single; 16 replaces example.com/tagged" {
		t.Errorf("replaces = %q", got)
	}
	for version, tagged := range map[string]bool{
		"v0.14.1": true, "v10.0.0": true, "v0.0.0-20260101000000-abcdef123456": false,
		"v1.2.3-rc.1": false, "v2.0.0+incompatible": false, "v01.2.3": false, "0.14.1": false,
	} {
		if got := releaseTag.MatchString(version); got != tagged {
			t.Errorf("releaseTag matches %q = %v, want %v", version, got, tagged)
		}
	}
}
