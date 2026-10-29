//go:build integration

// Package tls_test proves, against a real PostgreSQL server in a disposable Compose project, that
// connections are encrypted and verified and that every other attempt is rejected as an error result.
// The module archive and the consumer fixture are read from the sibling stackpuddle tree, so these
// tests are developer coverage, narrower than the published artifact path. Neither password
// authentication nor pg_isready is TLS evidence here: the evidence is the server's own report of the
// session (pg_stat_ssl) and the rejection of every attempt that is not encrypted, verified and
// addressed to a name the certificate carries.
package tls_test

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/tls"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

const (
	namePrefix  = "sp-m01-060-"
	runtimeUser = "app_runtime"
	runtimeRef  = "db/app-runtime-password"
)

// serverCertificate reads the certificate the server container was delivered.
func serverCertificate(t *testing.T, s *testkit.ConsumerStack) []byte {
	t.Helper()
	out, err := testkit.DockerErr("exec", s.Server(), "cat", "/run/stackpuddle/tls/server.crt")
	if err != nil {
		t.Fatalf("read the delivered certificate: %v\n%s", err, out)
	}
	return []byte(out)
}

// loopback connects from inside the server container to its own loopback address, the one name the
// certificate carries only when the environment declares host access.
func loopback(ctx context.Context, s *testkit.ConsumerStack, host string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", "exec", "-i", "-e", "PGPASSWORD", "-e", "PGSSLMODE=verify-full", "-e", "PGSSLROOTCERT=/tmp/ca.crt", s.Server(),
		"sh", "-c", `cat > /tmp/ca.crt; exec psql -X -v ON_ERROR_STOP=1 -At -h "$1" -U app_runtime -d app -c "SELECT ssl::text FROM pg_stat_ssl WHERE pid = pg_backend_pid()"`, "sh", host)
	cmd.Env = append(cmd.Environ(), "PGPASSWORD="+s.Secret(runtimeRef))
	cmd.Stdin = bytes.NewReader(s.CA())
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

func TestTLS_AnEncryptedVerifiedConnectionSucceedsAndEveryOtherAttemptIsRejected(t *testing.T) {
	s := testkit.NewConsumerStack(t, namePrefix, "")
	s.MustUp()
	ctx := t.Context()

	t.Run("verify-full to the service name succeeds and the server reports an encrypted session", func(t *testing.T) {
		out, err := s.Connect(runtimeUser, runtimeRef, "db--server", "verify-full").Run(ctx, "SELECT ssl::text || ':' || coalesce(version, '') FROM pg_stat_ssl WHERE pid = pg_backend_pid()")
		if err != nil || !strings.HasPrefix(out, "true:TLS") {
			t.Fatalf("session = %q, %v: want an encrypted TLS session", out, err)
		}
	})

	rejected := func(t *testing.T, c testkit.Connection, want ...string) {
		t.Helper()
		out, err := c.Run(ctx, "SELECT 1")
		if err == nil {
			t.Fatalf("the attempt succeeded: %q", out)
		}
		if errors.Is(err, testkit.ErrProbeTimedOut) {
			t.Fatalf("the attempt hung instead of ending in a result: %v", err)
		}
		for _, w := range want {
			if !strings.Contains(out, w) {
				t.Errorf("the rejection %q does not say %q", out, w)
			}
		}
	}

	t.Run("a plaintext attempt is rejected by the hostnossl rule", func(t *testing.T) {
		rejected(t, s.Connect(runtimeUser, runtimeRef, "db--server", "disable"), "pg_hba.conf rejects connection", "no encryption")
	})
	t.Run("a plaintext attempt is rejected for the migration identity too", func(t *testing.T) {
		rejected(t, s.Connect("app_migrator", "db/app-migrator-password", "db--server", "disable"), "pg_hba.conf rejects connection")
	})
	t.Run("a name the certificate does not carry is rejected under verify-full", func(t *testing.T) {
		ip := strings.TrimSpace(testkit.Docker(t, "inspect", "-f", `{{(index .NetworkSettings.Networks "`+s.Network()+`").IPAddress}}`, s.Server()))
		if ip == "" {
			t.Fatal("the server container has no address on the scope network")
		}
		rejected(t, s.Connect(runtimeUser, runtimeRef, ip, "verify-full"), "does not match host name")
	})
	t.Run("a certificate authority the environment did not issue is rejected", func(t *testing.T) {
		key, err := tls.GenerateKey()
		if err != nil {
			t.Fatal(err)
		}
		other, err := tls.IssueCA(key, testkit.Epoch)
		if err != nil {
			t.Fatal(err)
		}
		c := s.Connect(runtimeUser, runtimeRef, "db--server", "verify-full")
		c.CA = other
		rejected(t, c, "certificate verify failed")
	})
	t.Run("the loopback name is not in the certificate when the environment declares no host access", func(t *testing.T) {
		out, err := loopback(ctx, s, "127.0.0.1")
		if err == nil || !strings.Contains(out, "does not match host name") {
			t.Fatalf("verify-full to 127.0.0.1 = %q, %v: want a host name mismatch", out, err)
		}
		cert, err := tls.ParseCertificate(serverCertificate(t, s))
		if err != nil {
			t.Fatal(err)
		}
		if !slices.Equal(cert.DNSNames, []string{"db--server"}) || len(cert.IPAddresses) != 0 {
			t.Errorf("certificate names = DNS %v IP %v, want only the service name", cert.DNSNames, cert.IPAddresses)
		}
	})
	t.Run("the certificate and the key arrive with the declared owner and mode", func(t *testing.T) {
		got := testkit.Docker(t, "exec", s.Server(), "stat", "-c", "%a %u:%g %n", "/run/stackpuddle/tls/server.crt", "/run/stackpuddle/tls/server.key")
		want := "444 999:999 /run/stackpuddle/tls/server.crt\n600 999:999 /run/stackpuddle/tls/server.key"
		if got != want {
			t.Errorf("server files =\n%s\nwant\n%s", got, want)
		}
	})
}

func TestTLS_HostNamesAppearInTheCertificateOnlyWhenHostAccessIsDeclared(t *testing.T) {
	s := testkit.NewConsumerStack(t, namePrefix, "", testkit.ConsumerExposure())
	s.MustUp()
	cert, err := tls.ParseCertificate(serverCertificate(t, s))
	if err != nil {
		t.Fatal(err)
	}
	names := slices.Clone(cert.DNSNames)
	for _, ip := range cert.IPAddresses {
		names = append(names, ip.String())
	}
	slices.Sort(names)
	if !slices.Equal(names, []string{"127.0.0.1", "db--server", "localhost"}) {
		t.Errorf("certificate names = %v, want the service name, localhost and 127.0.0.1", names)
	}
	out, err := loopback(t.Context(), s, "127.0.0.1")
	if err != nil || out != "true" {
		t.Errorf("verify-full to 127.0.0.1 = %q, %v: want an encrypted, verified session", out, err)
	}
}
