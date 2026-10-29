package tls_test

import (
	"crypto/x509"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/tls"
)

var now = time.Date(2026, 10, 29, 10, 0, 0, 0, time.UTC)

type authority struct {
	tls.Authority
	cert *x509.Certificate
}

func newAuthority(t *testing.T, at time.Time) authority {
	t.Helper()
	key, err := tls.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.IssueCA(key, at)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := tls.ParseCertificate(cert)
	if err != nil {
		t.Fatal(err)
	}
	return authority{Authority: tls.Authority{KeyPEM: key, CertPEM: cert}, cert: parsed}
}

func issue(t *testing.T, ca authority, names []string, at time.Time) []byte {
	t.Helper()
	key, err := tls.GenerateKey()
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.IssueServer(ca.Authority, key, names, at)
	if err != nil {
		t.Fatal(err)
	}
	return cert
}

func TestIssue_EveryCertificateStartsOneHourBeforeTheInjectedClock(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t, now)
	server, err := tls.ParseCertificate(issue(t, ca, []string{"db--server"}, now))
	if err != nil {
		t.Fatal(err)
	}
	for name, cert := range map[string]*x509.Certificate{"CA": ca.cert, "server": server} {
		if want := now.Add(-time.Hour); !cert.NotBefore.Equal(want) {
			t.Errorf("%s NotBefore = %v, want %v", name, cert.NotBefore, want)
		}
	}
	if want := now.Add(tls.CAValidity); !ca.cert.NotAfter.Equal(want) || !ca.cert.IsCA {
		t.Errorf("CA NotAfter = %v IsCA = %v", ca.cert.NotAfter, ca.cert.IsCA)
	}
	if want := now.Add(tls.ServerValidity); !server.NotAfter.Equal(want) || server.IsCA {
		t.Errorf("server NotAfter = %v IsCA = %v", server.NotAfter, server.IsCA)
	}
}

func TestIssue_TheChainVerifiesForTheServiceNameAndNoOtherName(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t, now)
	server, err := tls.ParseCertificate(issue(t, ca, []string{"db--server"}, now))
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca.cert)
	at := now.Add(time.Minute)
	for _, tc := range []struct {
		name string
		dns  string
		ok   bool
	}{
		{"the service name", "db--server", true},
		{"a different service", "cache--server", false},
		{"localhost, which was not requested", "localhost", false},
	} {
		_, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: tc.dns, CurrentTime: at, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}})
		if (err == nil) != tc.ok {
			t.Errorf("%s: verify = %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
	other := newAuthority(t, now)
	otherPool := x509.NewCertPool()
	otherPool.AddCert(other.cert)
	if _, err := server.Verify(x509.VerifyOptions{Roots: otherPool, DNSName: "db--server", CurrentTime: at}); err == nil {
		t.Error("the certificate verifies against a CA that did not sign it")
	}
}

func TestIssue_NamesBecomeDNSAndIPAlternativesAndAtLeastOneIsRequired(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t, now)
	cert, err := tls.ParseCertificate(issue(t, ca, []string{"db--server", "localhost", "127.0.0.1"}, now))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cert.DNSNames, []string{"db--server", "localhost"}) || len(cert.IPAddresses) != 1 || cert.IPAddresses[0].String() != "127.0.0.1" {
		t.Errorf("DNS %v IP %v", cert.DNSNames, cert.IPAddresses)
	}
	key, _ := tls.GenerateKey()
	if _, err := tls.IssueServer(ca.Authority, key, nil, now); !errors.Is(err, tls.ErrInvalidMaterial) {
		t.Errorf("no names: %v, want ErrInvalidMaterial", err)
	}
}

func TestParse_RefusesWhatIsNotAKeyOrACertificate(t *testing.T) {
	t.Parallel()
	for _, in := range [][]byte{nil, []byte("not pem"), []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")} {
		if _, err := tls.ParseKey(in); !errors.Is(err, tls.ErrInvalidMaterial) {
			t.Errorf("ParseKey(%q) = %v", in, err)
		}
		if _, err := tls.ParseCertificate(in); !errors.Is(err, tls.ErrInvalidMaterial) {
			t.Errorf("ParseCertificate(%q) = %v", in, err)
		}
	}
	if _, err := tls.IssueCA([]byte("junk"), now); !errors.Is(err, tls.ErrInvalidMaterial) {
		t.Errorf("IssueCA with a bad key: %v", err)
	}
}

func TestCheck_KeepsAFreshMatchingCertificateAndReissuesOtherwise(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t, now)
	names := []string{"db--server"}
	cert := issue(t, ca, names, now)
	otherCA := newAuthority(t, now)
	tests := []struct {
		name  string
		cert  []byte
		ca    []byte
		names []string
		at    time.Time
		want  tls.Decision
	}{
		{"fresh and matching", cert, ca.CertPEM, names, now.Add(time.Hour), tls.Keep},
		{"names in another order", issue(t, ca, []string{"localhost", "db--server"}, now), ca.CertPEM, []string{"db--server", "localhost"}, now, tls.Keep},
		{"a name was added", cert, ca.CertPEM, []string{"db--server", "localhost", "127.0.0.1"}, now, tls.Reissue},
		{"a name was removed", issue(t, ca, []string{"db--server", "localhost"}, now), ca.CertPEM, names, now, tls.Reissue},
		{"31 days remain", cert, ca.CertPEM, names, now.Add(tls.ServerValidity - 31*24*time.Hour), tls.Keep},
		{"29 days remain", cert, ca.CertPEM, names, now.Add(tls.ServerValidity - 29*24*time.Hour), tls.Reissue},
		{"expired", cert, ca.CertPEM, names, now.Add(tls.ServerValidity + time.Hour), tls.Reissue},
		{"signed by another CA", cert, otherCA.CertPEM, names, now, tls.Reissue},
		{"missing", nil, ca.CertPEM, names, now, tls.Reissue},
		{"unreadable", []byte("junk"), ca.CertPEM, names, now, tls.Reissue},
		{"the CA is unreadable", cert, []byte("junk"), names, now, tls.Reissue},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := tls.Check(tc.cert, tc.ca, tc.names, tc.at); got != tc.want {
				t.Errorf("Check = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestCheckCA_KeepsAnAuthorityUntilItIsNearItsEnd(t *testing.T) {
	t.Parallel()
	ca := newAuthority(t, now)
	server := issue(t, ca, []string{"db--server"}, now)
	for _, tc := range []struct {
		name string
		cert []byte
		at   time.Time
		want tls.Decision
	}{
		{"fresh", ca.CertPEM, now, tls.Keep},
		{"near its end", ca.CertPEM, now.Add(tls.CAValidity - 29*24*time.Hour), tls.Reissue},
		{"missing", nil, now, tls.Reissue},
		{"not an authority", server, now, tls.Reissue},
	} {
		if got := tls.CheckCA(tc.cert, tc.at); got != tc.want {
			t.Errorf("%s: CheckCA = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestDecisionNamesItself(t *testing.T) {
	t.Parallel()
	if tls.Keep.String() != "keep" || tls.Reissue.String() != "reissue" || tls.Decision(0).String() != "unknown" {
		t.Errorf("names = %q %q %q", tls.Keep, tls.Reissue, tls.Decision(0))
	}
}

func TestGenerateKey_NeverRepeats(t *testing.T) {
	t.Parallel()
	a, _ := tls.GenerateKey()
	b, _ := tls.GenerateKey()
	if string(a) == string(b) {
		t.Error("two generated keys are equal")
	}
}
