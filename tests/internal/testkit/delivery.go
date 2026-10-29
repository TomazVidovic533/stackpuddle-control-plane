package testkit

import (
	"fmt"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle/model"
)

// BoundOptions vary the delivery project: a store module with an sql export, a migration job and an
// app service that bind it, and optionally a plain provider with a consumer.
type BoundOptions struct {
	// MigrateUser and RuntimeUser are the users the job and the app bind; "" means migrator and app.
	MigrateUser, RuntimeUser string
	// MigrateCommand replaces the job's command; nil keeps "true".
	MigrateCommand []string
	// PasswordInEnvironment maps the secret password value to an environment key of the app.
	PasswordInEnvironment bool
	// Expose publishes the sql export on the host.
	Expose bool
	// Plain adds a plain cache provider and a reader that binds its host and port to an environment
	// key and to a file.
	Plain bool
	// PlainSecretValue makes the plain provider declare a secret value and the reader file it.
	PlainSecretValue bool
	// NoApp leaves out the app service and its binding.
	NoApp bool
}

// BoundFiles returns the files of the delivery project.
func BoundFiles(o BoundOptions) map[string]string {
	if o.MigrateUser == "" {
		o.MigrateUser = "migrator"
	}
	if o.RuntimeUser == "" {
		o.RuntimeUser = "app"
	}
	command := "[\"true\"]"
	if o.MigrateCommand != nil {
		command = "[" + strings.Join(quoteAll(o.MigrateCommand), ", ") + "]"
	}
	var y strings.Builder
	y.WriteString(`apiVersion: stackpuddle/v1
kind: Project
metadata:
  name: shop
spec:
  definitions:
    bound-store:
      path: definitions/bound-store
  modules:
    db:
      definition: bound-store
  workloads:
    migrate:
      type: job
      image: ` + BusyboxImage + `
      command: ` + command + `
      dependencies:
        database:
          capability: sql.connection/1
`)
	if !o.NoApp {
		y.WriteString(`    app:
      type: service
      image: ` + BusyboxImage + `
      command: [sleep, "3600"]
      dependencies:
        database:
          capability: sql.connection/1
      prerequisites:
        - job: migrate
`)
	}
	if o.Plain {
		y.WriteString(`    cache:
      type: service
      image: ` + BusyboxImage + `
      command: [sleep, "3600"]
      listeners:
        tcp:
          port: 6379
          protocol: tcp
      exports:
        tcp:
          capability: tcp.endpoint/1
          listener: tcp
`)
		if o.PlainSecretValue {
			y.WriteString("          values:\n            token:\n              sensitivity: secret\n")
		}
		y.WriteString(`    reader:
      type: service
      image: ` + BusyboxImage + `
      command: [sleep, "3600"]
      dependencies:
        cache:
          capability: tcp.endpoint/1
`)
	}
	y.WriteString("  bindings:\n")
	y.WriteString(binding("migrate", o.MigrateUser, false, false))
	if !o.NoApp {
		y.WriteString(binding("app", o.RuntimeUser, o.PasswordInEnvironment, true))
	}
	if o.Plain {
		files := ""
		if o.PlainSecretValue {
			files = "          token:\n            path: /run/sp/token\n            mode: \"0400\"\n"
		}
		y.WriteString(`    - consumer: workload/reader
      dependency: cache
      source:
        object: workload/cache
        export: tcp
      delivery:
        environment:
          CACHE_HOST: host
          CACHE_PORT: port
        files:
          host:
            path: /run/sp/cache-host
            mode: "0444"
` + files)
	}
	y.WriteString(`  environments:
    local:
      runtime:
        type: compose
        context: default
      secrets:
        provider: local
`)
	if o.Expose {
		y.WriteString("      exposures:\n        - source:\n            object: module/db\n            export: sql\n          port: 15432\n")
	}
	return map[string]string{
		"stackpuddle.yaml":                                    y.String(),
		"definitions/bound-store/module.yaml":                 boundModule(),
		"definitions/bound-store/content/options.schema.json": kvSchema,
	}
}

func binding(consumer, user string, passwordInEnvironment, withAppExtras bool) string {
	env := "          DB_HOST: host\n          DB_PORT: port\n          DB_NAME: database\n          DB_USER: username\n"
	if passwordInEnvironment {
		env += "          DB_PASSWORD: password\n"
	}
	return `    - consumer: workload/` + consumer + `
      dependency: database
      source:
        object: module/db
        export: sql
      parameters:
        user: ` + user + `
      delivery:
        environment:
` + env + `        files:
          password:
            path: /run/sp/password
            mode: "0400"
            owner: "999:999"
          ca:
            path: /run/sp/ca.crt
            mode: "0444"
`
}

func boundModule() string {
	return `apiVersion: stackpuddle/v1
kind: Module
name: bound-store
version: 0.1.0
requires:
  capabilities: [runtime.compose/1]
configuration:
  schema: content/options.schema.json
workloads:
  store:
    type: service
    image: ` + BusyboxImage + `
    command: [sleep, "3600"]
    listeners:
      sql:
        port: 5432
        protocol: tcp
    exports:
      sql:
        capability: sql.connection/1
        listener: sql
        values:
          database:
            sensitivity: public
          username:
            sensitivity: public
          password:
            sensitivity: secret
          ca:
            sensitivity: public
    volumes:
      data:
        path: /var/lib/store
`
}

// Bound loads the delivery project.
func Bound(t testing.TB, o BoundOptions) model.Project { return LoadProject(t, BoundFiles(o)) }

// BoundProject loads the delivery project as a project named name.
func BoundProject(name string, o BoundOptions) (model.Project, error) {
	files := BoundFiles(o)
	const placeholder = "metadata:\n  name: shop\n"
	if strings.Count(files["stackpuddle.yaml"], placeholder) != 1 {
		return model.Project{}, fmt.Errorf("testkit: the delivery project lost its name line")
	}
	files["stackpuddle.yaml"] = strings.Replace(files["stackpuddle.yaml"], placeholder, "metadata:\n  name: "+name+"\n", 1)
	return LoadProjectErr(files)
}
