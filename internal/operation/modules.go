package operation

import (
	"context"
	"regexp"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/config"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/port"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func objectPattern() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z]+(?:/[a-z0-9][a-z0-9-]*)+$`)
}

func actionPattern() *regexp.Regexp {
	return regexp.MustCompile(`^[a-z][a-z0-9-]*\.[a-z][a-z0-9-]*(?:\.[a-z][a-z0-9-]*)*$`)
}

func (s *session) instanceSelected(inst plan.Instance) bool {
	if s.prep.selection.All {
		return true
	}
	return slices.ContainsFunc(s.top.Workloads, func(w plan.Workload) bool {
		return w.Instance == inst.Key && slices.Contains(s.prep.selection.Names, w.Name)
	})
}

// addSecretAndModuleSteps assembles the steps of an up or run in the deployment order: secrets
// and certificates, the provider workloads, the module access steps, the prerequisite jobs, the final
// access check of every module instance a consumer binds, and last the consumers. Nothing after a
// failed step runs, so a failed migration blocks the rollout.
func (s *session) addSecretAndModuleSteps(ctx context.Context, pl plan.Plan, tlsSteps []plan.Step) (plan.Plan, error) {
	secretSteps := s.secretInputSteps()
	var moduleSteps []plan.Step
	for _, inst := range s.top.Instances {
		driver, ok := s.e.drivers[inst.Definition]
		if !ok || !s.instanceSelected(inst) {
			continue
		}
		steps, err := s.planInstance(ctx, driver, inst)
		if err != nil {
			return plan.Plan{}, err
		}
		secretSteps = s.addSecretNeeds(secretSteps, steps)
		moduleSteps = append(moduleSteps, steps...)
	}
	var byPhase [plan.PhaseConsumer + 1][]plan.Step
	for _, step := range pl.Steps {
		phase := min(max(step.Phase, plan.PhaseProvider), plan.PhaseConsumer)
		byPhase[phase] = append(byPhase[phase], step)
	}
	var verify []plan.Step
	if len(pl.Steps)+len(moduleSteps) > 0 {
		// An up that changes nothing stays empty; one that changes anything re-checks the access first.
		verify = s.verifySteps()
	}
	pl.Steps = slices.Concat(secretSteps, tlsSteps, byPhase[plan.PhaseProvider], moduleSteps, byPhase[plan.PhaseJob], verify, byPhase[plan.PhaseConsumer])
	return pl, nil
}

// verifySteps are the final access checks: one per selected module instance that a workload of the
// topology binds, so a consumer never starts on an instance that failed its check. A run adds none.
func (s *session) verifySteps() []plan.Step {
	if s.req.Command != state.CommandUp {
		return nil
	}
	bound := map[string]bool{}
	for _, b := range s.req.Project.Bindings() {
		consumer, source := b.Ref().Consumer, b.Source().Object
		if _, present := s.top.Find(consumer.Key().String()); present && consumer.Kind() == model.ObjectWorkload && source.Kind() == model.ObjectModule {
			bound[source.Key().String()] = true
		}
	}
	var steps []plan.Step
	for _, inst := range s.top.Instances {
		if _, ok := s.e.drivers[inst.Definition]; ok && bound[inst.Key] && s.instanceSelected(inst) {
			steps = append(steps, plan.Step{Action: plan.ActionModuleVerify, Object: "module/" + inst.Key, Verify: &plan.ModuleVerify{Instance: inst.Key}})
		}
	}
	return steps
}

func (s *session) secretInputSteps() []plan.Step {
	keys := make([]string, 0, len(s.req.SecretInputs))
	for key := range s.req.SecretInputs {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	var steps []plan.Step
	for _, key := range keys {
		steps = append(steps, plan.Step{Action: plan.ActionSecretResolve, Object: "secret/input/" + key})
	}
	return steps
}

// addSecretNeeds adds one secret.create step for each secret a driver step needs and no credential
// or earlier step provides.
func (s *session) addSecretNeeds(secretSteps, driverSteps []plan.Step) []plan.Step {
	for _, step := range driverSteps {
		need := step.Secret
		if need != nil && !s.hasSecret(need.Ref) && !hasStep(secretSteps, "secret/"+need.Ref) {
			secretSteps = append(secretSteps, plan.Step{Action: plan.ActionSecretCreate, Object: "secret/" + need.Ref, Secret: need})
		}
	}
	return secretSteps
}

func hasStep(steps []plan.Step, object string) bool {
	return slices.ContainsFunc(steps, func(s plan.Step) bool { return s.Object == object })
}

func (s *session) hasSecret(ref string) bool {
	if s.st.Accepted == nil {
		return false
	}
	_, ok := s.st.Accepted.Credentials[ref]
	return ok
}

// instance returns the port's view of a topology instance, with the scope it runs in.
func (s *session) instance(i plan.Instance) module.Instance {
	return module.Instance{Key: i.Key, Definition: i.Definition, Version: i.Version, Options: i.Options, Scope: s.names().ScopeUID}
}

func (s *session) instanceMappings(key string) []module.Mapping {
	r, ok := s.st.Realizations[plan.RealizationKey("module", key)]
	if !ok {
		return nil
	}
	out := make([]module.Mapping, 0, len(r.Mappings))
	for _, m := range r.Mappings {
		out = append(out, module.Mapping{Kind: m.Kind, Name: m.Name, ID: m.ID})
	}
	return out
}

// driverCall names one driver call: the instance whose scope its probes may touch, and the call.
type driverCall struct {
	instance string
	what     string
}

func (d driverCall) String() string { return d.instance + " " + d.what }

// rounds runs the request and probe loop of one driver call, at most module.MaxProbeRounds times.
func rounds[T any](ctx context.Context, s *session, d driverCall, call func(results []module.ProbeResult) ([]module.Probe, *T, error)) (T, error) {
	what := d.String()
	var zero T
	var results []module.ProbeResult
	for range module.MaxProbeRounds {
		probes, final, err := call(results)
		if err != nil {
			return zero, fail(state.CodeEffectFailed, state.CategoryUnexpected, "module %s", what).wrap(err)
		}
		if final != nil {
			return *final, nil
		}
		if len(probes) == 0 {
			return zero, fail(state.CodeEffectFailed, state.CategoryUnexpected, "module %s returned neither probes nor a result", what)
		}
		results = results[:0:0]
		for _, p := range probes {
			res, err := s.runProbe(ctx, d.instance, p)
			if err != nil {
				return zero, err
			}
			results = append(results, res)
		}
	}
	return zero, fail(state.CodeEffectFailed, state.CategoryUnexpected, "module %s asked for more than %d probe rounds", what, module.MaxProbeRounds)
}

// runProbe runs one probe a driver asked for, after refusing what a driver must not reach: a target
// outside the calling instance's workloads, a secret outside the instance's namespace, and an
// environment name that could override the docker client's own settings.
func (s *session) runProbe(ctx context.Context, instance string, p module.Probe) (module.ProbeResult, error) {
	if err := checkProbeScope(instance, p); err != nil {
		return module.ProbeResult{}, err
	}
	refs := make([]string, 0, len(p.SecretEnv))
	for _, b := range p.SecretEnv {
		refs = append(refs, b.Ref)
	}
	call, err := s.call(ctx, refs)
	if err != nil {
		return module.ProbeResult{}, err
	}
	if err := s.deriveSecretForms(&call, p); err != nil {
		return module.ProbeResult{}, err
	}
	res, err := s.e.d.Runtime.Probe(ctx, call, p)
	if err != nil {
		return module.ProbeResult{}, fail(state.CodeEffectFailed, state.CategoryUnexpected, "probe %s", p.ID).wrap(err)
	}
	return res, nil
}

// deriveSecretForms adds to the call the derived value each form binding of the probe asks for. The
// plaintext and the derived value live only in the probe child's environment.
func (s *session) deriveSecretForms(call *port.Call, p module.Probe) error {
	for _, b := range p.SecretEnv {
		if b.Form == module.FormValue {
			continue
		}
		plain, ok := call.Secrets[b.Ref]
		if !ok {
			return fail(state.CodeEffectFailed, state.CategoryUnexpected, "probe %s needs secret %s that was not resolved", p.ID, b.Ref)
		}
		verifier, err := secrets.SCRAMSHA256(secrets.NewValue(plain.Reveal()), s.e.d.Random)
		if err != nil {
			return fail(state.CodeEffectFailed, state.CategoryUnexpected, "probe %s: derive the %s form of %s", p.ID, b.Form, b.Ref).wrap(err)
		}
		call.Secrets[port.SecretKey(b.Ref, b.Form)] = verifier
	}
	return nil
}

func envNamePattern() *regexp.Regexp { return regexp.MustCompile(`^[A-Z_][A-Z0-9_]*$`) }

// reservedEnvPrefixes belong to the docker client, compose and the engine's own secret delivery.
func reservedEnvPrefixes() []string { return []string{"DOCKER_", "COMPOSE_", "SP_SECRET_"} }

// inputNamespace holds the user's secret inputs; no driver may reach it, whatever its instance is called.
const inputNamespace = "input/"

func checkProbeScope(instance string, p module.Probe) error {
	if !strings.HasPrefix(p.Target, instance+"--") {
		return fail(state.CodeOutOfScope, state.CategoryForbidden, "probe %s targets %q, which is not a workload of instance %s", p.ID, p.Target, instance)
	}
	for _, b := range p.SecretEnv {
		if !strings.HasPrefix(b.Ref, instance+"/") || strings.HasPrefix(b.Ref, inputNamespace) {
			return fail(state.CodeOutOfScope, state.CategoryForbidden, "probe %s asks for secret %q outside the namespace of instance %s", p.ID, b.Ref, instance)
		}
		if b.Form != module.FormValue && b.Form != module.FormSCRAMSHA256 {
			return fail(state.CodeOutOfScope, state.CategoryForbidden, "probe %s binds secret %q in the form %q, which the engine does not know", p.ID, b.Ref, b.Form)
		}
		if !envNamePattern().MatchString(b.Name) || slices.Contains(config.ChildEnvNames(), b.Name) ||
			slices.ContainsFunc(reservedEnvPrefixes(), func(prefix string) bool { return strings.HasPrefix(b.Name, prefix) }) {
			return fail(state.CodeOutOfScope, state.CategoryForbidden, "probe %s binds secret %q to the environment name %q, which a driver may not use", p.ID, b.Ref, b.Name)
		}
	}
	return nil
}

func (s *session) planInstance(ctx context.Context, driver module.Driver, inst plan.Instance) ([]plan.Step, error) {
	instance := s.instance(inst)
	mappings := s.instanceMappings(inst.Key)
	observation, err := rounds(ctx, s, driverCall{inst.Key, "observe"}, func(results []module.ProbeResult) ([]module.Probe, *module.Observation, error) {
		resp, err := driver.Observe(ctx, module.ObserveRequest{Instance: instance, Mappings: mappings, Results: results})
		return resp.Probes, resp.Observation, err
	})
	if err != nil {
		return nil, err
	}
	driverSteps, err := rounds(ctx, s, driverCall{inst.Key, "plan"}, func(results []module.ProbeResult) ([]module.Probe, *[]module.Step, error) {
		resp, err := driver.Plan(ctx, module.PlanRequest{Instance: instance, Mappings: mappings, Observation: observation, Results: results})
		if resp.Probes == nil && err == nil {
			return nil, &resp.Steps, nil
		}
		return resp.Probes, nil, err
	})
	if err != nil {
		return nil, err
	}
	return s.moduleSteps(driver, inst, driverSteps)
}

func (s *session) moduleSteps(driver module.Driver, inst plan.Instance, driverSteps []module.Step) ([]plan.Step, error) {
	desc := driver.Describe()
	var out []plan.Step
	for _, ds := range driverSteps {
		object := "module/" + inst.Key + "/" + strings.TrimPrefix(ds.Object, "/")
		if !actionPattern().MatchString(ds.Action) || !strings.HasPrefix(ds.Action, desc.Module+".") || !objectPattern().MatchString(object) {
			return nil, fail(state.CodeEffectFailed, state.CategoryUnexpected, "module %s planned a step with an invalid action or object (%q, %q)", desc.Module, ds.Action, ds.Object)
		}
		if ds.Secret != nil && (!strings.HasPrefix(ds.Secret.Ref, inst.Key+"/") || strings.HasPrefix(ds.Secret.Ref, inputNamespace)) {
			return nil, fail(state.CodeOutOfScope, state.CategoryForbidden, "module %s planned a secret %q outside the namespace of instance %s", desc.Module, ds.Secret.Ref, inst.Key)
		}
		if ds.Destructive && !desc.Deletes {
			return nil, fail(state.CodeEffectFailed, state.CategoryUnexpected, "module %s planned a destructive step but does not describe the delete capability", desc.Module)
		}
		out = append(out, plan.Step{Action: ds.Action, Object: object, Secret: ds.Secret, Module: &plan.ModuleStep{Instance: inst.Key, Step: ds}, Unknown: true})
	}
	return out, nil
}

// applyModuleStep runs one driver step and folds its mappings into the instance's realization.
func (s *session) applyModuleStep(ctx context.Context, step plan.Step) (string, error) {
	// A signal ends ctx, and the runner then refuses the next child. A started step must finish its
	// probe rounds (a database created and not yet marked is refused by every later up), so the
	// rounds run detached from the cancellation; the runner's probe timeout still bounds them, and
	// the loop checks ctx between steps.
	ctx = context.WithoutCancel(ctx)
	inst, ok := findInstance(s.top, step.Module.Instance)
	if !ok {
		return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "instance %s is not in the topology", step.Module.Instance)
	}
	driver := s.e.drivers[inst.Definition]
	outcome, err := rounds(ctx, s, driverCall{inst.Key, "apply"}, func(results []module.ProbeResult) ([]module.Probe, *module.Outcome, error) {
		resp, err := driver.Apply(ctx, module.ApplyRequest{Instance: s.instance(inst), Step: step.Module.Step, Results: results})
		return resp.Probes, resp.Outcome, err
	})
	if err != nil {
		return "", err
	}
	key := plan.RealizationKey("module", inst.Key)
	s.update(key, func(r *state.Realization) {
		for _, m := range outcome.Mappings {
			r.Mappings = upsertMapping(r.Mappings, state.Mapping{Kind: m.Kind, Name: m.Name, ID: m.ID, CreatedAt: s.e.now()})
		}
		r.AppliedRevision = s.st.Operation.Revision
	})
	return outcome.Evidence, nil
}

func upsertMapping(list []state.Mapping, m state.Mapping) []state.Mapping {
	for i := range list {
		if list[i].Kind == m.Kind && list[i].Name == m.Name {
			list[i].ID = m.ID
			return list
		}
	}
	return append(list, m)
}

func findInstance(top plan.Topology, key string) (plan.Instance, bool) {
	for _, i := range top.Instances {
		if i.Key == key {
			return i, true
		}
	}
	return plan.Instance{}, false
}

// verifyModules asks every selected driver whose instance no verify step has checked yet whether its
// instance meets its options.
func (s *session) verifyModules(ctx context.Context) error {
	for _, inst := range s.top.Instances {
		if _, ok := s.e.drivers[inst.Definition]; !ok || !s.instanceSelected(inst) || s.verified[inst.Key] {
			continue
		}
		if err := s.verifyInstance(ctx, inst); err != nil {
			return err
		}
	}
	return nil
}

// runVerifyStep runs the final access check of one instance as a step.
func (s *session) runVerifyStep(ctx context.Context, instance string) (string, error) {
	inst, ok := findInstance(s.top, instance)
	if !ok {
		return "", fail(state.CodeEffectFailed, state.CategoryUnexpected, "instance %s is not in the topology", instance)
	}
	if err := s.verifyInstance(ctx, inst); err != nil {
		return "", err
	}
	return "verified module instance " + instance, nil
}

func (s *session) verifyInstance(ctx context.Context, inst plan.Instance) error {
	// Verification observes after the effects; a signal must not mislabel it as a failure.
	ctx = context.WithoutCancel(ctx)
	driver := s.e.drivers[inst.Definition]
	verdict, err := rounds(ctx, s, driverCall{inst.Key, "verify"}, func(results []module.ProbeResult) ([]module.Probe, *module.Verification, error) {
		resp, err := driver.Verify(ctx, module.VerifyRequest{Instance: s.instance(inst), Mappings: s.instanceMappings(inst.Key), Results: results})
		return resp.Probes, resp.Verification, err
	})
	if err != nil {
		return err
	}
	if !verdict.OK {
		return fail(state.CodeEffectFailed, state.CategoryUnexpected, "module instance %s failed verification: %s", inst.Key, strings.Join(verdict.Problems, "; "))
	}
	if s.verified == nil {
		s.verified = map[string]bool{}
	}
	s.verified[inst.Key] = true
	return nil
}
