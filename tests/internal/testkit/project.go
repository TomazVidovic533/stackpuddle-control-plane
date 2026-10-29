package testkit

import (
	"fmt"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/TomazVidovic533/stackpuddle/model"
)

// BusyboxImage is the pinned image the tests run: small, and pulled through the mirror by digest.
const BusyboxImage = "mirror.gcr.io/library/busybox@sha256:a7dbc319b1438115a207e11930dc4bb3e643dcb3ec0bdd694bb36cd8fad289ec"

// ShopOptions vary the sample project so a test can edit "source" between two loads.
type ShopOptions struct {
	// DockerContext is the Docker context the environment declares; "" means "default".
	DockerContext string
	// APICommand replaces the api workload's command; nil keeps "sleep 3600".
	APICommand []string
	// APIEnv adds literal environment variables to the api workload.
	APIEnv map[string]string
	// APISecretInput binds the api workload's TOKEN variable to the db-password secret input, which the
	// engine refuses; it needs SecretInput.
	APISecretInput bool
	// StoreImage replaces the image of the kv-store definition's workload; "" keeps the busybox image.
	StoreImage string
	// Instances lists the module instances of the kv-store definition; nil means primary and reporting.
	Instances []string
	// ExposePort publishes the api http export on this host port; zero publishes nothing.
	ExposePort int
	// NoJob drops the migrate job and the api's prerequisite on it.
	NoJob bool
	// PublicInput declares a public input named log-level that nothing binds.
	PublicInput bool
	// SecretInput declares a secret input named db-password that nothing binds.
	SecretInput bool
	// ExtraWorkload adds a second service named worker.
	ExtraWorkload bool
	// RequireCapability adds a module instance whose definition requires this capability, which the
	// engine does not advertise: a required enforcement the adapter cannot give.
	RequireCapability string
	// Name replaces the project name "shop"; the Docker tests use a name that carries a cleanup prefix.
	Name string
	// BuildAPI makes the api workload build from src/Dockerfile instead of running an image.
	BuildAPI bool
	// JobCommand replaces the migrate job's command; nil keeps "true".
	JobCommand []string
	// APIImage replaces the api workload's pinned image, such as with a tag the engine must resolve.
	APIImage string
}

// ShopFiles returns the files of the sample project.
func ShopFiles(o ShopOptions) map[string]string {
	if o.DockerContext == "" {
		o.DockerContext = "default"
	}
	if o.StoreImage == "" {
		o.StoreImage = BusyboxImage
	}
	command := "[sleep, \"3600\"]"
	if o.APICommand != nil {
		command = "[" + strings.Join(quoteAll(o.APICommand), ", ") + "]"
	}
	instances := o.Instances
	if instances == nil {
		instances = []string{"primary", "reporting"}
	}
	var yaml strings.Builder
	name := o.Name
	if name == "" {
		name = "shop"
	}
	yaml.WriteString("apiVersion: stackpuddle/v1\nkind: Project\nmetadata:\n  name: " + name + "\nspec:\n")
	if o.SecretInput || o.PublicInput {
		yaml.WriteString("  inputs:\n")
	}
	if o.PublicInput {
		yaml.WriteString("    log-level:\n      sensitivity: public\n      value: info\n")
	}
	if o.SecretInput {
		yaml.WriteString("    db-password:\n      sensitivity: secret\n")
	}
	yaml.WriteString("  definitions:\n    kv-store:\n      path: definitions/kv-store\n")
	needs := o.RequireCapability
	if needs != "" {
		yaml.WriteString("    needs-pg:\n      path: definitions/needs-pg\n")
	}
	if len(instances) > 0 || needs != "" {
		yaml.WriteString("  modules:\n")
	}
	for _, name := range instances {
		fmt.Fprintf(&yaml, "    %s:\n      definition: kv-store\n      options:\n        note: %s\n", name, name)
	}
	if needs != "" {
		yaml.WriteString("    db:\n      definition: needs-pg\n")
	}
	apiArtifact := "      image: " + BusyboxImage + "\n"
	if o.APIImage != "" {
		apiArtifact = "      image: " + o.APIImage + "\n"
	}
	if o.BuildAPI {
		apiArtifact = "      build:\n        context: src\n"
	}
	yaml.WriteString("  workloads:\n    api:\n      type: service\n" + apiArtifact + "      command: " + command + "\n")
	yaml.WriteString("      environment:\n        MODE: standard\n        LITERAL: \"${SP_SECRET_1}\"\n")
	for _, k := range sortedKeys(o.APIEnv) {
		fmt.Fprintf(&yaml, "        %s: %q\n", k, o.APIEnv[k])
	}
	if o.APISecretInput {
		yaml.WriteString("        TOKEN:\n          input: db-password\n")
	}
	yaml.WriteString("      listeners:\n        http:\n          port: 8080\n          protocol: http\n")
	yaml.WriteString("      exports:\n        http:\n          capability: http.endpoint/1\n          listener: http\n")
	yaml.WriteString("      volumes:\n        uploads:\n          path: /data\n")
	if !o.NoJob {
		jobCommand := "[\"true\"]"
		if o.JobCommand != nil {
			jobCommand = "[" + strings.Join(quoteAll(o.JobCommand), ", ") + "]"
		}
		yaml.WriteString("      prerequisites:\n        - job: migrate\n    migrate:\n      type: job\n      image: " + BusyboxImage + "\n      command: " + jobCommand + "\n")
	}
	if o.ExtraWorkload {
		yaml.WriteString("    worker:\n      type: service\n      image: " + BusyboxImage + "\n      command: [sleep, \"3600\"]\n")
	}
	yaml.WriteString("  environments:\n    local:\n      runtime:\n        type: compose\n        context: " + o.DockerContext + "\n      secrets:\n        provider: local\n")
	if o.ExposePort != 0 {
		fmt.Fprintf(&yaml, "      exposures:\n        - source:\n            object: workload/api\n            export: http\n          port: %d\n", o.ExposePort)
	}
	files := map[string]string{
		"stackpuddle.yaml":                                 yaml.String(),
		"definitions/kv-store/module.yaml":                 kvModule(o.StoreImage),
		"definitions/kv-store/content/options.schema.json": kvSchema,
	}
	if needs != "" {
		files["definitions/needs-pg/module.yaml"] = needsModule(needs)
	}
	if o.BuildAPI {
		files["src/Dockerfile"] = "FROM " + BusyboxImage + "\nCMD [\"sleep\", \"3600\"]\n"
	}
	return files
}

// needsModule is a project-local definition that requires a capability this engine does not
// advertise.
func needsModule(capability string) string {
	return `apiVersion: stackpuddle/v1
kind: Module
name: needs-pg
version: 0.1.0
requires:
  capabilities: [` + capability + `, runtime.compose/1]
workloads:
  server:
    type: service
    image: ` + BusyboxImage + `
    command: [sleep, "3600"]
`
}

func quoteAll(items []string) []string {
	out := make([]string, len(items))
	for i, item := range items {
		out[i] = fmt.Sprintf("%q", item)
	}
	return out
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	for i := range keys {
		for j := i + 1; j < len(keys); j++ {
			if keys[j] < keys[i] {
				keys[i], keys[j] = keys[j], keys[i]
			}
		}
	}
	return keys
}

const kvSchema = `{
  "$schema": "https://json-schema.org/draft/2020-12/schema",
  "$id": "urn:stackpuddle:module:kv-store:configuration",
  "type": "object",
  "additionalProperties": false,
  "properties": { "note": { "type": "string" } }
}
`

func kvModule(image string) string {
	return `apiVersion: stackpuddle/v1
kind: Module
name: kv-store
version: 0.1.0
requires:
  capabilities: [runtime.compose/1]
configuration:
  schema: content/options.schema.json
workloads:
  store:
    type: service
    image: ` + image + `
    command: [sleep, "3600"]
    listeners:
      kv:
        port: 6379
        protocol: tcp
    exports:
      kv:
        capability: kv.endpoint/1
        listener: kv
    volumes:
      data:
        path: /var/lib/kv
`
}

// LoadProjectErr loads the project whose root file is stackpuddle.yaml in files.
func LoadProjectErr(files map[string]string) (model.Project, error) {
	fsys := fstest.MapFS{}
	for name, content := range files {
		fsys[name] = &fstest.MapFile{Data: []byte(content)}
	}
	project, diags := model.Load(fsys, "stackpuddle.yaml", nil)
	if len(diags) > 0 {
		return model.Project{}, fmt.Errorf("the sample project does not load: %v", diags)
	}
	return project, nil
}

// LoadProject is LoadProjectErr for a test.
func LoadProject(t testing.TB, files map[string]string) model.Project {
	t.Helper()
	project, err := LoadProjectErr(files)
	if err != nil {
		t.Fatal(err)
	}
	return project
}

// Shop loads the sample project.
func Shop(t testing.TB, o ShopOptions) model.Project { return LoadProject(t, ShopFiles(o)) }
