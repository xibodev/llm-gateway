package main

import (
	"bytes"
	"strings"
	"testing"

	"llmgw/internal/buildinfo"
)

func TestPrintVersionIncludesBuildIdentity(t *testing.T) {
	old := buildinfo.Current()
	buildinfo.Version = "1.2.3"
	buildinfo.Commit = "0123456789abcdef"
	buildinfo.BuildTime = "2026-01-02T03:04:05Z"
	t.Cleanup(func() {
		buildinfo.Version = old.Version
		buildinfo.Commit = old.Commit
		buildinfo.BuildTime = old.BuildTime
	})
	var output bytes.Buffer
	printVersion(&output)
	for _, expected := range []string{"llm-gateway 1.2.3", "commit 0123456789abcdef", "build_time 2026-01-02T03:04:05Z"} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("version output %q lacks %q", output.String(), expected)
		}
	}
}
