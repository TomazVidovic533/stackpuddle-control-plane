package testkit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/module"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/modules/postgres"
	"github.com/TomazVidovic533/stackpuddle/model"
)

// ConsumerStack is a Rig over the shared consumer fixture: a postgres instance "db", a migration job
// and an app, in one disposable Compose project. It reads the fixture from the sibling tree and fails
// by name when the tree, the fixture or the module image is missing.
type ConsumerStack struct {
	*Rig
}

// NewConsumerStack builds the stack. prefix starts the project name, environment is the fixture's
// environment to act on ("" means local) and edits change the fixture's text before it loads.
func NewConsumerStack(t *testing.T, prefix, environment string, edits ...Edit) *ConsumerStack {
	t.Helper()
	RequirePostgresImage(t)
	root, err := ConsumerFixtureRoot()
	if err != nil {
		t.Fatal(err)
	}
	_, record, err := BuildPackagedModule("postgres")
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("consumer fixture read from %s; %s", root, record)
	rig := NewRig(t, prefix, func(name string) (model.Project, error) { return ConsumerProject(name, edits...) }, postgres.Driver{})
	rig.Environment = environment
	return &ConsumerStack{Rig: rig}
}

// With replaces the engine with one over the catalog.
func (s *ConsumerStack) With(catalog ...module.Driver) { s.Rig.Rebuild(catalog...) }

// Server is the name of the database server's container.
func (s *ConsumerStack) Server() string {
	return s.ScopeFile().ComposeProject + "-db--server-1"
}

// Container is the name of the single container of a service, by Compose's naming.
func (s *ConsumerStack) Container(service string) string {
	return s.ScopeFile().ComposeProject + "-" + service + "-1"
}

// Network is the scope's Compose network.
func (s *ConsumerStack) Network() string { return s.ScopeFile().ComposeProject + "_default" }

// CA returns the PEM of the environment's CA certificate, a public file of the environment folder.
func (s *ConsumerStack) CA() []byte {
	s.T.Helper()
	data, err := os.ReadFile(filepath.Join(s.EnvDir(), "tls", "ca.crt"))
	if err != nil {
		s.T.Fatal(err)
	}
	return data
}

// Connect returns an attempt as user with the password the sealed store holds under ref.
func (s *ConsumerStack) Connect(user, ref, host, sslmode string) Connection {
	s.T.Helper()
	return Connection{
		Name: s.Name, Network: s.Network(), Host: host, User: user, Database: "app", Password: s.Secret(ref),
		SSLMode: sslmode, CA: s.CA(),
	}
}

// Admin runs SQL in the server as postgres over the in-container socket and returns its trimmed output.
func (s *ConsumerStack) Admin(database, sql string) string {
	s.T.Helper()
	out, err := DockerStdin(sql, "exec", "-i", "-u", "postgres", s.Server(), "psql", "-X", "-v", "ON_ERROR_STOP=1", "-At", "-d", database)
	if err != nil {
		s.T.Fatalf("admin sql %q: %v\n%s", sql, err, out)
	}
	return out
}

// Services lists the names of the Compose services that have a container in this scope, running or not.
func (s *ConsumerStack) Services() []string {
	s.T.Helper()
	out := Docker(s.T, "ps", "-a", "--filter", "label=io.stackpuddle.scope="+s.ScopeFile().ScopeUID.String(), "--format", `{{.Label "com.docker.compose.service"}}`)
	return Lines(strings.TrimSpace(out))
}
