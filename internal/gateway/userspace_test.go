package gateway

import (
	"testing"

)

func TestPathSandboxRequiredOnlyForHostedDeploy(t *testing.T) {
	t.Setenv("FLUCTIO_DEPLOY", "")
	if pathSandboxRequired() {
		t.Fatal("self-hosted default must allow direct host file access")
	}

	t.Setenv("FLUCTIO_DEPLOY", "self-hosted")
	if pathSandboxRequired() {
		t.Fatal("explicit self-hosted deploy must allow direct host file access")
	}

	t.Setenv("FLUCTIO_DEPLOY", "hosted")
	if !pathSandboxRequired() {
		t.Fatal("hosted deploy must retain workspace path isolation")
	}
}

