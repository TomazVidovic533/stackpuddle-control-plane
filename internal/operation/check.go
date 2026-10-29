package operation

import (
	"fmt"
	"slices"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle/model"
	"github.com/TomazVidovic533/stackpuddle/validation"
)

// validateProject is the library-boundary defence: a project with diagnostics, or with definitions
// no validator checked, is refused before anything is provisioned.
func validateProject(p model.Project) error {
	result, err := validation.Validate(p)
	if err != nil {
		return fmt.Errorf("operation: validate project: %w", err)
	}
	if len(result.Diagnostics) > 0 {
		codes := make([]string, 0, len(result.Diagnostics))
		for _, d := range result.Diagnostics {
			codes = append(codes, string(d.Code()))
		}
		return fail(state.CodeProjectInvalid, state.CategoryValidation, "the project has %d validation findings: %s", len(codes), strings.Join(codes, ", "))
	}
	if len(result.Unchecked) > 0 {
		return fail(state.CodeProjectUnresolved, state.CategoryValidation, "definitions were not checked: %s", joinNames(result.Unchecked))
	}
	return nil
}

// checkCapabilities refuses a project that requires a capability this engine does not advertise,
// before any lock, state read or Docker call.
func (e *Engine) checkCapabilities(p model.Project, env model.EnvironmentName) error {
	required, ok := p.RequiredCapabilities(env)
	if !ok {
		return fail(state.CodeUnknownObject, state.CategoryNotFound, "environment %s is not declared", env)
	}
	info, err := e.Advertise()
	if err != nil {
		return err
	}
	have := map[string]bool{}
	for _, c := range info.Capabilities {
		have[c.String()] = true
	}
	var missing []string
	for _, c := range required {
		if !have[c.String()] {
			missing = append(missing, c.String())
		}
	}
	if len(missing) > 0 {
		return fail(state.CodeCapabilityUnsupported, state.CategoryValidation, "this engine does not advertise: %s", strings.Join(missing, ", "))
	}
	return nil
}

// checkInputs refuses an input supplied through the wrong channel, before any lock or write. A public
// input must be declared and must not be declared secret, or its text would be stored in state.json
// in plaintext; a secret input must not be declared public, or its value would travel as public text.
func checkInputs(p model.Project, r Request) error {
	declared := map[string]model.Sensitivity{}
	for _, input := range p.Inputs() {
		declared[input.Key().String()] = input.Sensitivity()
	}
	for _, key := range sortedKeys(r.PublicInputs) {
		sensitivity, ok := declared[key]
		switch {
		case !ok:
			return fail(state.CodeInputInvalid, state.CategoryValidation, "input %s is not declared by the project", key)
		case sensitivity == model.SensitivitySecret:
			return fail(state.CodeInputInvalid, state.CategoryValidation, "input %s is declared secret and cannot be supplied as public text", key)
		}
	}
	for _, key := range sortedKeys(r.SecretInputs) {
		if sensitivity, ok := declared[key]; ok && sensitivity != model.SensitivitySecret {
			return fail(state.CodeInputInvalid, state.CategoryValidation, "input %s is declared public and cannot be supplied as a secret", key)
		}
	}
	return nil
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	return keys
}
