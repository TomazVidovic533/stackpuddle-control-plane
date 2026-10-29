// Package compose is the only code in the module that runs Docker. It renders a runtime-neutral
// topology to Compose JSON (docs/architecture/decisions/0001-compose-renderer.md) and runs every
// effect as a child process of the declared Docker context.
package compose

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
)

// Sentinels for the failures a caller classifies.
var (
	// ErrRender marks a topology that cannot be rendered.
	ErrRender = errors.New("topology cannot be rendered")
	// ErrBuildContext marks a build context outside the project directory.
	ErrBuildContext = errors.New("build context is outside the project directory")
)

const (
	loopback     = "127.0.0.1"
	secretPrefix = "SP_SECRET_"
	hashChars    = 16
	kindVolume   = "volume"
	// defaultSecretMode is 0400, written as the decimal JSON number Compose reads as the octal mode.
	defaultSecretMode = 0o400
	// SecretMountDir is where Compose serves a secret file inside a container.
	secretMountDir = "/run/secrets"
)

type composeFile struct {
	Services map[string]service `json:"services"`
	Volumes  map[string]volume  `json:"volumes,omitempty"`
	Secrets  map[string]secret  `json:"secrets,omitempty"`
}

type service struct {
	Image       string            `json:"image"`
	Build       *build            `json:"build,omitempty"`
	PullPolicy  string            `json:"pull_policy"`
	Command     []string          `json:"command,omitempty"`
	Environment map[string]string `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels"`
	Ports       []publishedPort   `json:"ports,omitempty"`
	Volumes     []mount           `json:"volumes,omitempty"`
	Secrets     []serviceSecret   `json:"secrets,omitempty"`
}

type build struct {
	Context    string `json:"context"`
	Dockerfile string `json:"dockerfile,omitempty"`
	Target     string `json:"target,omitempty"`
}

type publishedPort struct {
	Target    int    `json:"target"`
	Published string `json:"published,omitempty"`
	HostIP    string `json:"host_ip"`
	Protocol  string `json:"protocol"`
}

type mount struct {
	Type   string `json:"type"`
	Source string `json:"source"`
	Target string `json:"target"`
}

type volume struct {
	Name string `json:"name"`
}

type secret struct {
	Environment string `json:"environment"`
}

type serviceSecret struct {
	Source string `json:"source"`
	Target string `json:"target"`
	// UID and GID own the file inside the container; empty leaves Compose's default.
	UID string `json:"uid,omitempty"`
	GID string `json:"gid,omitempty"`
	// Mode is the file mode in the container; Compose reads a JSON number as the octal mode.
	Mode int `json:"mode"`
}

// escape doubles every "$" so Compose interpolation cannot pull a variable of the child's
// environment, such as a secret, into a value taken from the model.
func escape(s string) string { return strings.ReplaceAll(s, "$", "$$") }

// BuiltImageTag is the local tag a build workload's image carries until its ID is recorded.
func BuiltImageTag(composeProject, workload string) string {
	return composeProject + "-" + workload + ":built"
}

// VolumeName is the Docker volume name of a topology volume in a scope.
func VolumeName(composeProject, volume string) string { return composeProject + "_" + volume }

// Render translates the topology to Compose JSON. It is pure. Every string taken from the model is
// written with "$" doubled; no variable reference is ever rendered. Secrets appear only as top-level
// Compose secrets sourced from SP_SECRET_n variables of the child's environment. The result also
// carries the per-workload spec hash that observation compares.
func Render(scope plan.ScopeNames, top plan.Topology) (plan.Rendered, error) {
	file := composeFile{Services: map[string]service{}, Volumes: map[string]volume{}, Secrets: map[string]secret{}}
	vars := secretVars(top)
	hashes := map[string]string{}
	for _, w := range top.Workloads {
		svc, err := renderService(scope, w, vars)
		if err != nil {
			return plan.Rendered{}, err
		}
		hash, err := specHash(svc)
		if err != nil {
			return plan.Rendered{}, err
		}
		svc.Labels[plan.LabelSpec] = hash
		hashes[w.Name] = hash
		file.Services[w.Name] = svc
		for _, m := range w.Volumes {
			file.Volumes[m.Name] = volume{Name: VolumeName(scope.ComposeProject, m.Name)}
		}
		for _, f := range w.Files {
			variable := vars[f.Source.Key()]
			file.Secrets[secretName(variable)] = secret{Environment: variable}
		}
	}
	if len(file.Volumes) == 0 {
		file.Volumes = nil
	}
	if len(file.Secrets) == 0 {
		file.Secrets = nil
	}
	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return plan.Rendered{}, fmt.Errorf("%w: %w", ErrRender, err)
	}
	return plan.Rendered{Compose: data, SecretVars: vars, Literals: literals(top), SpecHashes: hashes}, nil
}

// secretVars numbers the sources of every delivered file in key order: the same file set always gets
// the same variables, whatever order the workloads were listed in.
func secretVars(top plan.Topology) map[string]string {
	var keys []string
	for _, w := range top.Workloads {
		for _, f := range w.Files {
			if key := f.Source.Key(); !slices.Contains(keys, key) {
				keys = append(keys, key)
			}
		}
	}
	slices.Sort(keys)
	out := map[string]string{}
	for i, key := range keys {
		out[key] = secretPrefix + strconv.Itoa(i+1)
	}
	return out
}

// literals maps the key of each text source to its public text, which the caller puts in the child
// environment like any other delivered value.
func literals(top plan.Topology) map[string]string {
	out := map[string]string{}
	for _, w := range top.Workloads {
		for _, f := range w.Files {
			if f.Source.Kind == plan.SourceText {
				out[f.Source.Key()] = f.Source.Text
			}
		}
	}
	return out
}

// secretName is the Compose secret name of a source variable; it is unique per source by construction.
func secretName(variable string) string {
	return strings.ToLower(strings.ReplaceAll(variable, "_", "-"))
}

func renderService(scope plan.ScopeNames, w plan.Workload, vars map[string]string) (service, error) {
	svc := service{PullPolicy: "never", Labels: map[string]string{plan.LabelScope: scope.ScopeUID}}
	if uid, ok := scope.Realizations[plan.RealizationKey(w.Type, w.Name)]; ok {
		svc.Labels[plan.LabelRealization] = uid
	}
	if err := renderImage(&svc, scope, w); err != nil {
		return service{}, err
	}
	for _, c := range w.Command {
		svc.Command = append(svc.Command, escape(c))
	}
	if len(w.Env) > 0 {
		svc.Environment = map[string]string{}
		for _, e := range w.Env {
			svc.Environment[e.Name] = escape(e.Value)
		}
	}
	svc.Ports = renderPorts(w)
	for _, m := range w.Volumes {
		svc.Volumes = append(svc.Volumes, mount{Type: kindVolume, Source: m.Name, Target: escape(m.Path)})
	}
	secrets, err := renderFiles(w, vars)
	svc.Secrets = secrets
	if len(w.Files) > 0 {
		svc.Labels[plan.LabelFiles] = filesHash(w, scope.Delivered)
	}
	return svc, err
}

// filesHash digests what is delivered into a workload and the version token of each source, so a
// rotated secret or a reissued certificate changes the rendered service. It holds no value.
func filesHash(w plan.Workload, delivered map[string]string) string {
	h := sha256.New()
	for _, f := range w.Files {
		key := f.Source.Key()
		fmt.Fprintf(h, "%s|%o|%s|%s|%s\n", f.Path, f.Mode, f.Owner, key, delivered[key])
	}
	return hex.EncodeToString(h.Sum(nil))[:hashChars]
}

// renderPorts publishes only declared ports, always on the loopback interface.
func renderPorts(w plan.Workload) []publishedPort {
	var ports []publishedPort
	for _, p := range w.Ports {
		rendered := publishedPort{Target: p.Container, HostIP: loopback, Protocol: "tcp"}
		if p.Host != 0 {
			rendered.Published = strconv.Itoa(p.Host)
		}
		ports = append(ports, rendered)
	}
	return ports
}

func renderFiles(w plan.Workload, vars map[string]string) ([]serviceSecret, error) {
	var out []serviceSecret
	for _, f := range w.Files {
		variable := vars[f.Source.Key()]
		if variable == "" {
			return nil, fmt.Errorf("%w: file %s has no source variable", ErrRender, f.Path)
		}
		uid, gid, err := splitOwner(f.Owner)
		if err != nil {
			return nil, fmt.Errorf("%w: file %s: %w", ErrRender, f.Path, err)
		}
		out = append(out, serviceSecret{Source: secretName(variable), Target: escape(secretTarget(f.Path)), UID: uid, GID: gid, Mode: secretMode(f.Mode)})
	}
	return out, nil
}

// splitOwner reads "<uid>:<gid>"; the empty owner leaves both to Compose.
func splitOwner(owner string) (uid, gid string, err error) {
	if owner == "" {
		return "", "", nil
	}
	uid, gid, found := strings.Cut(owner, ":")
	if _, uidErr := strconv.ParseUint(uid, 10, 32); !found || uidErr != nil {
		return "", "", fmt.Errorf("owner %q is not <uid>:<gid>", owner)
	}
	if _, gidErr := strconv.ParseUint(gid, 10, 32); gidErr != nil {
		return "", "", fmt.Errorf("owner %q is not <uid>:<gid>", owner)
	}
	return uid, gid, nil
}

// secretMode is the container file mode of a secret: owner read-only unless the delivery says otherwise.
func secretMode(mode uint32) int {
	if mode == 0 {
		return defaultSecretMode
	}
	return int(mode)
}

func secretTarget(p string) string {
	if path.IsAbs(p) && path.Dir(p) == secretMountDir {
		return path.Base(p)
	}
	return p
}

func renderImage(svc *service, scope plan.ScopeNames, w plan.Workload) error {
	if artifact, ok := scope.Artifacts[w.Name]; ok && (artifact.Image != "" || artifact.ImageID != "") {
		svc.Image = artifact.Image
		if artifact.ImageID != "" {
			svc.Image = artifact.ImageID
		}
		return nil
	}
	if w.Build == nil {
		svc.Image = escape(w.Image)
		return nil
	}
	context := w.Build.Context
	if !filepath.IsLocal(context) {
		return fmt.Errorf("%w: %q", ErrBuildContext, context)
	}
	svc.Image = BuiltImageTag(scope.ComposeProject, w.Name)
	svc.Build = &build{
		Context:    escape(filepath.Join(scope.ProjectDir, filepath.FromSlash(context))),
		Dockerfile: escape(w.Build.Dockerfile),
		Target:     escape(w.Build.Target),
	}
	return nil
}

func specHash(svc service) (string, error) {
	data, err := json.Marshal(svc)
	if err != nil {
		return "", fmt.Errorf("%w: %w", ErrRender, err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])[:hashChars], nil
}
