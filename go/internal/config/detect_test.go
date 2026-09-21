package config

import (
	"slices"
	"testing"
)

func TestLocalHostsReflectGatewayProcessBoundary(t *testing.T) {
	original := processRunsInContainer
	t.Cleanup(func() { processRunsInContainer = original })

	processRunsInContainer = func() bool { return false }
	if hosts := localHosts(); !slices.Equal(hosts, []string{"127.0.0.1", "localhost"}) {
		t.Fatalf("native process hosts=%v", hosts)
	}

	processRunsInContainer = func() bool { return true }
	if hosts := localHosts(); !slices.Equal(hosts, []string{"127.0.0.1", "localhost", "host.docker.internal"}) {
		t.Fatalf("container process hosts=%v", hosts)
	}
}
