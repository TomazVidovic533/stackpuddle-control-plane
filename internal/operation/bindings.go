package operation

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// loopback is the address a host-context consumer reaches a published Export at.
const loopback = "127.0.0.1"

// addressLookup supplies the address each binding resolves to: the service name and container port
// on the runtime network, and the loopback address with the declared host port on the host. A host
// port the runtime assigns is not known before the run; only host-context consumers (clients) read the
// host address, and the engine delivers nothing into a client.
func addressLookup(p model.Project, env model.Environment) model.AddressLookup {
	return func(source model.ExportRef, context model.Context) (model.Endpoint, bool) {
		name, port, ok := plan.ExportAddress(p, source)
		if !ok {
			return model.Endpoint{}, false
		}
		if context != model.ContextHost {
			return model.Endpoint{Host: name, Port: port}, true
		}
		for _, exposure := range env.Exposures() {
			if exposure.Source == source {
				if host, assigned := exposure.HostPort(); assigned {
					return model.Endpoint{Host: loopback, Port: host}, true
				}
				return model.Endpoint{Host: loopback, Port: port}, true
			}
		}
		return model.Endpoint{}, false
	}
}

// deliveries resolves every binding of the project that declares a delivery, asks the driver of a
// module source for its values, and returns what each consumer and each server workload receives. It
// runs no probe and has no effect, so a dry run and the real run see the same result, and a refusal
// comes before any effect.
func (e *Engine) deliveries(ctx context.Context, p model.Project, envName model.EnvironmentName) (plan.Deliveries, error) {
	env, ok := p.Environment(envName)
	if !ok {
		return nil, fail(state.CodeUnknownObject, state.CategoryNotFound, "environment %s is not declared", envName)
	}
	resolved, diagnostics := p.ResolveBindings(envName, addressLookup(p, env))
	if len(diagnostics) > 0 {
		codes := make([]string, 0, len(diagnostics))
		for _, d := range diagnostics {
			codes = append(codes, string(d.Code()))
		}
		return nil, fail(state.CodeProjectInvalid, state.CategoryValidation, "the bindings do not resolve: %s", strings.Join(codes, ", "))
	}
	b := binder{e: e, project: p, env: envName, resolved: resolved}
	var bound []plan.BoundBinding
	for _, binding := range p.Bindings() {
		delivery, declared := binding.Delivery()
		if !declared {
			continue
		}
		one, err := b.bind(ctx, binding, delivery)
		if err != nil {
			return nil, err
		}
		if one != nil {
			bound = append(bound, *one)
		}
	}
	out, err := plan.BuildDeliveries(bound)
	if err != nil {
		return nil, classifyPlanError(err)
	}
	e.addServerFiles(p, out)
	return out, nil
}

// binder gathers what the bindings of one project and environment deliver.
type binder struct {
	e        *Engine
	project  model.Project
	env      model.EnvironmentName
	resolved model.Bindings
}

// bind gathers what one binding's delivery needs. A binding whose consumer runs on the host delivers
// nothing and gives nil.
func (b binder) bind(ctx context.Context, binding model.Binding, delivery model.Delivery) (*plan.BoundBinding, error) {
	consumer := binding.Ref().Consumer
	switch consumer.Kind() {
	case model.ObjectClient:
		return nil, nil //nolint:nilnil // reason: a client runs on the host and is delivered nothing; nil with no error says so
	case model.ObjectModule:
		return nil, fail(state.CodeValueUnavailable, state.CategoryValidation, "%s: the engine delivers into workloads, not into a module instance", binding.Ref())
	case model.ObjectWorkload:
	}
	index := slices.IndexFunc(b.resolved.Entries(), func(r model.ResolvedBinding) bool { return r.Ref == binding.Ref() })
	if index < 0 {
		return nil, fail(state.CodeProjectInvalid, state.CategoryValidation, "%s did not resolve", binding.Ref())
	}
	values, ok := exportValues(b.project, binding.Source())
	if !ok {
		return nil, fail(state.CodeUnknownObject, state.CategoryValidation, "%s names no export", binding.Ref())
	}
	bound := plan.BoundBinding{
		Consumer: consumer.Key().String(), Ref: binding.Ref(), Delivery: delivery,
		Endpoint: b.resolved.Entries()[index].Endpoint, Values: values,
	}
	if binding.Source().Object.Kind() != model.ObjectModule {
		return &bound, nil
	}
	driver, instance, hasDriver := b.e.driverOf(b.project, b.env, binding.Source().Object)
	if !hasDriver {
		return &bound, nil
	}
	parameters, _ := binding.Parameters()
	response, err := driver.Bind(ctx, module.BindRequest{Instance: instance, Export: binding.Source().Export.String(), Parameters: parameters})
	if err != nil {
		return nil, fail(state.CodeParametersInvalid, state.CategoryValidation, "%s", binding.Ref()).wrap(err)
	}
	bound.Bound, err = checkBound(instance.Key, values, response)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", binding.Ref(), err)
	}
	return &bound, nil
}

// driverOf returns the driver of a module instance and the instance as the port sees it. The scope is
// empty: Bind depends on the options only.
func (e *Engine) driverOf(p model.Project, envName model.EnvironmentName, ref model.ObjectRef) (module.Driver, module.Instance, bool) {
	for _, mi := range p.ModuleInstances() {
		if mi.Key() != ref.Key() {
			continue
		}
		driver, ok := e.drivers[mi.Definition().String()]
		def, found := p.Definition(mi.Definition())
		if !ok || !found {
			return nil, module.Instance{}, false
		}
		options, _ := p.EnvironmentOptions(envName, mi.Key())
		return driver, module.Instance{Key: mi.Key().String(), Definition: def.Name().String(), Version: def.Version().String(), Options: options.JSON()}, true
	}
	return nil, module.Instance{}, false
}

// exportValues are the declared values of the Export a binding consumes.
func exportValues(p model.Project, source model.ExportRef) ([]model.ExportValue, bool) {
	switch source.Object.Kind() {
	case model.ObjectWorkload:
		return workloadExportValues(p, source)
	case model.ObjectModule:
		return moduleExportValues(p, source)
	case model.ObjectClient:
	}
	return nil, false
}

func workloadExportValues(p model.Project, source model.ExportRef) ([]model.ExportValue, bool) {
	for _, w := range p.Workloads() {
		if w.Key() != source.Object.Key() {
			continue
		}
		for _, export := range w.Exports() {
			if export.Key == source.Export {
				return export.Values(), true
			}
		}
	}
	return nil, false
}

func moduleExportValues(p model.Project, source model.ExportRef) ([]model.ExportValue, bool) {
	for _, mi := range p.ModuleInstances() {
		def, found := p.Definition(mi.Definition())
		if mi.Key() != source.Object.Key() || !found {
			continue
		}
		for _, export := range def.Exports() {
			if export.Export.Key == source.Export {
				return export.Export.Values(), true
			}
		}
	}
	return nil, false
}

// checkBound refuses a driver response the engine cannot deliver: a value the Export does not declare,
// a declared value that is missing, a value that is not exactly one of text, secret reference and
// material, an unknown material, a secret reference outside the instance's namespace, and a value
// whose kind does not match its sensitivity. It runs before any effect.
func checkBound(instance string, declared []model.ExportValue, response module.BindResponse) (map[string]module.BoundValue, error) {
	out := map[string]module.BoundValue{}
	for _, v := range response.Values {
		index := slices.IndexFunc(declared, func(d model.ExportValue) bool { return d.Name.String() == v.Name })
		if index < 0 {
			return nil, fail(state.CodeValueUnavailable, state.CategoryValidation, "the driver bound %q, which the export does not declare", v.Name)
		}
		if _, dup := out[v.Name]; dup {
			return nil, fail(state.CodeValueUnavailable, state.CategoryValidation, "the driver bound %q twice", v.Name)
		}
		if err := checkValue(instance, v, declared[index].Sensitivity); err != nil {
			return nil, err
		}
		out[v.Name] = v
	}
	for _, d := range declared {
		if _, ok := out[d.Name.String()]; !ok {
			return nil, fail(state.CodeValueUnavailable, state.CategoryValidation, "the driver did not bind the declared value %q", d.Name)
		}
	}
	return out, nil
}

func checkValue(instance string, v module.BoundValue, sensitivity model.Sensitivity) error {
	set := 0
	for _, present := range []bool{v.Text != "", v.SecretRef != "", v.Material != ""} {
		if present {
			set++
		}
	}
	switch {
	case set != 1:
		return fail(state.CodeValueUnavailable, state.CategoryValidation, "the driver bound %q as %d of text, secret reference and material; exactly one is required", v.Name, set)
	case v.Material != "" && v.Material != module.MaterialTLSCA:
		return fail(state.CodeValueUnavailable, state.CategoryValidation, "the driver bound %q to the unknown material %q", v.Name, v.Material)
	case v.SecretRef != "" && (!strings.HasPrefix(v.SecretRef, instance+"/") || strings.HasPrefix(v.SecretRef, inputNamespace)):
		return fail(state.CodeOutOfScope, state.CategoryForbidden, "the driver bound %q to a secret outside the namespace of instance %s", v.Name, instance)
	case (v.SecretRef != "") != (sensitivity == model.SensitivitySecret):
		return fail(state.CodeValueUnavailable, state.CategoryValidation, "the driver bound %q with a kind that does not match its sensitivity", v.Name)
	}
	return nil
}

// addServerFiles adds the certificate and key files of every module workload that serves encrypted
// connections, from the driver's descriptor.
func (e *Engine) addServerFiles(p model.Project, out plan.Deliveries) {
	for _, mi := range p.ModuleInstances() {
		driver, ok := e.drivers[mi.Definition().String()]
		if !ok {
			continue
		}
		for _, tlsWorkload := range driver.Describe().ServerTLS {
			name := mi.Key().String() + "--" + tlsWorkload.Workload
			d := out[name]
			d.Files = append(d.Files, plan.ServerFiles(tlsWorkload.Owner, name)...)
			out[name] = d
		}
	}
}

// runSelection names the workloads a run needs besides the job itself: the workloads its bindings
// consume, and every workload of a module instance it binds. Their own prerequisites join through the
// merge.
func runSelection(p model.Project, top plan.Topology, job string) []string {
	names := []string{job}
	for _, b := range p.Bindings() {
		consumer := b.Ref().Consumer
		if consumer.Kind() != model.ObjectWorkload || consumer.Key().String() != job {
			continue
		}
		source := b.Source().Object
		switch source.Kind() {
		case model.ObjectWorkload:
			names = append(names, source.Key().String())
		case model.ObjectModule:
			for _, w := range top.Workloads {
				if w.Instance == source.Key().String() {
					names = append(names, w.Name)
				}
			}
		case model.ObjectClient:
		}
	}
	slices.Sort(names)
	return slices.Compact(names)
}
