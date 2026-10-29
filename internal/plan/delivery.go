package plan

import (
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// Sentinels for the failures a caller classifies.
var (
	// ErrDeliveryConflict marks an environment key or file path that two configuration paths both set.
	ErrDeliveryConflict = errors.New("a delivered key or path is already set by another configuration path")
	// ErrValueUnavailable marks an Export value that no source can deliver.
	ErrValueUnavailable = errors.New("an export value cannot be delivered")
)

// Names of the engine's own secrets and material.
const (
	// MaterialCA is the material reference of the environment's CA certificate.
	MaterialCA = "tls/ca"
	// SecretCAKey is the secret reference of the environment's CA private key.
	SecretCAKey = "tls/ca-key"
)

// ServerMaterial is the material reference of a server certificate.
func ServerMaterial(workload string) string { return "tls/" + workload }

// ServerKey is the secret reference of a server's private key.
func ServerKey(workload string) string { return "tls/server/" + workload }

// MaterialFile is the file name inside the environment's tls folder that holds a material reference.
func MaterialFile(ref string) string { return strings.TrimPrefix(ref, "tls/") + ".crt" }

// Modes of the server files.
const (
	certificateMode = 0o444
	privateKeyMode  = 0o600
)

// ServerFiles are the two files a server workload receives at the fixed module targets.
func ServerFiles(owner, workload string) []File {
	return []File{
		{Path: module.ServerCertPath, Mode: certificateMode, Owner: owner, Source: Source{Kind: SourceMaterial, Ref: ServerMaterial(workload)}},
		{Path: module.ServerKeyPath, Mode: privateKeyMode, Owner: owner, Source: Source{Kind: SourceSecret, Ref: ServerKey(workload)}},
	}
}

// ServerNames are the names a server certificate carries: the scoped service name, plus localhost and
// 127.0.0.1 only when the environment exposes the workload on the host.
func ServerNames(top Topology, workload string) []string {
	names := []string{workload}
	if w, ok := top.Find(workload); ok && len(w.Ports) > 0 {
		names = append(names, "localhost", "127.0.0.1")
	}
	return names
}

// Delivery is what the bindings of one workload put into it. Env holds public text only.
type Delivery struct {
	Env   []EnvVar
	Files []File
}

// Deliveries maps a workload name to what is delivered into it.
type Deliveries map[string]Delivery

// BoundBinding is one binding with everything its delivery needs.
type BoundBinding struct {
	// Consumer is the instance-scoped name of the consuming workload.
	Consumer string
	Ref      model.BindingRef
	Delivery model.Delivery
	// Endpoint is where the consumer reaches the Export; it supplies "host" and "port".
	Endpoint model.Endpoint
	// Values are the Export's declared values and their sensitivity.
	Values []model.ExportValue
	// Bound holds what the module's driver bound, by value name; nil when the Export has no driver.
	Bound map[string]module.BoundValue
}

// BuildDeliveries turns the bindings into the environment keys and files each consumer receives. It is
// pure and deterministic: the same bindings give the same deliveries, with keys and paths sorted. It
// reads no process environment, and the model's strings keep a literal "$". A secret never becomes an
// environment key, and a key or path set twice is refused.
func BuildDeliveries(bindings []BoundBinding) (Deliveries, error) {
	out := Deliveries{}
	for _, b := range bindings {
		d := out[b.Consumer]
		if err := d.addEnvironment(b); err != nil {
			return nil, err
		}
		if err := d.addFiles(b); err != nil {
			return nil, err
		}
		out[b.Consumer] = d
	}
	for name, d := range out {
		slices.SortFunc(d.Env, func(a, b EnvVar) int { return strings.Compare(a.Name, b.Name) })
		slices.SortFunc(d.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
		out[name] = d
	}
	return out, nil
}

func (d *Delivery) addEnvironment(b BoundBinding) error {
	for _, e := range b.Delivery.Environment() {
		text, err := envText(b, e.Value)
		if err != nil {
			return fmt.Errorf("%s: variable %s: %w", b.Ref, e.Key, err)
		}
		if slices.ContainsFunc(d.Env, func(v EnvVar) bool { return v.Name == e.Key.String() }) {
			return fmt.Errorf("%w: %s sets variable %s a second time in %s", ErrDeliveryConflict, b.Ref, e.Key, b.Consumer)
		}
		d.Env = append(d.Env, EnvVar{Name: e.Key.String(), Value: text})
	}
	return nil
}

func (d *Delivery) addFiles(b BoundBinding) error {
	for _, f := range b.Delivery.Files() {
		source, err := fileSource(b, f.Value)
		if err != nil {
			return fmt.Errorf("%s: file %s: %w", b.Ref, f.Path, err)
		}
		if slices.ContainsFunc(d.Files, func(v File) bool { return v.Path == f.Path }) {
			return fmt.Errorf("%w: %s writes path %s a second time in %s", ErrDeliveryConflict, b.Ref, f.Path, b.Consumer)
		}
		owner, _ := f.Owner()
		d.Files = append(d.Files, File{Path: f.Path, Mode: uint32(f.Mode), Owner: owner, Source: source})
	}
	return nil
}

// declared returns the sensitivity of a declared (non-implicit) value.
func declared(b BoundBinding, name model.ValueName) (model.Sensitivity, bool) {
	for _, v := range b.Values {
		if v.Name == name {
			return v.Sensitivity, true
		}
	}
	return 0, false
}

func implicitText(b BoundBinding, name model.ValueName) (string, bool) {
	switch name.String() {
	case "host":
		return b.Endpoint.Host, true
	case "port":
		return strconv.Itoa(b.Endpoint.Port), true
	}
	return "", false
}

// envText is the public text an environment key receives.
func envText(b BoundBinding, name model.ValueName) (string, error) {
	if text, ok := implicitText(b, name); ok {
		return text, nil
	}
	sensitivity, ok := declared(b, name)
	if !ok {
		return "", fmt.Errorf("%w: the export declares no value %q", ErrValueUnavailable, name)
	}
	if sensitivity == model.SensitivitySecret {
		return "", fmt.Errorf("value %q: %w", name, ErrSecretInEnvironment)
	}
	value, bound := b.Bound[name.String()]
	if !bound || value.Text == "" {
		return "", fmt.Errorf("%w: value %q has no text for an environment key", ErrValueUnavailable, name)
	}
	return value.Text, nil
}

// fileSource is where a delivered file's bytes come from.
func fileSource(b BoundBinding, name model.ValueName) (Source, error) {
	if text, ok := implicitText(b, name); ok {
		return Source{Kind: SourceText, Text: text}, nil
	}
	if _, ok := declared(b, name); !ok {
		return Source{}, fmt.Errorf("%w: the export declares no value %q", ErrValueUnavailable, name)
	}
	value, bound := b.Bound[name.String()]
	switch {
	case !bound:
		return Source{}, fmt.Errorf("%w: no module binds value %q", ErrValueUnavailable, name)
	case value.SecretRef != "":
		return Source{Kind: SourceSecret, Ref: value.SecretRef}, nil
	case value.Material == module.MaterialTLSCA:
		return Source{Kind: SourceMaterial, Ref: MaterialCA}, nil
	case value.Text != "":
		return Source{Kind: SourceText, Text: value.Text}, nil
	}
	return Source{}, fmt.Errorf("%w: value %q is empty", ErrValueUnavailable, name)
}

// apply merges a delivery into a workload. A key or path the workload already has is a conflict, so
// no binding-owned key is overwritten.
func (d Delivery) apply(w *Workload) error {
	for _, e := range d.Env {
		if slices.ContainsFunc(w.Env, func(v EnvVar) bool { return v.Name == e.Name }) {
			return fmt.Errorf("%w: variable %s of %s", ErrDeliveryConflict, e.Name, w.Name)
		}
		w.Env = append(w.Env, e)
	}
	slices.SortFunc(w.Env, func(a, b EnvVar) int { return strings.Compare(a.Name, b.Name) })
	for _, f := range d.Files {
		if slices.ContainsFunc(w.Files, func(v File) bool { return v.Path == f.Path }) {
			return fmt.Errorf("%w: path %s of %s", ErrDeliveryConflict, f.Path, w.Name)
		}
		w.Files = append(w.Files, f)
	}
	slices.SortFunc(w.Files, func(a, b File) int { return strings.Compare(a.Path, b.Path) })
	return nil
}
