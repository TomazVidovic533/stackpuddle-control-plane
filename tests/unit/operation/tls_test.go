package operation_test

import (
	"crypto/ecdsa"
	"crypto/x509"
	"slices"
	"testing"
	"time"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/state"
	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/tls"
	"github.com/TomazVidovic533/stackpuddle-control-plane/tests/internal/testkit"
)

func (h *harness) certificate(name string) *x509.Certificate {
	h.t.Helper()
	cert, err := tls.ParseCertificate(h.fileBytes("tls/" + name))
	if err != nil {
		h.t.Fatalf("tls/%s: %v", name, err)
	}
	return cert
}

func (h *harness) serverNames() []string {
	h.t.Helper()
	cert := h.certificate("db--store.crt")
	names := slices.Clone(cert.DNSNames)
	for _, ip := range cert.IPAddresses {
		names = append(names, ip.String())
	}
	slices.Sort(names)
	return names
}

func (h *harness) storedKeyVersions(ref string) int {
	h.t.Helper()
	parsed, err := secrets.ParseRef(ref)
	if err != nil {
		h.t.Fatal(err)
	}
	entry, _ := h.store().Entry(parsed)
	return len(entry.Versions)
}

func TestTLS_UpIssuesACAAndAServerCertificateWhoseKeysLiveOnlyInTheSealedStore(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatalf("up: %v", err)
	}
	ca, server := h.certificate("ca.crt"), h.certificate("db--store.crt")
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: "db--store", CurrentTime: testkit.Epoch.Add(time.Minute)}); err != nil {
		t.Errorf("the server certificate does not verify for the service name: %v", err)
	}
	if _, err := server.Verify(x509.VerifyOptions{Roots: pool, DNSName: "other", CurrentTime: testkit.Epoch.Add(time.Minute)}); err == nil {
		t.Error("the server certificate verifies for a name it does not carry")
	}
	for name, cert := range map[string]*x509.Certificate{"CA": ca, "server": server} {
		// The injected clock starts at the epoch and moves one second per reading.
		if early, late := testkit.Epoch.Add(-time.Hour), testkit.Epoch.Add(-time.Hour+time.Minute); cert.NotBefore.Before(early) || cert.NotBefore.After(late) {
			t.Errorf("%s certificate NotBefore = %v, want one hour before the issue time", name, cert.NotBefore)
		}
	}
	if got := h.serverNames(); !slices.Equal(got, []string{"db--store"}) {
		t.Errorf("names = %v: without host access only the service name belongs in the certificate", got)
	}
	for ref, cert := range map[string]*x509.Certificate{"tls/ca-key": ca, "tls/server/db--store": server} {
		key, err := tls.ParseKey([]byte(h.secretValue(ref)))
		if err != nil {
			t.Fatalf("stored key %s: %v", ref, err)
		}
		if pub, ok := cert.PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
			t.Errorf("the certificate for %s was not issued for the stored key", ref)
		}
		if got := h.state().Accepted.Credentials[ref].Origin; got != state.OriginGenerated {
			t.Errorf("%s origin = %v", ref, got)
		}
	}
}

func TestTLS_HostNamesAppearOnlyWhenHostAccessIsDeclaredAndAChangeReissuesAndRestartsTheServer(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatal(err)
	}
	caBefore, certBefore := h.fileBytes("tls/ca.crt"), h.fileBytes("tls/db--store.crt")
	keyVersion := h.state().Accepted.Credentials["tls/server/db--store"].Version
	hashBefore := h.state().Realizations["service/db--store"].SpecHash
	applies := h.rt.CallCount("apply plan.StartService db--store")

	result, err := h.upBound(testkit.BoundOptions{Expose: true})
	if err != nil {
		t.Fatalf("up with host access: %v", err)
	}
	if got := h.serverNames(); !slices.Equal(got, []string{"127.0.0.1", "db--store", "localhost"}) {
		t.Errorf("names = %v, want the service name plus the host names", got)
	}
	if string(h.fileBytes("tls/ca.crt")) != string(caBefore) {
		t.Error("a SAN change reissued the CA")
	}
	if string(h.fileBytes("tls/db--store.crt")) == string(certBefore) {
		t.Error("the server certificate was not reissued")
	}
	if h.state().Accepted.Credentials["tls/server/db--store"].Version != keyVersion || h.storedKeyVersions("tls/server/db--store") != 1 {
		t.Error("the reissued certificate did not keep its stored key")
	}
	if got := steps(*result.Operation); !slices.Contains(got, "tls.issue tls/db--store") || slices.Contains(got, "tls.issue tls/ca") {
		t.Errorf("steps = %v: only the server certificate is issued", got)
	}
	if h.rt.CallCount("apply plan.StartService db--store") != applies+1 || h.state().Realizations["service/db--store"].SpecHash == hashBefore {
		t.Error("the reissued certificate did not restart the server through its spec hash")
	}

	again, err := h.upBound(testkit.BoundOptions{Expose: true})
	if err != nil || again.Operation != nil {
		t.Errorf("an unchanged certificate must restart nothing: %v, %v", again.Operation, err)
	}
}

func TestTLS_AMissingCertificateIsIssuedAgainFromTheStoredKeyEvenWhenTheCredentialWasNeverRecorded(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatal(err)
	}
	// A process killed between the store write and the certificate write leaves the key in the store,
	// no credential in state, and no certificate file.
	st := h.state()
	delete(st.Accepted.Credentials, "tls/server/db--store")
	if _, err := h.open().Commit(st.Generation, st); err != nil {
		t.Fatal(err)
	}
	removeFile(t, h, "tls/db--store.crt")

	if _, err := h.upBound(testkit.BoundOptions{}); err != nil {
		t.Fatalf("up after the interrupted issue: %v", err)
	}
	if n := h.storedKeyVersions("tls/server/db--store"); n != 1 {
		t.Errorf("the store holds %d versions of the server key, want the one that was already there", n)
	}
	key, err := tls.ParseKey([]byte(h.secretValue("tls/server/db--store")))
	if err != nil {
		t.Fatal(err)
	}
	if pub, ok := h.certificate("db--store.crt").PublicKey.(*ecdsa.PublicKey); !ok || !pub.Equal(&key.PublicKey) {
		t.Error("the new certificate was not issued for the stored key")
	}
}

func TestTLS_ADryRunPlansTheCertificatesAndWritesNone(t *testing.T) {
	t.Parallel()
	h := newBoundHarness(t)
	req := h.boundRequest(testkit.BoundOptions{}, state.CommandUp)
	req.DryRun = true
	result, err := h.eng.Submit(t.Context(), req)
	if err != nil || result.Preview == nil {
		t.Fatalf("dry run = %+v, %v", result, err)
	}
	var issues []string
	for _, row := range result.Preview.Actions {
		if row.Action == "tls.issue" {
			issues = append(issues, row.Object)
		}
	}
	if !slices.Equal(issues, []string{"tls/ca", "tls/db--store"}) {
		t.Errorf("preview certificates = %v", issues)
	}
	if len(h.rt.Calls) > 2 {
		t.Errorf("a dry run called the runtime: %v", h.rt.Calls)
	}
}
