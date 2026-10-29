package plan

import (
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle/model"
)

// Sentinels for the failures a caller classifies.
var (
	// ErrSecretInEnvironment marks a workload environment variable bound to a secret input.
	ErrSecretInEnvironment = errors.New("a secret input cannot be delivered through the environment")
	// ErrInputMissing marks an input with no value for the environment.
	ErrInputMissing = errors.New("input has no value")
	// ErrExpandedScopeRequired marks a change to a shared resource that has unselected consumers.
	ErrExpandedScopeRequired = errors.New("the change reaches unselected consumers")
	// ErrUnknownObject marks a reference that names nothing in the project.
	ErrUnknownObject = errors.New("reference names no object")
	// ErrInvalidName marks an instance-scoped name that is too long to be a DNS label.
	ErrInvalidName = errors.New("scoped name is too long")
)

const (
	// scopeSeparator joins an instance key and a workload or volume key; keys never contain it.
	scopeSeparator = "--"
	maxNameLength  = 62
	maxVolumeName  = 120
)

// Inputs are the input values the caller resolved for one environment.
type Inputs struct {
	// Public maps a public input key to its text.
	Public map[string]string
	// Secret lists the secret input keys that are supplied.
	Secret map[string]bool
}

// BuildTopology turns the project into the runtime-neutral desired state of one environment.
// Project workloads keep their key; each module instance's contributed workloads become
// <instance>--<workload>, and volumes <workload>--<volume>, so two instances of one definition
// never collide. A secret input bound to a workload environment variable is refused. The deliveries
// of the bindings are merged into their consumers; a key or path set twice is refused, and no
// delivery reads the process environment.
func BuildTopology(p model.Project, envName model.EnvironmentName, inputs Inputs, deliveries Deliveries) (Topology, error) {
	env, ok := p.Environment(envName)
	if !ok {
		return Topology{}, fmt.Errorf("%w: environment %s", ErrUnknownObject, envName)
	}
	resolver := inputResolver{project: p, env: env, inputs: inputs}
	var top Topology
	overrides := map[string][]model.EnvVar{}
	for _, o := range env.WorkloadOverrides() {
		overrides[o.Key.String()] = o.Environment
	}
	for _, w := range p.Workloads() {
		built, err := resolver.workload(w, workloadSpec{name: w.Key().String(), override: overrides[w.Key().String()]})
		if err != nil {
			return Topology{}, err
		}
		top.Workloads = append(top.Workloads, built)
	}
	for _, instance := range p.ModuleInstances() {
		if err := addInstance(&top, resolver, instance, envName); err != nil {
			return Topology{}, err
		}
	}
	if err := applyExposures(p, env, &top); err != nil {
		return Topology{}, err
	}
	markProviders(p, &top)
	if err := applyDeliveries(&top, deliveries); err != nil {
		return Topology{}, err
	}
	slices.SortFunc(top.Workloads, func(a, b Workload) int { return strings.Compare(a.Name, b.Name) })
	return top, nil
}

// markProviders flags the workloads other objects bind to and every workload a module contributes, so
// they come up before the jobs and services that consume them.
func markProviders(p model.Project, top *Topology) {
	sources := map[string]bool{}
	for _, b := range p.Bindings() {
		if source := b.Source().Object; source.Kind() == model.ObjectWorkload {
			sources[source.Key().String()] = true
		}
	}
	for i := range top.Workloads {
		w := &top.Workloads[i]
		w.Provider = w.Instance != "" || sources[w.Name]
	}
}

// applyDeliveries merges each workload's delivery into it, in name order so the first conflict reported
// does not depend on map order.
func applyDeliveries(top *Topology, deliveries Deliveries) error {
	names := make([]string, 0, len(deliveries))
	for name := range deliveries {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		index := slices.IndexFunc(top.Workloads, func(w Workload) bool { return w.Name == name })
		if index < 0 {
			return fmt.Errorf("%w: delivery into %s", ErrUnknownObject, name)
		}
		if err := deliveries[name].apply(&top.Workloads[index]); err != nil {
			return err
		}
	}
	return nil
}

// addInstance adds a module instance and the workloads its definition contributes, named
// <instance>--<workload>.
func addInstance(top *Topology, r inputResolver, instance model.ModuleInstance, envName model.EnvironmentName) error {
	def, ok := r.project.Definition(instance.Definition())
	if !ok {
		return fmt.Errorf("%w: definition %s", ErrUnknownObject, instance.Definition())
	}
	key := instance.Key().String()
	options, _ := r.project.EnvironmentOptions(envName, instance.Key())
	top.Instances = append(top.Instances, Instance{Key: key, Definition: def.Name().String(), Version: def.Version().String(), Options: options.JSON()})
	for _, w := range def.Workloads() {
		built, err := r.workload(w, workloadSpec{name: key + scopeSeparator + w.Key().String(), instance: key, definition: def.Name().String()})
		if err != nil {
			return err
		}
		for i := range built.Prerequisites {
			built.Prerequisites[i] = key + scopeSeparator + built.Prerequisites[i]
		}
		top.Workloads = append(top.Workloads, built)
	}
	return nil
}

type inputResolver struct {
	project model.Project
	env     model.Environment
	inputs  Inputs
}

// workloadSpec is the identity a declared workload gets in the topology.
type workloadSpec struct {
	name       string
	instance   string
	definition string
	override   []model.EnvVar
}

func (r inputResolver) workload(w model.Workload, spec workloadSpec) (Workload, error) {
	name, instance, definition, override := spec.name, spec.instance, spec.definition, spec.override
	if len(name) > maxNameLength {
		return Workload{}, fmt.Errorf("%w: %q has %d characters, the limit is %d", ErrInvalidName, name, len(name), maxNameLength)
	}
	out := Workload{Name: name, Instance: instance, Definition: definition, Type: w.Type().String(), Command: w.Command()}
	switch artifact := w.Artifact().(type) {
	case model.ImageArtifact:
		out.Image = artifact.Reference()
	case model.BuildArtifact:
		build := Build{Context: artifact.Context()}
		build.Dockerfile, _ = artifact.Dockerfile()
		build.Target, _ = artifact.Target()
		out.Build = &build
	}
	env, err := r.environment(w.Environment(), override)
	if err != nil {
		return Workload{}, fmt.Errorf("workload %s: %w", name, err)
	}
	out.Env = env
	for _, v := range w.Volumes() {
		volume := name + scopeSeparator + v.Key.String()
		if len(volume) > maxVolumeName {
			return Workload{}, fmt.Errorf("%w: volume %q", ErrInvalidName, volume)
		}
		out.Volumes = append(out.Volumes, VolumeMount{Name: volume, Path: v.Path})
	}
	for _, key := range w.Prerequisites() {
		out.Prerequisites = append(out.Prerequisites, key.String())
	}
	return out, nil
}

func (r inputResolver) environment(declared, override []model.EnvVar) ([]EnvVar, error) {
	merged := map[string]model.EnvVar{}
	for _, v := range declared {
		merged[v.Key.String()] = v
	}
	for _, v := range override {
		merged[v.Key.String()] = v
	}
	names := make([]string, 0, len(merged))
	for name := range merged {
		names = append(names, name)
	}
	slices.Sort(names)
	var out []EnvVar
	for _, name := range names {
		value, err := r.value(merged[name].Value)
		if err != nil {
			return nil, fmt.Errorf("variable %s: %w", name, err)
		}
		out = append(out, EnvVar{Name: name, Value: value})
	}
	return out, nil
}

func (r inputResolver) value(v model.EnvValue) (string, error) {
	switch value := v.(type) {
	case model.LiteralValue:
		return value.Text(), nil
	case model.InputValue:
		return r.inputText(value.Input())
	}
	return "", fmt.Errorf("unsupported environment value %T", v)
}

// inputText resolves a public input: the caller's value, else the environment's, else the project's.
func (r inputResolver) inputText(key model.InputKey) (string, error) {
	if r.inputs.Secret[key.String()] || r.isSecret(key) {
		return "", fmt.Errorf("input %s: %w", key, ErrSecretInEnvironment)
	}
	if text, ok := r.inputs.Public[key.String()]; ok {
		return text, nil
	}
	for _, sources := range r.env.InputSources() {
		if text, ok := sources.Value(); ok && sources.Key == key {
			return text, nil
		}
	}
	for _, input := range r.project.Inputs() {
		if text, ok := input.Value(); ok && input.Key() == key {
			return text, nil
		}
	}
	return "", fmt.Errorf("input %s: %w", key, ErrInputMissing)
}

func (r inputResolver) isSecret(key model.InputKey) bool {
	for _, input := range r.project.Inputs() {
		if input.Key() == key {
			return input.Sensitivity() == model.SensitivitySecret
		}
	}
	return false
}

// applyExposures publishes the Exports an environment exposes: only declared ports, nothing else.
func applyExposures(p model.Project, env model.Environment, top *Topology) error {
	for _, exposure := range env.Exposures() {
		name, container, err := exposureTarget(p, exposure.Source)
		if err != nil {
			return err
		}
		index := slices.IndexFunc(top.Workloads, func(w Workload) bool { return w.Name == name })
		if index < 0 {
			return fmt.Errorf("%w: exposure of %s", ErrUnknownObject, exposure.Source.Object)
		}
		port := Port{Container: container}
		port.Host, _ = exposure.HostPort()
		top.Workloads[index].Ports = append(top.Workloads[index].Ports, port)
	}
	return nil
}

func exposureTarget(p model.Project, source model.ExportRef) (name string, container int, err error) {
	switch source.Object.Kind() {
	case model.ObjectWorkload:
		name, container, ok := workloadExposure(p, source)
		if ok {
			return name, container, nil
		}
	case model.ObjectModule:
		name, container, ok := moduleExposure(p, source)
		if ok {
			return name, container, nil
		}
	case model.ObjectClient:
	}
	return "", 0, fmt.Errorf("%w: export %s of %s", ErrUnknownObject, source.Export, source.Object)
}

func workloadExposure(p model.Project, source model.ExportRef) (name string, container int, ok bool) {
	for _, w := range p.Workloads() {
		if w.Key() != source.Object.Key() {
			continue
		}
		if port, found := listenerPort(w, source.Export); found {
			return w.Key().String(), port, true
		}
	}
	return "", 0, false
}

func moduleExposure(p model.Project, source model.ExportRef) (name string, container int, ok bool) {
	instance := source.Object.Key()
	for _, mi := range p.ModuleInstances() {
		if mi.Key() != instance {
			continue
		}
		def, found := p.Definition(mi.Definition())
		if !found {
			return "", 0, false
		}
		for _, w := range definitionExporters(def, source.Export) {
			if port, has := listenerPort(w, source.Export); has {
				return instance.String() + scopeSeparator + w.Key().String(), port, true
			}
		}
	}
	return "", 0, false
}

// definitionExporters returns the contributed workloads that offer the export.
func definitionExporters(def model.ModuleDefinition, export model.ExportKey) []model.Workload {
	var out []model.Workload
	for _, de := range def.Exports() {
		if de.Export.Key != export {
			continue
		}
		for _, w := range def.Workloads() {
			if w.Key() == de.Workload {
				out = append(out, w)
			}
		}
	}
	return out
}

func listenerPort(w model.Workload, export model.ExportKey) (int, bool) {
	for _, e := range w.Exports() {
		if e.Key != export {
			continue
		}
		for _, l := range w.Listeners() {
			if l.Key == e.Listener {
				return l.Port, true
			}
		}
	}
	return 0, false
}

// ExportAddress returns the instance-scoped service name and the container port an Export is reached
// at on the runtime network. ok is false for an Export no workload serves.
func ExportAddress(p model.Project, source model.ExportRef) (name string, port int, ok bool) {
	name, port, err := exposureTarget(p, source)
	return name, port, err == nil
}
