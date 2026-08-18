package web

import (
	"embed"
	"io/fs"
)

// AdminHTML is the preserved legacy admin dashboard.
//
//go:embed admin.html
var AdminHTML string

// PortalHTML is the preserved legacy self-service portal.
//
//go:embed portal.html
var PortalHTML string

// ConsoleFS holds the generated, local-only Preact console assets. The build
// output is committed so a normal Go build has no Node.js runtime dependency.
//
//go:embed console/dist
var ConsoleFS embed.FS

// ConsoleAsset reads one safe generated asset by its dist-relative name.
func ConsoleAsset(name string) ([]byte, error) {
	if !fs.ValidPath(name) {
		return nil, fs.ErrNotExist
	}
	return ConsoleFS.ReadFile("console/dist/" + name)
}

// ConsoleIndex returns the SPA entry document.
func ConsoleIndex() []byte {
	data, err := ConsoleAsset("index.html")
	if err != nil {
		return []byte("console assets are unavailable")
	}
	return data
}
