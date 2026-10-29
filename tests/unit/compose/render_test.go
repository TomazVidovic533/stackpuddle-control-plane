package compose_test

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/runtime/compose"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
)

func scope() plan.ScopeNames {
	return plan.ScopeNames{
		ScopeUID: "sc-01J9Z8K3M4N5P6Q7R8S9T0V1W3", ComposeProject: "shop-dev-s9t0v1", ProjectDir: "/work/shop",
		Realizations: map[string]string{"service/api": "rz-01J9Z8K3M4N5P6Q7R8S9T0V1W4"},
		Artifacts:    map[string]state.Artifact{},
	}
}

func api() plan.Workload {
	return plan.Workload{
		Name: "api", Type: plan.TypeService, Image: "ghcr.io/example/api:1.0",
		Command: []string{"serve", "--token=$TOKEN"},
		Env:     []plan.EnvVar{{Name: "MODE", Value: "standard"}, {Name: "LITERAL", Value: "${SP_SECRET_1}"}},
		Ports:   []plan.Port{{Container: 8080, Host: 18080}, {Container: 9090}},
		Volumes: []plan.VolumeMount{{Name: "api--uploads", Path: "/data/$HOME"}},
	}
}

type rendered struct {
	Services map[string]struct {
		Image       string            `json:"image"`
		Build       map[string]string `json:"build"`
		PullPolicy  string            `json:"pull_policy"`
		Command     []string          `json:"command"`
		Environment map[string]string `json:"environment"`
		Labels      map[string]string `json:"labels"`
		Ports       []map[string]any  `json:"ports"`
		Volumes     []map[string]any  `json:"volumes"`
		Secrets     []map[string]any  `json:"secrets"`
	} `json:"services"`
	Volumes map[string]map[string]any `json:"volumes"`
	Secrets map[string]map[string]any `json:"secrets"`
}

func render(t *testing.T, s plan.ScopeNames, workloads ...plan.Workload) (plan.Rendered, rendered) {
	t.Helper()
	out, err := compose.Render(s, plan.Topology{Workloads: workloads})
	if err != nil {
		t.Fatalf("Render: %v", err)
	}
	var parsed rendered
	if err := json.Unmarshal(out.Compose, &parsed); err != nil {
		t.Fatalf("the output is not JSON: %v\n%s", err, out.Compose)
	}
	return out, parsed
}

func TestRender_EscapesEveryStringFromTheModel(t *testing.T) {
	t.Parallel()
	out, parsed := render(t, scope(), api())
	svc := parsed.Services["api"]
	if svc.Environment["LITERAL"] != "$${SP_SECRET_1}" {
		t.Errorf("a model value with a variable reference is %q, want it doubled", svc.Environment["LITERAL"])
	}
	if svc.Command[1] != "--token=$$TOKEN" {
		t.Errorf("command = %q", svc.Command)
	}
	if got := svc.Volumes[0]["target"]; got != "/data/$$HOME" {
		t.Errorf("volume target = %v", got)
	}
	// After doubling, no "$" is left that is not part of "$$".
	if strings.Contains(strings.ReplaceAll(string(out.Compose), "$$", ""), "$") {
		t.Errorf("a single $ remains in the rendered file:\n%s", out.Compose)
	}
}

func TestRender_EscapesImageAndBuildFields(t *testing.T) {
	t.Parallel()
	w := plan.Workload{Name: "svc", Type: plan.TypeService, Build: &plan.Build{Context: "src/$dir", Dockerfile: "Docker$file", Target: "t$"}}
	_, parsed := render(t, scope(), w)
	build := parsed.Services["svc"].Build
	if build["context"] != "/work/shop/src/$$dir" || build["dockerfile"] != "Docker$$file" || build["target"] != "t$$" {
		t.Errorf("build = %v", build)
	}
	img := plan.Workload{Name: "img", Type: plan.TypeService, Image: "reg/$x:1"}
	_, parsed = render(t, scope(), img)
	if parsed.Services["img"].Image != "reg/$$x:1" {
		t.Errorf("image = %q", parsed.Services["img"].Image)
	}
}

func TestRender_RefusesABuildContextOutsideTheProject(t *testing.T) {
	t.Parallel()
	for _, context := range []string{"../escape", "/abs", "a/../../b", ""} {
		w := plan.Workload{Name: "svc", Type: plan.TypeService, Build: &plan.Build{Context: context}}
		if _, err := compose.Render(scope(), plan.Topology{Workloads: []plan.Workload{w}}); !errors.Is(err, compose.ErrBuildContext) {
			t.Errorf("context %q: error = %v, want ErrBuildContext", context, err)
		}
	}
}

func TestRender_PublishesOnlyDeclaredPortsOnLoopback(t *testing.T) {
	t.Parallel()
	_, parsed := render(t, scope(), api())
	ports := parsed.Services["api"].Ports
	if len(ports) != 2 {
		t.Fatalf("ports = %v", ports)
	}
	for _, p := range ports {
		if p["host_ip"] != "127.0.0.1" {
			t.Errorf("port %v is not bound to loopback", p)
		}
	}
	if ports[0]["published"] != "18080" {
		t.Errorf("fixed port = %v", ports[0])
	}
	if _, ok := ports[1]["published"]; ok {
		t.Errorf("a run-time assigned port declares published: %v", ports[1])
	}
	_, none := render(t, scope(), plan.Workload{Name: "quiet", Type: plan.TypeService, Image: "x"})
	if len(none.Services["quiet"].Ports) != 0 {
		t.Errorf("a workload with no exposure publishes %v", none.Services["quiet"].Ports)
	}
}

func TestRender_LabelsNamesAndVolumes(t *testing.T) {
	t.Parallel()
	out, parsed := render(t, scope(), api())
	svc := parsed.Services["api"]
	if svc.Labels[plan.LabelScope] != scope().ScopeUID || svc.Labels[plan.LabelRealization] != "rz-01J9Z8K3M4N5P6Q7R8S9T0V1W4" {
		t.Errorf("labels = %v", svc.Labels)
	}
	if svc.Labels[plan.LabelSpec] == "" || svc.Labels[plan.LabelSpec] != out.SpecHashes["api"] {
		t.Errorf("spec label %q, hashes %v", svc.Labels[plan.LabelSpec], out.SpecHashes)
	}
	if svc.PullPolicy != "never" {
		t.Errorf("pull policy = %q: a repeated up must not pull", svc.PullPolicy)
	}
	if got := parsed.Volumes["api--uploads"]["name"]; got != "shop-dev-s9t0v1_api--uploads" {
		t.Errorf("volume name = %v", got)
	}
	if parsed.Secrets != nil {
		t.Errorf("secrets = %v although no workload uses one", parsed.Secrets)
	}
}

func TestRender_SecretsComeFromTheChildEnvironmentOnly(t *testing.T) {
	t.Parallel()
	w := api()
	w.Files = []plan.File{
		{Path: "/run/secrets/admin", Source: plan.Source{Kind: plan.SourceSecret, Ref: "primary/admin"}},
		{Path: "/run/secrets/db", Source: plan.Source{Kind: plan.SourceSecret, Ref: "input/db-password"}},
	}
	out, parsed := render(t, scope(), w)
	if out.SecretVars["input/db-password"] != "SP_SECRET_1" || out.SecretVars["primary/admin"] != "SP_SECRET_2" {
		t.Errorf("secret variables = %v", out.SecretVars)
	}
	for key, def := range parsed.Secrets {
		if _, hasFile := def["file"]; hasFile {
			t.Errorf("secret %s is sourced from a host file: %v", key, def)
		}
		if !regexp.MustCompile(`^SP_SECRET_[0-9]+$`).MatchString(def["environment"].(string)) {
			t.Errorf("secret %s = %v", key, def)
		}
	}
	for _, mount := range parsed.Services["api"].Volumes {
		if mount["type"] != "volume" {
			t.Errorf("a service mount is not a named volume: %v", mount)
		}
	}
	secretFiles := parsed.Services["api"].Secrets
	if len(secretFiles) != 2 || secretFiles[0]["target"] != "admin" || secretFiles[1]["target"] != "db" {
		t.Errorf("secret files = %v", secretFiles)
	}
	for _, f := range secretFiles {
		if f["mode"] != float64(0o400) {
			t.Errorf("secret file %v is not owner read-only", f)
		}
	}
}

func TestRender_RecordedArtifactReplacesTheDeclaredImage(t *testing.T) {
	t.Parallel()
	s := scope()
	w := plan.Workload{Name: "b", Type: plan.TypeService, Build: &plan.Build{Context: "src"}}
	_, built := render(t, s, w)
	if built.Services["b"].Build == nil {
		t.Fatal("an unbuilt workload renders no build")
	}
	s.Artifacts["b"] = state.Artifact{ImageID: "sha256:abc"}
	_, recorded := render(t, s, w)
	if got := recorded.Services["b"]; got.Image != "sha256:abc" || got.Build != nil {
		t.Errorf("after the build is recorded: image %q, build %v", got.Image, got.Build)
	}
	s.Artifacts["a"] = state.Artifact{Image: "reg/app@sha256:def"}
	_, pinned := render(t, s, plan.Workload{Name: "a", Type: plan.TypeService, Image: "reg/app:1"})
	if pinned.Services["a"].Image != "reg/app@sha256:def" {
		t.Errorf("pinned image = %q", pinned.Services["a"].Image)
	}
}

func TestRender_IsDeterministicAndTheHashTracksTheSpec(t *testing.T) {
	t.Parallel()
	a, _ := render(t, scope(), api())
	b, _ := render(t, scope(), api())
	if string(a.Compose) != string(b.Compose) {
		t.Error("two renders of one topology differ")
	}
	edited := api()
	edited.Env = append(edited.Env, plan.EnvVar{Name: "NEW", Value: "1"})
	c, _ := render(t, scope(), edited)
	if c.SpecHashes["api"] == a.SpecHashes["api"] {
		t.Error("an environment change did not change the spec hash")
	}
}

func TestRender_HasNoModuleSpecificBranch(t *testing.T) {
	t.Parallel()
	// Two instances of one definition and a workload named after a product render alike.
	a := plan.Workload{Name: "primary--store", Instance: "primary", Definition: "kv-store", Type: plan.TypeService, Image: "x"}
	b := plan.Workload{Name: "postgres", Instance: "pg", Definition: "postgres", Type: plan.TypeService, Image: "x"}
	_, parsed := render(t, scope(), a, b)
	if len(parsed.Services) != 2 {
		t.Fatalf("services = %v", parsed.Services)
	}
	left, right := parsed.Services["primary--store"], parsed.Services["postgres"]
	if left.Image != right.Image || left.PullPolicy != right.PullPolicy || len(left.Labels) != len(right.Labels) {
		t.Errorf("a product name changed the rendering: %+v vs %+v", left, right)
	}
}
