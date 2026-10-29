//go:build integration

package integration_test

import (
	"os"
	"os/exec"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

// TestComposeAdapterPassesTheRuntimeConformanceScenarios runs, against Docker, the scenario set the
// registered FakeRuntime double also passes (tests/unit/operation/runtime_contract_test.go).
func TestComposeAdapterPassesTheRuntimeConformanceScenarios(t *testing.T) {
	docker, err := exec.LookPath("docker")
	if err != nil {
		t.Fatalf("docker is not on PATH: %v", err)
	}
	settings, err := config.Load(testkit.LookupEnv(map[string]string{
		"STACKPUDDLE_STATE_DIR": t.TempDir(), "HOME": os.Getenv("HOME"), "PATH": os.Getenv("PATH"),
	}), "linux")
	if err != nil {
		t.Fatal(err)
	}
	name := randomName(t)
	uid, err := state.NewScopeUID(testkit.Clock()(), testkit.NewRandom(name))
	if err != nil {
		t.Fatal(err)
	}
	cs := testkit.ConformanceScope{ScopeUID: uid.String(), ComposeProject: name + "-local-conf", DockerContext: "default"}
	t.Cleanup(func() {
		// Only what this run's scope label or unique project name finds.
		for _, id := range lines(mustOut(dockerErr("ps", "-a", "-q", "--filter", "label=io.stackpuddle.scope="+cs.ScopeUID))) {
			_, _ = dockerErr("rm", "-f", id)
		}
		_, _ = dockerErr("compose", "-p", cs.ComposeProject, "down", "-v", "--remove-orphans")
		for _, v := range lines(mustOut(dockerErr("volume", "ls", "-q", "--filter", "label=io.stackpuddle.scope="+cs.ScopeUID))) {
			_, _ = dockerErr("volume", "rm", "-f", v)
		}
	})
	testkit.RuntimeConformance(t, compose.New(compose.Options{Docker: docker, ChildEnv: settings.ChildEnv}), cs)
}
