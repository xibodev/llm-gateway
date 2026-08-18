package copilotauth

import (
	"path/filepath"
	"testing"
)

func TestBYOCSessionCachePathsAreIsolated(t *testing.T) {
	t.Setenv("LLMGW_GITHUB_COPILOT_CACHE_DIR", t.TempDir())
	first := sessionCachePathForOAuth("oauth-token-one")
	second := sessionCachePathForOAuth("oauth-token-two")
	if first == second {
		t.Fatal("different OAuth credentials share a session cache path")
	}
	if filepath.Base(first) == sessionCacheFile || filepath.Base(second) == sessionCacheFile {
		t.Fatal("BYOC cache reused the global session filename")
	}
}
