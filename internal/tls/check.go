package tls

import (
	"crypto/x509"
	"net"
	"slices"
	"time"
)

// Decision says what to do with a certificate that is already on disk.
type Decision int

// The decisions.
const (
	// Keep leaves the certificate as it is.
	Keep Decision = iota + 1
	// Reissue replaces it.
	Reissue
)

// String returns "keep" or "reissue".
func (d Decision) String() string {
	switch d {
	case Keep:
		return "keep"
	case Reissue:
		return "reissue"
	}
	return "unknown"
}

// Check decides whether a server certificate stays. It is reissued when it is missing or unreadable,
// when its names differ from the wanted set, when it was not signed by ca, or when fewer than
// RenewBefore remain. The clock arrives as a value so the decision is the same in every test.
func Check(certPEM, caPEM []byte, names []string, now time.Time) Decision {
	cert, err := ParseCertificate(certPEM)
	if err != nil {
		return Reissue
	}
	if !sameNames(cert, names) || cert.NotAfter.Sub(now) < RenewBefore {
		return Reissue
	}
	authority, err := ParseCertificate(caPEM)
	if err != nil {
		return Reissue
	}
	pool := x509.NewCertPool()
	pool.AddCert(authority)
	if _, err := cert.Verify(x509.VerifyOptions{Roots: pool, CurrentTime: now, KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}); err != nil {
		return Reissue
	}
	return Keep
}

// CheckCA decides whether the authority certificate stays: it is reissued when it is missing,
// unreadable, not a certificate authority, or has fewer than RenewBefore left.
func CheckCA(certPEM []byte, now time.Time) Decision {
	cert, err := ParseCertificate(certPEM)
	if err != nil || !cert.IsCA || cert.NotAfter.Sub(now) < RenewBefore {
		return Reissue
	}
	return Keep
}

// sameNames reports whether the certificate carries exactly the wanted names, DNS and IP alike.
func sameNames(cert *x509.Certificate, names []string) bool {
	have := slices.Clone(cert.DNSNames)
	for _, ip := range cert.IPAddresses {
		have = append(have, ip.String())
	}
	want := make([]string, 0, len(names))
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			name = ip.String()
		}
		want = append(want, name)
	}
	slices.Sort(have)
	slices.Sort(want)
	return slices.Equal(slices.Compact(have), slices.Compact(want))
}
