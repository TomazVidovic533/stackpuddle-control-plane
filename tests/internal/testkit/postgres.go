package testkit

import (
	"fmt"
	"testing"
	"testing/fstest"

	"github.com/TomazVidovic533/stackpuddle/model"
)

// PostgresImage is the postgres module's pinned image; the tests require it present by this reference.
const PostgresImage = "postgres:18.6-bookworm@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af"

// postgresMirrorImage is the reference pulled through the mirror when PostgresImage is absent.
const postgresMirrorImage = "mirror.gcr.io/library/postgres@sha256:1c59e2c3c818eaa0f0628f695b36e7c9e362d6b219b36a54a32df645cbd7e1af"

// PostgresInstance is the key of the module instance in the sample project.
const PostgresInstance = "db"

// PostgresOptions declares one database with a migration user, two runtime users and two permission
// roles. It is JSON, which is YAML.
const PostgresOptions = `{
  "users": {
    "catalog_app": {"purpose": "runtime", "passwordSecretRef": "catalog-app-password", "memberOf": ["catalog_writer"]},
    "catalog_ro": {"purpose": "runtime", "passwordSecretRef": "catalog-ro-password", "memberOf": ["catalog_reader"]},
    "catalog_migrator": {"purpose": "migrations", "passwordSecretRef": "catalog-migration-password"}
  },
  "roles": {"catalog_writer": {"login": false}, "catalog_reader": {"login": false}},
  "databases": {
    "catalog": {
      "owner": "catalog_migrator", "connectRoles": ["catalog_writer", "catalog_reader"], "connectionLimit": 25,
      "settings": {"statementTimeoutMs": 30000},
      "schemas": {"public": {"grants": {
        "catalog_writer": {"tables": ["SELECT", "INSERT", "UPDATE", "DELETE"], "sequences": ["USAGE", "SELECT"]},
        "catalog_reader": {"tables": ["SELECT"]}
      }}}
    }
  }
}`

// RequirePostgresImage makes the module's pinned image present: it pulls through the mirror when
// needed and then requires the module's own reference, failing by name otherwise (D67).
func RequirePostgresImage(t testing.TB) {
	t.Helper()
	if _, err := DockerErr("image", "inspect", PostgresImage); err == nil {
		return
	}
	if out, err := DockerErr("pull", "--quiet", postgresMirrorImage); err != nil {
		t.Fatalf("RequirePostgresImage: pull %s: %v\n%s", postgresMirrorImage, err, out)
	}
	if out, err := DockerErr("image", "inspect", PostgresImage); err != nil {
		t.Fatalf("RequirePostgresImage: the module's pinned image %s is not present after pulling %s: %v\n%s", PostgresImage, postgresMirrorImage, err, out)
	}
}

// PostgresProject loads a project named name with one postgres instance, PostgresInstance, whose
// definition is the packaged module built from the sibling tree.
func PostgresProject(name, options string) (model.Project, error) {
	definition, _, err := BuildPackagedModule("postgres")
	if err != nil {
		return model.Project{}, err
	}
	root := fmt.Sprintf(`apiVersion: stackpuddle/v1
kind: Project
metadata:
  name: %s
spec:
  modules:
    %s:
      definition: postgres
      options: %s
  environments:
    local:
      runtime:
        type: compose
        context: default
      secrets:
        provider: local
`, name, PostgresInstance, options)
	fsys := fstest.MapFS{"stackpuddle.yaml": &fstest.MapFile{Data: []byte(root)}}
	loaded, diagnostics := model.Load(fsys, "stackpuddle.yaml", []model.ModuleDefinition{definition})
	if len(diagnostics) > 0 {
		return model.Project{}, fmt.Errorf("the sample project does not load: %v", diagnostics)
	}
	return loaded, nil
}
