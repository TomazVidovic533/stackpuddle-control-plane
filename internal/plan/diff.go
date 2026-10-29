package plan

import (
	"slices"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

// Step actions the engine owns. A driver's actions carry its module's namespace instead.
const (
	ActionSecretResolve = "secret.resolve"
	ActionSecretCreate  = "secret.create"
	ActionTLSIssue      = "tls.issue"
	ActionModuleVerify  = "module.verify"
	ActionImage         = "compose.image"
	ActionVolume        = "compose.volume"
	ActionJob           = "compose.job"
	ActionService       = "compose.service"
	ActionStop          = "compose.stop"
	ActionRemove        = "compose.remove"
	ActionVolumeDelete  = "compose.volume-delete"
)

// RealizationKey is the key of a realization in state: "service/api", "job/migrate" or "volume/store--data".
func RealizationKey(kind, name string) string { return kind + "/" + name }

// Phase places a step of an up in the deployment order: providers first, then the prerequisite
// jobs of the consumers, then the consumers. The engine puts the module access steps between the
// first two and the final access check between the last two. Steps of other commands carry none.
type Phase int

// The phases.
const (
	// PhaseProvider holds the workloads other objects bind to, the module workloads, and the jobs they require.
	PhaseProvider Phase = iota + 1
	// PhaseJob holds the prerequisite jobs of the consumers.
	PhaseJob
	// PhaseConsumer holds the consuming services.
	PhaseConsumer
)

// Impact is what a planned step does to the runtime. The set is closed; ImpactBlocked is the
// impact of a finding, which changes nothing.
type Impact int

// The impacts.
const (
	ImpactApply Impact = iota + 1
	ImpactStop
	ImpactDelete
	ImpactBlocked
)

// String returns the lower-case name of the impact.
func (i Impact) String() string {
	switch i {
	case ImpactApply:
		return "apply"
	case ImpactStop:
		return "stop"
	case ImpactDelete:
		return "delete"
	case ImpactBlocked:
		return "blocked"
	}
	return "unknown"
}

// Step is one planned action. Effect is nil for engine steps that need no runtime (secrets) and for
// driver steps, which carry Module instead.
type Step struct {
	Action string
	Object string
	Effect Effect
	// Secret asks the engine to resolve or create a secret before the step runs.
	Secret *module.SecretNeed
	// Module is the driver step, with the instance it belongs to.
	Module *ModuleStep
	// Verify is the final access check of a module instance, run after its consumers' jobs and before
	// the consumers start. It only reads.
	Verify *ModuleVerify
	// Unknown is true when the plan cannot say whether the step still has anything to do: the step
	// was started by an operation that is still open, or a driver's step whose effect the engine
	// cannot see.
	Unknown bool
	// Phase is the step's place in an up; zero for every other step.
	Phase Phase
}

// Impact returns what the step does to the runtime.
func (s Step) Impact() Impact {
	if s.Module != nil {
		if s.Module.Step.Destructive {
			return ImpactDelete
		}
		return ImpactApply
	}
	switch s.Effect.(type) {
	case StopService:
		return ImpactStop
	case RemoveService, DeleteVolume:
		return ImpactDelete
	case ResolveImage, BuildImage, CreateVolume, StartService, RunJob, nil:
		return ImpactApply
	}
	return ImpactApply
}

// ModuleVerify names the module instance whose driver verifies it.
type ModuleVerify struct {
	Instance string
}

// ModuleStep is a step a module driver planned.
type ModuleStep struct {
	Instance string
	Step     module.Step
}

// Finding is a problem the planner found with an object; it blocks the operation.
type Finding struct {
	Object  string
	Problem state.Problem
	Detail  string
}

// Plan is the ordered steps of one operation and what blocks it.
type Plan struct {
	Steps    []Step
	Findings []Finding
	// Lost lists the realization keys whose mapped data object vanished; the caller marks them lost.
	Lost []string
}

// DiffInput is everything Diff reads.
type DiffInput struct {
	Desired    Topology
	State      state.State
	Inventory  Inventory
	Command    state.Command
	Selection  Selection
	DeleteData bool
	// SpecHashes maps a workload name to the hash of its rendered service.
	SpecHashes map[string]string
	// Invoke names the job a run command invokes; the job runs even when its spec is unchanged.
	Invoke string
	// Reissued names the workloads whose delivered certificate this operation replaces, so they restart
	// although their spec hash has not been recomputed yet.
	Reissued map[string]bool
}

// Diff compares the desired topology with the state and the observed inventory and returns the steps
// that bring the runtime to the command's goal. It is pure. It never starts an object whose accepted
// intent is stopped unless this request selects it, and never recreates a vanished data object.
func Diff(in DiffInput) Plan {
	var out Plan
	out.Findings = unmapped(in)
	switch in.Command {
	case state.CommandUp, state.CommandRun:
		diffUp(in, &out)
	case state.CommandStop:
		diffStop(in, &out)
	case state.CommandDestroy:
		diffDestroy(in, &out)
	}
	markStarted(in.State.Operation, &out)
	return out
}

// markStarted flags every planned step that an open operation already started: the effect may have
// happened, so the preview cannot promise it will run.
func markStarted(open *state.Operation, out *Plan) {
	if open == nil {
		return
	}
	for i, step := range out.Steps {
		started := slices.ContainsFunc(open.Steps, func(o state.Step) bool {
			return o.Action == step.Action && o.Object == step.Object && (o.Status == state.StepStarted || o.Status == state.StepBlocked)
		})
		out.Steps[i].Unknown = out.Steps[i].Unknown || started
	}
}

func unmapped(in DiffInput) []Finding {
	known := map[string]bool{}
	for _, r := range in.State.Realizations {
		known[r.UID.String()] = true
	}
	var out []Finding
	for _, o := range in.Inventory.Volumes {
		if !known[o.Realization] {
			out = append(out, Finding{Object: "volume/" + o.Name, Problem: state.Problem{Code: state.CodeUnmappedOwnedObject, Category: state.CategoryConflict}, Detail: "volume " + o.Name + " carries this scope's label but no realization maps it"})
		}
	}
	for _, c := range in.Inventory.Containers {
		if !known[c.Realization] {
			out = append(out, Finding{Object: "container/" + c.Name, Problem: state.Problem{Code: state.CodeUnmappedOwnedObject, Category: state.CategoryConflict}, Detail: "container " + c.Name + " carries this scope's label but no realization maps it"})
		}
	}
	return out
}

func (in DiffInput) selected(name string) bool {
	return in.Selection.All || slices.Contains(in.Selection.Names, name)
}

// ordered lists the workloads with every job before the workloads that require it.
func ordered(top Topology) []Workload {
	var out []Workload
	seen := map[string]bool{}
	var visit func(w Workload)
	visit = func(w Workload) {
		if seen[w.Name] {
			return
		}
		seen[w.Name] = true
		for _, pre := range w.Prerequisites {
			if p, ok := top.Find(pre); ok {
				visit(p)
			}
		}
		out = append(out, w)
	}
	for _, w := range top.Workloads {
		visit(w)
	}
	return out
}

// diffUp plans the workloads in three phases. Within a phase a job comes before the
// workloads that require it. A job is planned only as a prerequisite of a workload that is up or when
// the request invokes it; naming it in a group plans nothing. A run plans the providers the job needs,
// the job's own prerequisites and the job, and starts no consumer.
func diffUp(in DiffInput, out *Plan) {
	phases := phasesOf(in)
	var buckets [PhaseConsumer + 1][]Step
	for _, w := range ordered(in.Desired) {
		phase, planned := phases[w.Name]
		if !planned {
			continue
		}
		key := RealizationKey(w.Type, w.Name)
		if in.State.Accepted != nil && in.State.Accepted.Intent[w.Name] == state.IntentStopped && !in.selected(w.Name) {
			continue
		}
		var local Plan
		upImage(in, w, key, &local)
		upVolumes(in, w, &local)
		upWorkload(in, w, key, &local)
		for _, step := range local.Steps {
			step.Phase = phase
			buckets[phase] = append(buckets[phase], step)
		}
		out.Findings = append(out.Findings, local.Findings...)
		out.Lost = append(out.Lost, local.Lost...)
	}
	for _, phase := range []Phase{PhaseProvider, PhaseJob, PhaseConsumer} {
		out.Steps = append(out.Steps, buckets[phase]...)
	}
}

// phasesOf places every workload an up or run plans in its phase. A workload absent from the result is
// not planned.
func phasesOf(in DiffInput) map[string]Phase {
	top := in.Desired
	providers := closure(top, func(w Workload) bool { return w.Provider })
	required := requiredJobs(in)
	phases := map[string]Phase{}
	for _, w := range top.Workloads {
		switch {
		case providers[w.Name]:
			phases[w.Name] = PhaseProvider
		case w.Type == TypeJob && required[w.Name]:
			phases[w.Name] = PhaseJob
		case w.Type == TypeService && in.Command != state.CommandRun:
			phases[w.Name] = PhaseConsumer
		}
	}
	return phases
}

// requiredJobs are the jobs some planned workload requires, directly or through other jobs: the
// prerequisites of every service, or for a run the invoked job and its own prerequisites.
func requiredJobs(in DiffInput) map[string]bool {
	if in.Command == state.CommandRun {
		return closure(in.Desired, func(w Workload) bool { return w.Name == in.Invoke })
	}
	required := closure(in.Desired, func(w Workload) bool { return w.Type == TypeService })
	for _, w := range in.Desired.Workloads {
		if w.Type == TypeService {
			delete(required, w.Name)
		}
	}
	return required
}

// closure returns the names of the workloads that match, with everything they require.
func closure(top Topology, match func(Workload) bool) map[string]bool {
	seen := map[string]bool{}
	var visit func(w Workload)
	visit = func(w Workload) {
		if seen[w.Name] {
			return
		}
		seen[w.Name] = true
		for _, pre := range w.Prerequisites {
			if p, ok := top.Find(pre); ok {
				visit(p)
			}
		}
	}
	for _, w := range top.Workloads {
		if match(w) {
			visit(w)
		}
	}
	return seen
}

func upImage(in DiffInput, w Workload, key string, out *Plan) {
	if r, ok := in.State.Realizations[key]; ok && r.Artifact != nil {
		return
	}
	object := "image/" + w.Name
	if w.Build != nil {
		out.Steps = append(out.Steps, Step{Action: ActionImage, Object: object, Effect: BuildImage{Workload: w.Name}})
		return
	}
	out.Steps = append(out.Steps, Step{Action: ActionImage, Object: object, Effect: ResolveImage{Workload: w.Name}})
}

func upVolumes(in DiffInput, w Workload, out *Plan) {
	for _, mount := range w.Volumes {
		key := RealizationKey("volume", mount.Name)
		r, exists := in.State.Realizations[key]
		mapped := exists && len(r.Mappings) > 0
		present := slices.ContainsFunc(in.Inventory.Volumes, func(o Object) bool { return o.Name == mount.Name })
		switch {
		case mapped && r.State == state.RealizationLost:
			out.Findings = append(out.Findings, lostFinding(key, mount.Name))
		case mapped && !present:
			out.Findings = append(out.Findings, lostFinding(key, mount.Name))
			out.Lost = append(out.Lost, key)
		case !mapped:
			out.Steps = append(out.Steps, Step{Action: ActionVolume, Object: key, Effect: CreateVolume{Volume: mount.Name}})
		}
	}
}

func lostFinding(key, name string) Finding {
	return Finding{
		Object: key, Problem: state.Problem{Code: state.CodeLostObject, Category: state.CategoryConflict},
		Detail: "volume " + name + " is mapped in state but no longer exists; it is not recreated empty",
	}
}

func upWorkload(in DiffInput, w Workload, key string, out *Plan) {
	r, ok := in.State.Realizations[key]
	hash := in.SpecHashes[w.Name]
	running := slices.ContainsFunc(in.Inventory.Containers, func(c Container) bool { return c.Service == w.Name && c.Running })
	changed := !ok || r.SpecHash != hash || in.Reissued[w.Name]
	if w.Type == TypeJob {
		changed = changed || w.Name == in.Invoke
		if changed {
			out.Steps = append(out.Steps, Step{Action: ActionJob, Object: key, Effect: RunJob{Workload: w.Name}})
		}
		return
	}
	if changed || !running {
		out.Steps = append(out.Steps, Step{Action: ActionService, Object: key, Effect: StartService{Workload: w.Name}})
	}
}

func diffStop(in DiffInput, out *Plan) {
	for _, w := range in.Desired.Workloads {
		if w.Type != TypeService || !in.selected(w.Name) {
			continue
		}
		if slices.ContainsFunc(in.Inventory.Containers, func(c Container) bool { return c.Service == w.Name && c.Running }) {
			out.Steps = append(out.Steps, Step{Action: ActionStop, Object: RealizationKey(TypeService, w.Name), Effect: StopService{Workload: w.Name}})
		}
	}
}

func diffDestroy(in DiffInput, out *Plan) {
	for _, w := range in.Desired.Workloads {
		if !in.selected(w.Name) {
			continue
		}
		if slices.ContainsFunc(in.Inventory.Containers, func(c Container) bool { return c.Service == w.Name }) {
			out.Steps = append(out.Steps, Step{Action: ActionRemove, Object: RealizationKey(w.Type, w.Name), Effect: RemoveService{Workload: w.Name}})
		}
	}
	if !in.DeleteData {
		return
	}
	for _, w := range in.Desired.Workloads {
		if !in.selected(w.Name) {
			continue
		}
		for _, mount := range w.Volumes {
			if slices.ContainsFunc(in.Inventory.Volumes, func(o Object) bool { return o.Name == mount.Name }) {
				out.Steps = append(out.Steps, Step{Action: ActionVolumeDelete, Object: RealizationKey("volume", mount.Name), Effect: DeleteVolume{Volume: mount.Name}})
			}
		}
	}
}

// Observed reports whether the inventory already shows the evidence of an effect, which settles a
// step that was started when an operation died. ok is false when the inventory cannot tell: an image
// pull or build leaves no labelled object, and a present tag does not prove this pull ran, so the
// caller settles it as not applied and repeats it. That is safe because both effects are idempotent;
// it deviates from "unclear is blocked" on purpose (recorded in the fix-round deviations).
func Observed(e Effect, inv Inventory, hashes map[string]string) (applied, ok bool) {
	switch effect := e.(type) {
	case CreateVolume:
		return hasVolume(inv, effect.Volume), true
	case DeleteVolume:
		return !hasVolume(inv, effect.Volume), true
	case ResolveImage, BuildImage:
		return false, false
	case StartService, RunJob, StopService, RemoveService:
		return observedWorkload(e, inv, hashes), true
	}
	return false, false
}

func observedWorkload(e Effect, inv Inventory, hashes map[string]string) bool {
	name := e.Target()
	switch e.(type) {
	case StartService:
		return hasContainer(inv, name, func(c Container) bool { return c.Running && c.SpecHash == hashes[name] })
	case RunJob:
		return hasContainer(inv, name, func(c Container) bool { return !c.Running && c.ExitCode == 0 && c.SpecHash == hashes[name] })
	case StopService:
		return !hasContainer(inv, name, func(c Container) bool { return c.Running })
	case RemoveService:
		return !hasContainer(inv, name, func(Container) bool { return true })
	case ResolveImage, BuildImage, CreateVolume, DeleteVolume:
		return false
	}
	return false
}

func hasVolume(inv Inventory, name string) bool {
	return slices.ContainsFunc(inv.Volumes, func(o Object) bool { return o.Name == name })
}

func hasContainer(inv Inventory, service string, match func(Container) bool) bool {
	return slices.ContainsFunc(inv.Containers, func(c Container) bool { return c.Service == service && match(c) })
}
