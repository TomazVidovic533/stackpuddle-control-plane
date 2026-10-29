//go:build integration

package modules_test

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"syscall"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/plan"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

func postgresFixture() fixture {
	return fixture{
		prepare: testkit.RequirePostgresImage,
		project: func(name string) (model.Project, error) {
			return testkit.PostgresProject(name, testkit.PostgresOptions)
		},
		instance: testkit.PostgresInstance,
		workloads: []plan.Workload{{
			Name: testkit.PostgresInstance + "--server", Type: plan.TypeService, Image: testkit.PostgresImage,
		}},
		options: json.RawMessage(testkit.PostgresOptions),
		absent:  module.ProbeResult{ExitCode: -1, Stderr: "target is not running"},
		denied:  module.ProbeResult{ExitCode: 2, Stderr: "ERROR:  42501"},
		unknown: module.ProbeResult{ExitCode: 2, Stderr: "FATAL:  the database system is starting up"},
		status:  func(o module.Observation) string { return o.Facts["status"] },

		wantAbsent: "absent", wantDenied: "denied", wantUnknown: "unknown",
		killAfterEffect: func(d module.Driver) module.Driver { return killAfterCreateRole{d} },
		snapshot: func(r *testkit.Rig) []string {
			server := r.ScopeFile().ComposeProject + "-" + testkit.PostgresInstance + "--server-1"
			out, err := testkit.DockerStdin(`SELECT oid || ':' || rolname || ':' || coalesce(rolpassword, '') FROM pg_authid WHERE rolname LIKE 'catalog\_%' ORDER BY rolname`,
				"exec", "-i", "-u", "postgres", server, "psql", "-X", "-v", "ON_ERROR_STOP=1", "-At", "-d", "postgres")
			if err != nil {
				return []string{"unreadable: " + err.Error()}
			}
			return testkit.Lines(out)
		},
		secretRefs: []string{"db/catalog-app-password", "db/catalog-ro-password", "db/catalog-migration-password"},
		export:     "sql",
		parameters: json.RawMessage(`{"user":"catalog_app","database":"catalog"}`),
	}
}

// killAfterCreateRole sends the process SIGKILL once the first user's CREATE ROLE probe has returned.
type killAfterCreateRole struct{ module.Driver }

func (k killAfterCreateRole) Apply(ctx context.Context, r module.ApplyRequest) (module.ApplyResponse, error) {
	if r.Step.Action == "postgres.role" && len(r.Results) > 0 && r.Results[0].ID == "mutate" && r.Results[0].ExitCode == 0 &&
		strings.Contains(string(r.Step.Params), `"login":true`) {
		_ = syscall.Kill(os.Getpid(), syscall.SIGKILL)
	}
	return k.Driver.Apply(ctx, r)
}
