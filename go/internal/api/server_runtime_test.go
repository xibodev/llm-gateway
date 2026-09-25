package api

import (
	"testing"

	"llmgw/internal/providers"
	"llmgw/internal/router"
)

func TestServerActsOnItsRuntime(t *testing.T) {
	explicit := Runtime{Providers: providers.NewRuntime(), Router: router.NewRuntime(nil)}
	owner := &server{runtime: explicit}
	if owner.providers() != explicit.Providers || owner.router() != explicit.Router {
		t.Fatal("a server built with runtimes must act on them")
	}
	installedProviders := providers.InstallForTests(t)
	installedRouter := router.InstallForTests(t)
	following := &server{}
	if following.providers() != installedProviders || following.router() != installedRouter {
		t.Fatal("a server built without runtimes must follow the installed ones")
	}
}
