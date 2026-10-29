//go:build linux || darwin

package compose_test

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"
)

func probeCall() port.Call {
	return port.Call{
		Scope:         scope(),
		DockerContext: "default",
		Topology:      plan.Topology{Workloads: []plan.Workload{{Name: "primary--store", Type: plan.TypeService, Image: "ghcr.io/example/store:1.0"}}},
	}
}

func oneShot() module.Probe {
	return module.Probe{ID: "ping", Kind: module.ProbeOneShot, Target: "primary--store", Argv: []string{"true"}}
}

func adapterFor(docker string) *compose.Adapter {
	return compose.New(compose.Options{Docker: docker, ChildEnv: func() []string { return nil }})
}

func TestProbe_AChildThatNeverStartedIsAnErrorNotASuccess(t *testing.T) {
	t.Parallel()
	result, err := adapterFor("/nonexistent/docker").Probe(t.Context(), probeCall(), oneShot())
	if !errors.Is(err, compose.ErrCommand) {
		t.Fatalf("a probe whose child cannot start must fail, got result %+v and error %v", result, err)
	}
}

func TestProbe_ANonZeroExitIsAResultTheDriverReads(t *testing.T) {
	t.Parallel()
	falsePath, err := exec.LookPath("false")
	if err != nil {
		t.Fatalf("the test needs a false binary on PATH: %v", err)
	}
	result, err := adapterFor(falsePath).Probe(t.Context(), probeCall(), oneShot())
	if err != nil {
		t.Fatalf("a non-zero exit is the driver's to read: %v", err)
	}
	if result.ID != "ping" || result.ExitCode == 0 {
		t.Errorf("result = %+v, want the non-zero exit code", result)
	}
}

func TestRepoDigest_TakesTheDigestOfTheDeclaredRepositoryOnly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		ref     string
		digests []string
		want    string
		ok      bool
	}{
		{"the first digest belongs to another repository", "mirror.gcr.io/library/busybox:1.37", []string{"other.example/busybox@sha256:aa", "mirror.gcr.io/library/busybox@sha256:bb"}, "mirror.gcr.io/library/busybox@sha256:bb", true},
		{"a digest reference", "mirror.gcr.io/library/busybox@sha256:bb", []string{"mirror.gcr.io/library/busybox@sha256:bb"}, "mirror.gcr.io/library/busybox@sha256:bb", true},
		{"a registry with a port and a tag", "localhost:5000/team/app:2", []string{"localhost:5000/team/app@sha256:cc"}, "localhost:5000/team/app@sha256:cc", true},
		{"only another repository's digest", "ghcr.io/example/api:1.0", []string{"other.example/api@sha256:dd"}, "", false},
		{"no digest", "ghcr.io/example/api:1.0", nil, "", false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			got, ok := compose.RepoDigest(test.ref, test.digests)
			if got != test.want || ok != test.ok {
				t.Errorf("RepoDigest = %q, %v; want %q, %v", got, ok, test.want, test.ok)
			}
		})
	}
}
