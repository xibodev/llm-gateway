package router

import (
	"testing"

	"llmgw/internal/providers"
)

func TestRouterRuntimeActsOnItsProviderRuntime(t *testing.T) {
	explicit := providers.NewRuntime()
	if NewRuntime(explicit).providers() != explicit {
		t.Fatal("a router Runtime built with a provider Runtime must act on it")
	}
	following := NewRuntime(nil)
	installed := providers.InstallForTests(t)
	if following.providers() != installed {
		t.Fatal("a router Runtime built without one must follow the installed provider Runtime")
	}
}

func TestRouterInstallForTestsRestoresTheRuntime(t *testing.T) {
	outer := Current()
	var inner *Runtime
	t.Run("inner", func(t *testing.T) {
		inner = InstallForTests(t)
		if Current() != inner || inner == outer {
			t.Fatal("InstallForTests did not install a new router Runtime")
		}
	})
	if Current() != outer {
		t.Fatal("the previous router Runtime was not restored")
	}
}
