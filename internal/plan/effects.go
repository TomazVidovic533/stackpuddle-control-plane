package plan

// Effect is one runtime action a step performs. The set is closed.
//
//sumtype:decl
type Effect interface {
	effect()
	// Target returns the workload or volume name the effect acts on.
	Target() string
}

// ResolveImage pulls a declared image and records its digest.
type ResolveImage struct{ Workload string }

// BuildImage builds a declared build and records the image ID.
type BuildImage struct{ Workload string }

// CreateVolume creates a labelled data volume.
type CreateVolume struct{ Volume string }

// StartService starts one service (and nothing it depends on).
type StartService struct{ Workload string }

// RunJob runs one job to completion.
type RunJob struct{ Workload string }

// StopService stops one service and keeps its container.
type StopService struct{ Workload string }

// RemoveService removes one service's containers.
type RemoveService struct{ Workload string }

// DeleteVolume removes one data volume.
type DeleteVolume struct{ Volume string }

func (ResolveImage) effect()  {}
func (BuildImage) effect()    {}
func (CreateVolume) effect()  {}
func (StartService) effect()  {}
func (RunJob) effect()        {}
func (StopService) effect()   {}
func (RemoveService) effect() {}
func (DeleteVolume) effect()  {}

// Target returns the workload name.
func (e ResolveImage) Target() string { return e.Workload }

// Target returns the workload name.
func (e BuildImage) Target() string { return e.Workload }

// Target returns the volume name.
func (e CreateVolume) Target() string { return e.Volume }

// Target returns the workload name.
func (e StartService) Target() string { return e.Workload }

// Target returns the workload name.
func (e RunJob) Target() string { return e.Workload }

// Target returns the workload name.
func (e StopService) Target() string { return e.Workload }

// Target returns the workload name.
func (e RemoveService) Target() string { return e.Workload }

// Target returns the volume name.
func (e DeleteVolume) Target() string { return e.Volume }
