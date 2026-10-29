//go:build integration

package delivery_test

import (
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/drivers"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// TestDelivery_TheSamePathDeliversAFileAndAnEnvironmentKeyToAConsumerThatIsNotPostgreSQL uses a
// busybox store module and a plain busybox provider: the engine's delivery names no product.
func TestDelivery_TheSamePathDeliversAFileAndAnEnvironmentKeyToAConsumerThatIsNotPostgreSQL(t *testing.T) {
	s := testkit.NewRig(t, namePrefix, func(name string) (model.Project, error) {
		return testkit.BoundProject(name, testkit.BoundOptions{Plain: true})
	}, drivers.Bound{})
	s.MustUp()
	project := s.ScopeFile().ComposeProject
	reader, app, store := project+"-reader-1", project+"-app-1", project+"-db--store-1"

	if got := execIn(t, reader, "stat", "-c", "%a %u:%g", "/run/sp/cache-host"); got != "444 0:0" {
		t.Errorf("cache-host = %s, want mode 444 and the default owner", got)
	}
	if got := execIn(t, reader, "cat", "/run/sp/cache-host"); got != "cache" {
		t.Errorf("cache-host holds %q, want the provider's service name", got)
	}
	env := containerEnv(t, reader)
	if env["CACHE_HOST"] != "cache" || env["CACHE_PORT"] != "6379" {
		t.Errorf("environment = host %q port %q", env["CACHE_HOST"], env["CACHE_PORT"])
	}
	if got := execIn(t, app, "stat", "-c", "%a %u:%g %n", "/run/sp/password", "/run/sp/ca.crt"); got != "400 999:999 /run/sp/password\n444 0:0 /run/sp/ca.crt" {
		t.Errorf("app files = %s", got)
	}
	if got := execIn(t, app, "cat", "/run/sp/password"); got != s.Secret("db/app-password") {
		t.Error("the app's password file is not its own secret")
	}
	if got := execIn(t, store, "stat", "-c", "%a %u:%g %n", "/run/stackpuddle/tls/server.crt", "/run/stackpuddle/tls/server.key"); !strings.HasPrefix(got, "444 999:999") || !strings.Contains(got, "\n600 999:999") {
		t.Errorf("server files = %s", got)
	}
	if _, ok := containerEnv(t, app)["DB_PASSWORD"]; ok {
		t.Error("the password is an environment key")
	}
	absentFromHost(t, s.Root, s.Secret("db/app-password"), s.Secret("db/migrator-password"), s.Secret("tls/server/db--store"), s.Secret("tls/ca-key"))
}
