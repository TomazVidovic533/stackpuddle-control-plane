//go:build linux || darwin

package compose

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Sentinels for the failures a caller classifies.
var (
	// ErrNoDocker marks an environment whose PATH holds no docker binary.
	ErrNoDocker = errors.New("docker binary not found on PATH")
	// ErrUnreachable marks a declared context whose engine does not answer.
	ErrUnreachable = errors.New("docker context is unreachable")
	// ErrOutOfScope marks a probe target that is not a container of this scope.
	ErrOutOfScope = errors.New("target is not part of this scope")
	// ErrImageMissing marks a recorded image that is gone; it is never rebuilt implicitly.
	ErrImageMissing = errors.New("recorded image is missing")
)

// Docker command words used more than once.
const (
	argFormat    = "--format"
	argFilter    = "--filter"
	argLabel     = "--label"
	argInspect   = "inspect"
	argVolume    = "volume"
	argImage     = "image"
	argContainer = "container"
	argNoTrunc   = "--no-trunc"
	formatID     = "{{.ID}}"
)

// notRunning is the exit code of a probe whose target container is not running.
const notRunning = -1

// Options configure an Adapter.
type Options struct {
	// Docker is the absolute path of the docker binary; empty means none was found.
	Docker string
	// ChildEnv returns the allowlisted environment of every child.
	ChildEnv func() []string
}

// Adapter runs Docker and Compose for the declared context. It holds no state between calls.
type Adapter struct {
	docker   string
	childEnv func() []string
}

// New returns an Adapter.
func New(opts Options) *Adapter {
	return &Adapter{docker: opts.Docker, childEnv: opts.ChildEnv}
}

// Render translates the topology to Compose JSON; see the package function.
func (*Adapter) Render(scope plan.ScopeNames, top plan.Topology) (plan.Rendered, error) {
	return Render(scope, top)
}

// Target asks the declared context for the identity of its engine. Nothing is created.
func (a *Adapter) Target(ctx context.Context, call port.Call) (plan.TargetInfo, error) {
	if a.docker == "" {
		return plan.TargetInfo{}, ErrNoDocker
	}
	res, err := a.run(ctx, call, child{args: []string{"info", argFormat, "{{.ID}} {{.ServerVersion}}"}, timeout: timeoutInfo})
	if err != nil {
		return plan.TargetInfo{}, fmt.Errorf("%w: %w", ErrUnreachable, err)
	}
	fields := strings.Fields(string(res.stdout))
	if len(fields) == 0 {
		return plan.TargetInfo{}, fmt.Errorf("%w: docker info printed no engine ID", ErrUnreachable)
	}
	info := plan.TargetInfo{EngineID: fields[0]}
	if len(fields) > 1 {
		info.ServerVersion = fields[1]
	}
	return info, nil
}

type inspectedContainer struct {
	ID      string `json:"Id"`
	Name    string `json:"Name"`
	Created string `json:"Created"`
	State   struct {
		Running  bool `json:"Running"`
		ExitCode int  `json:"ExitCode"`
	} `json:"State"`
	Config struct {
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Ports map[string][]struct {
			HostIP   string `json:"HostIp"`
			HostPort string `json:"HostPort"`
		} `json:"Ports"`
	} `json:"NetworkSettings"`
}

type inspectedVolume struct {
	Name      string            `json:"Name"`
	CreatedAt string            `json:"CreatedAt"`
	Labels    map[string]string `json:"Labels"`
}

// Observe lists the containers and volumes that carry the scope's label.
func (a *Adapter) Observe(ctx context.Context, call port.Call) (plan.Inventory, error) {
	filter := "label=" + plan.LabelScope + "=" + call.Scope.ScopeUID
	var inv plan.Inventory
	ids, err := a.run(ctx, call, child{args: []string{argContainer, "ls", "-a", argNoTrunc, argFilter, filter, argFormat, formatID}, timeout: timeoutObserve})
	if err != nil {
		return inv, err
	}
	if list := strings.Fields(string(ids.stdout)); len(list) > 0 {
		out, err := a.run(ctx, call, child{args: append([]string{argContainer, argInspect, argFormat, "{{json .}}"}, list...), timeout: timeoutObserve})
		if err != nil {
			return inv, err
		}
		inv.Containers, err = parseContainers(out.stdout)
		if err != nil {
			return inv, err
		}
	}
	names, err := a.run(ctx, call, child{args: []string{argVolume, "ls", "-q", argFilter, filter}, timeout: timeoutObserve})
	if err != nil {
		return inv, err
	}
	if list := strings.Fields(string(names.stdout)); len(list) > 0 {
		out, err := a.run(ctx, call, child{args: append([]string{argVolume, argInspect, argFormat, "{{json .}}"}, list...), timeout: timeoutObserve})
		if err != nil {
			return inv, err
		}
		inv.Volumes, err = parseVolumes(out.stdout, call.Scope.ComposeProject)
		if err != nil {
			return inv, err
		}
	}
	return inv, nil
}

func parseContainers(out []byte) ([]plan.Container, error) {
	var containers []plan.Container
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var c inspectedContainer
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, fmt.Errorf("compose: read container inspect: %w", err)
		}
		containers = append(containers, plan.Container{
			Object:   plan.Object{Kind: "container", Name: strings.TrimPrefix(c.Name, "/"), ID: c.ID, Realization: c.Config.Labels[plan.LabelRealization], CreatedAt: c.Created},
			Service:  c.Config.Labels["com.docker.compose.service"],
			Running:  c.State.Running,
			ExitCode: c.State.ExitCode,
			SpecHash: c.Config.Labels[plan.LabelSpec],
			Ports:    c.publishedPorts(),
		})
	}
	return containers, nil
}

// publishedPorts lists the host bindings of the container, sorted by container port.
func (c inspectedContainer) publishedPorts() []state.PortMapping {
	var ports []state.PortMapping
	for spec, bindings := range c.NetworkSettings.Ports {
		containerPort, _, _ := strings.Cut(spec, "/")
		target, err := strconv.Atoi(containerPort)
		if err != nil {
			continue
		}
		for _, b := range bindings {
			if host, err := strconv.Atoi(b.HostPort); err == nil {
				ports = append(ports, state.PortMapping{Container: target, Host: host, HostIP: b.HostIP})
			}
		}
	}
	sort.Slice(ports, func(i, j int) bool { return ports[i].Container < ports[j].Container })
	return ports
}

func parseVolumes(out []byte, composeProject string) ([]plan.Object, error) {
	var volumes []plan.Object
	prefix := composeProject + "_"
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line == "" {
			continue
		}
		var v inspectedVolume
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("compose: read volume inspect: %w", err)
		}
		volumes = append(volumes, plan.Object{Kind: argVolume, Name: strings.TrimPrefix(v.Name, prefix), ID: v.Name, Realization: v.Labels[plan.LabelRealization], CreatedAt: v.CreatedAt})
	}
	return volumes, nil
}

// Apply performs one effect. The caller observes afterwards; Apply reports only what the effect
// itself proved.
func (a *Adapter) Apply(ctx context.Context, call port.Call, e plan.Effect) (plan.Evidence, error) {
	switch effect := e.(type) {
	case plan.CreateVolume:
		return a.createVolume(ctx, call, effect)
	case plan.DeleteVolume:
		return a.simple(ctx, call, child{args: []string{argVolume, "rm", VolumeName(call.Scope.ComposeProject, effect.Volume)}, timeout: timeoutEffect}, "removed volume "+effect.Volume)
	case plan.ResolveImage:
		return a.resolveImage(ctx, call, effect)
	case plan.BuildImage:
		return a.buildImage(ctx, call, effect)
	case plan.StartService:
		return a.compose(ctx, call, composeCall{timeoutEffect, "started " + effect.Workload}, "up", "-d", "--no-deps", "--no-build", "--pull", "never", effect.Workload)
	case plan.RunJob:
		return a.runJob(ctx, call, effect)
	case plan.StopService:
		return a.compose(ctx, call, composeCall{timeoutEffect, "stopped " + effect.Workload}, "stop", effect.Workload)
	case plan.RemoveService:
		return a.compose(ctx, call, composeCall{timeoutEffect, "removed " + effect.Workload}, "rm", "-f", "-s", effect.Workload)
	}
	return plan.Evidence{}, fmt.Errorf("compose: unsupported effect %T", e)
}

func (a *Adapter) simple(ctx context.Context, call port.Call, c child, evidence string) (plan.Evidence, error) {
	if _, err := a.run(ctx, call, c); err != nil {
		return plan.Evidence{}, err
	}
	return plan.Evidence{Text: evidence}, nil
}

func (a *Adapter) createVolume(ctx context.Context, call port.Call, e plan.CreateVolume) (plan.Evidence, error) {
	scope := call.Scope
	args := []string{
		argVolume, "create",
		argLabel, "com.docker.compose.project=" + scope.ComposeProject,
		argLabel, "com.docker.compose.volume=" + e.Volume,
		argLabel, plan.LabelScope + "=" + scope.ScopeUID,
		argLabel, plan.LabelRealization + "=" + scope.Realizations[plan.RealizationKey(argVolume, e.Volume)],
		VolumeName(scope.ComposeProject, e.Volume),
	}
	return a.simple(ctx, call, child{args: args, timeout: timeoutEffect}, "created volume "+e.Volume)
}

// compose runs one docker compose subcommand over the rendered project, passing the file on stdin
// and the secret values in the child's environment only.
// composeCall is the budget and the journal line of one docker compose call.
type composeCall struct {
	timeout  time.Duration
	evidence string
}

func (a *Adapter) compose(ctx context.Context, call port.Call, c composeCall, sub ...string) (plan.Evidence, error) {
	rendered, err := Render(call.Scope, call.Topology)
	if err != nil {
		return plan.Evidence{}, err
	}
	args := append([]string{"compose", "-p", call.Scope.ComposeProject, "--env-file", "/dev/null", "-f", "-"}, sub...)
	env := a.secretEnv(call, rendered)
	if _, err := a.run(ctx, call, child{args: args, stdin: rendered.Compose, env: env, timeout: c.timeout}); err != nil {
		return plan.Evidence{}, err
	}
	return plan.Evidence{Text: c.evidence}, nil
}

// secretEnv puts the value of every delivered file's source into the child's environment under the
// variable the rendered project names: a secret from the sealed store, a certificate, or a literal.
// Nothing else carries a value to Compose.
func (*Adapter) secretEnv(call port.Call, rendered plan.Rendered) []string {
	var env []string
	for key, name := range rendered.SecretVars {
		switch {
		case call.Secrets[key] != nil:
			env = append(env, name+"="+string(call.Secrets[key].Reveal()))
		case call.Files[key] != nil:
			env = append(env, name+"="+string(call.Files[key]))
		default:
			if text, ok := rendered.Literals[key]; ok {
				env = append(env, name+"="+text)
			}
		}
	}
	slices.Sort(env)
	return env
}

// runJob runs one job to the end of its container. The job's own exit code is a result, not a
// failure of the engine: whenever the job's container ran and exited, its code comes back in the
// evidence, zero or not, and only a job that could not be run is an error.
func (a *Adapter) runJob(ctx context.Context, call port.Call, e plan.RunJob) (plan.Evidence, error) {
	ev, err := a.compose(ctx, call, composeCall{timeoutEffect, "ran job " + e.Workload}, "up", "--no-deps", "--no-build", "--pull", "never", "--force-recreate", "--exit-code-from", e.Workload, e.Workload)
	if err == nil {
		ev.Job = &state.JobResult{}
		return ev, nil
	}
	if _, exited := errors.AsType[*exitError](err); !exited {
		return plan.Evidence{}, err
	}
	code, ran, lookupErr := a.jobExit(ctx, call, e.Workload)
	if lookupErr != nil || !ran || code == 0 {
		return plan.Evidence{}, err
	}
	return plan.Evidence{Text: fmt.Sprintf("job %s exited %d", e.Workload, code), Job: &state.JobResult{ExitCode: code}}, nil
}

// jobExit reads the exit code of the one exited container of a job service of this scope. ran is false
// when there is none, or it did not exit, so the failure was the engine's.
func (a *Adapter) jobExit(ctx context.Context, call port.Call, service string) (code int, ran bool, err error) {
	res, err := a.run(ctx, call, child{args: []string{
		argContainer, "ls", "-a", argNoTrunc, argFilter, "label=" + plan.LabelScope + "=" + call.Scope.ScopeUID,
		argFilter, "label=com.docker.compose.service=" + service, argFormat, formatID,
	}, timeout: timeoutObserve})
	if err != nil {
		return 0, false, err
	}
	ids := strings.Fields(string(res.stdout))
	if len(ids) != 1 {
		return 0, false, nil
	}
	out, err := a.run(ctx, call, child{args: []string{argContainer, argInspect, argFormat, "{{.State.Status}} {{.State.ExitCode}}", ids[0]}, timeout: timeoutObserve})
	if err != nil {
		return 0, false, err
	}
	status, text, _ := strings.Cut(strings.TrimSpace(string(out.stdout)), " ")
	if status != "exited" {
		return 0, false, nil
	}
	exit, convErr := strconv.Atoi(text)
	if convErr != nil {
		return 0, false, fmt.Errorf("compose: read the exit code of %s: %w", service, convErr)
	}
	return exit, true, nil
}

func (a *Adapter) resolveImage(ctx context.Context, call port.Call, e plan.ResolveImage) (plan.Evidence, error) {
	w, ok := call.Topology.Find(e.Workload)
	if !ok || w.Image == "" {
		return plan.Evidence{}, fmt.Errorf("compose: workload %q declares no image", e.Workload)
	}
	ref := w.Image
	if _, err := a.run(ctx, call, child{args: []string{argImage, argInspect, argFormat, "{{.Id}}", ref}, timeout: timeoutObserve}); err != nil || !strings.Contains(ref, "@sha256:") {
		if _, pullErr := a.run(ctx, call, child{args: []string{"pull", "--quiet", ref}, timeout: timeoutImage}); pullErr != nil {
			return plan.Evidence{}, pullErr
		}
	}
	out, err := a.run(ctx, call, child{args: []string{argImage, argInspect, argFormat, "{{json .RepoDigests}}|{{.Id}}", ref}, timeout: timeoutObserve})
	if err != nil {
		return plan.Evidence{}, err
	}
	digests, id, _ := strings.Cut(strings.TrimSpace(string(out.stdout)), "|")
	var repoDigests []string
	if err := json.Unmarshal([]byte(digests), &repoDigests); err != nil {
		return plan.Evidence{}, fmt.Errorf("compose: read repo digests: %w", err)
	}
	artifact := state.Artifact{ImageID: id}
	if digest, ok := RepoDigest(ref, repoDigests); ok {
		artifact = state.Artifact{Image: digest}
	}
	return plan.Evidence{Text: "resolved image of " + e.Workload, Artifact: &artifact}, nil
}

// RepoDigest returns the repo digest of the declared reference's own repository; an image can carry
// digests of other repositories, which do not identify what was declared. ok is false when none matches.
func RepoDigest(ref string, repoDigests []string) (string, bool) {
	repository := repositoryOf(ref)
	for _, digest := range repoDigests {
		if name, _, found := strings.Cut(digest, "@"); found && name == repository {
			return digest, true
		}
	}
	return "", false
}

// repositoryOf strips the tag and the digest from an image reference. A colon after the last slash is
// a tag; one before it is a registry port.
func repositoryOf(ref string) string {
	name, _, _ := strings.Cut(ref, "@")
	slash := strings.LastIndex(name, "/")
	if colon := strings.LastIndex(name, ":"); colon > slash {
		name = name[:colon]
	}
	return name
}

func (a *Adapter) buildImage(ctx context.Context, call port.Call, e plan.BuildImage) (plan.Evidence, error) {
	if _, err := a.compose(ctx, call, composeCall{timeoutImage, "built"}, "build", e.Workload); err != nil {
		return plan.Evidence{}, err
	}
	tag := BuiltImageTag(call.Scope.ComposeProject, e.Workload)
	out, err := a.run(ctx, call, child{args: []string{argImage, argInspect, argFormat, "{{.Id}}", tag}, timeout: timeoutObserve})
	if err != nil {
		return plan.Evidence{}, fmt.Errorf("%w: %s: %w", ErrImageMissing, tag, err)
	}
	artifact := state.Artifact{ImageID: strings.TrimSpace(string(out.stdout))}
	return plan.Evidence{Text: "built image of " + e.Workload, Artifact: &artifact}, nil
}

// Probe runs one exec or one-shot probe of a module driver. The target must be a service of this
// scope's topology and, for exec, a container that carries this scope's label.
func (a *Adapter) Probe(ctx context.Context, call port.Call, p module.Probe) (module.ProbeResult, error) {
	w, ok := call.Topology.Find(p.Target)
	if !ok {
		return module.ProbeResult{}, fmt.Errorf("%w: %q", ErrOutOfScope, p.Target)
	}
	env, passthrough, err := probeSecrets(call, p)
	if err != nil {
		return module.ProbeResult{}, err
	}
	args, absent, err := a.probeArgs(ctx, probeRun{call: call, probe: p, passthrough: passthrough, image: imageOf(call, w)})
	if err != nil {
		return module.ProbeResult{}, err
	}
	if absent {
		return module.ProbeResult{ID: p.ID, ExitCode: notRunning, Stderr: "target " + p.Target + " is not running"}, nil
	}
	c := child{args: args, env: env, timeout: timeoutProbe}
	if p.Stdin != "" {
		c.stdin = []byte(p.Stdin)
	}
	res, err := a.run(ctx, call, c)
	out := module.ProbeResult{ID: p.ID, ExitCode: res.exitCode, Stdout: string(res.stdout), Stderr: string(res.stderr)}
	if _, exited := errors.AsType[*exitError](err); err != nil && !exited {
		return out, err
	}
	return out, nil
}

// probeArgs builds the docker arguments of a probe. absent is true when an exec probe's target has no
// running container, which is a result for the driver to read, not an error.
func (a *Adapter) probeArgs(ctx context.Context, r probeRun) (args []string, absent bool, err error) {
	switch r.probe.Kind {
	case module.ProbeExec:
		id, err := a.scopeContainer(ctx, r.call, r.probe.Target)
		if err != nil || id == "" {
			return nil, id == "", err
		}
		return execArgs(r.probe, r.passthrough, id), false, nil
	case module.ProbeOneShot:
		return oneShotArgs(r.call, r.probe, r.passthrough, r.image), false, nil
	}
	return nil, false, fmt.Errorf("compose: unknown probe kind %q", r.probe.Kind)
}

// probeRun is one probe with what its docker arguments depend on.
type probeRun struct {
	call        port.Call
	probe       module.Probe
	passthrough []string
	image       string
}

// probeSecrets builds the child environment entries for a probe's secrets and the "-e NAME" flags that
// forward them by name, so no value appears on a command line.
func probeSecrets(call port.Call, p module.Probe) (env, passthrough []string, err error) {
	for _, binding := range p.SecretEnv {
		v, ok := call.Secrets[port.SecretKey(binding.Ref, binding.Form)]
		if !ok {
			return nil, nil, fmt.Errorf("compose: probe %s needs secret %s that was not resolved", p.ID, binding.Ref)
		}
		env = append(env, binding.Name+"="+string(v.Reveal()))
		passthrough = append(passthrough, "-e", binding.Name)
	}
	return env, passthrough, nil
}

func execArgs(p module.Probe, passthrough []string, container string) []string {
	args := []string{"exec"}
	if p.Stdin != "" {
		args = append(args, "-i")
	}
	if p.User != "" {
		args = append(args, "-u", p.User)
	}
	args = append(args, passthrough...)
	args = append(args, container)
	return append(args, p.Argv...)
}

func oneShotArgs(call port.Call, p module.Probe, passthrough []string, image string) []string {
	args := []string{"run", "--rm", "--network", call.Scope.ComposeProject + "_default", argLabel, plan.LabelScope + "=" + call.Scope.ScopeUID}
	if p.User != "" {
		args = append(args, "-u", p.User)
	}
	args = append(args, passthrough...)
	args = append(args, image)
	return append(args, p.Argv...)
}

func imageOf(call port.Call, w plan.Workload) string {
	if artifact, ok := call.Scope.Artifacts[w.Name]; ok {
		if artifact.ImageID != "" {
			return artifact.ImageID
		}
		if artifact.Image != "" {
			return artifact.Image
		}
	}
	return w.Image
}

// scopeContainer finds the one running container of a service that carries this scope's label; it
// returns "" when none runs and refuses more than one.
func (a *Adapter) scopeContainer(ctx context.Context, call port.Call, service string) (string, error) {
	res, err := a.run(ctx, call, child{args: []string{
		argContainer, "ls", argNoTrunc, argFilter, "label=" + plan.LabelScope + "=" + call.Scope.ScopeUID,
		argFilter, "label=com.docker.compose.service=" + service, argFormat, formatID,
	}, timeout: timeoutObserve})
	if err != nil {
		return "", err
	}
	ids := strings.Fields(string(res.stdout))
	switch len(ids) {
	case 0:
		return "", nil
	case 1:
		return ids[0], nil
	}
	return "", fmt.Errorf("%w: %d containers of %q carry this scope's label", ErrOutOfScope, len(ids), service)
}
