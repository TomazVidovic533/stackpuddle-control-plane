package operation_test

import (
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// The fake runtime is a registered double of Docker; this runs the scenario set the real adapter also
// passes (tests/integration/contract_test.go), so the two cannot drift apart unnoticed.
func TestFakeRuntimePassesTheRuntimeConformanceScenarios(t *testing.T) {
	t.Parallel()
	rt := testkit.NewFakeRuntime()
	rt.RefuseCancelled = true
	rt.JobExit = map[string]int{testkit.ConformanceBadJob: testkit.ConformanceBadExit}
	testkit.RuntimeConformance(t, rt, testkit.ConformanceScope{
		ScopeUID: "sc-01J9Z8K3M4N5P6Q7R8S9T0V1W3", ComposeProject: "conformance-local-t0v1w3", DockerContext: "default",
	})
}
