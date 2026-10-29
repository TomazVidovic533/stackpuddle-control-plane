//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"syscall"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/operation"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func TestMain(m *testing.M) {
	testkit.Dispatch(map[string]testkit.Helper{"submit": helperSubmit})
	os.Exit(m.Run())
}

// scenario is what the helper process runs: one Submit through the real adapter, optionally killed.
type scenario struct {
	Env        map[string]string
	Options    testkit.ShopOptions
	ProjectDir string
	Command    state.Command
	// Refs selects objects such as "workload/api"; empty means everything.
	Refs []string
	// Kill names the effect that ends the process with SIGKILL: its Go type name ("plan.CreateVolume"),
	// its target, and whether it happens "before" or "after" the effect.
	Kill struct{ Effect, Target, Phase string }
}

// killer ends the process with SIGKILL around one effect, as a crash would.
type killer struct {
	operation.Runtime
	kill struct{ Effect, Target, Phase string }
}

func (k killer) Apply(ctx context.Context, call port.Call, e plan.Effect) (plan.Evidence, error) {
	hit := k.kill.Effect != "" && fmt.Sprintf("%T", e) == k.kill.Effect && e.Target() == k.kill.Target
	if hit && k.kill.Phase == "before" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	evidence, err := k.Runtime.Apply(ctx, call, e)
	if hit && k.kill.Phase == "after" {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return evidence, err
}

func (k killer) Probe(ctx context.Context, call port.Call, p module.Probe) (module.ProbeResult, error) {
	return k.Runtime.Probe(ctx, call, p)
}

func helperSubmit(args []string) int {
	var sc scenario
	if err := json.Unmarshal([]byte(args[0]), &sc); err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	core, err := testkit.NewCore(testkit.CoreOptions{
		Env:         sc.Env,
		WrapRuntime: func(inner operation.Runtime) operation.Runtime { return killer{Runtime: inner, kill: sc.Kill} },
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	project, err := testkit.LoadProjectErr(testkit.ShopFiles(sc.Options))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	var refs []model.ObjectRef
	for _, text := range sc.Refs {
		ref, err := model.ParseObjectRef(text)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			return 2
		}
		refs = append(refs, ref)
	}
	name, _ := model.ParseProjectName(sc.Options.Name)
	environment, _ := model.ParseEnvironmentName("local")
	_, err = core.Engine.Submit(context.Background(), operation.Request{
		Scope: operation.Scope{Project: name, Environment: environment}, Project: project, ProjectDir: sc.ProjectDir,
		Command: sc.Command, All: len(refs) == 0, Refs: refs,
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 1
	}
	return 0
}

// helperCommand returns the command that runs the scenario in a separate process.
func (s *stack) helperCommand(o testkit.ShopOptions, kill struct{ Effect, Target, Phase string }, refs ...string) *scenarioRun {
	sc := scenario{Env: s.env, Options: s.opts(o), ProjectDir: s.projectDir(o), Command: state.CommandUp, Refs: refs}
	sc.Kill = kill
	data, err := json.Marshal(sc)
	if err != nil {
		s.t.Fatal(err)
	}
	return &scenarioRun{cmd: testkit.Command("submit", string(data))}
}
