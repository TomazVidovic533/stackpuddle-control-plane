// Package tls issues the local certificate authority of one environment and the server certificates
// it signs. It imports the standard library only: where the keys are stored and where the
// certificates are written belong to the caller.
//
// Keys are ECDSA P-256 and always come from crypto/rand, never from a seam the engine lets a test
// replace, so a deterministic reader cannot make a key predictable. Every certificate starts one hour
// before the issue time so a small clock difference between the control plane and a container does
// not reject it.
package tls

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"time"
)

// Validity and renewal windows.
const (
	// Backdate is how long before the issue time a certificate becomes valid.
	Backdate = time.Hour
	// CAValidity is how long a certificate authority certificate lives.
	CAValidity = 10 * 365 * 24 * time.Hour
	// ServerValidity is how long a server certificate lives.
	ServerValidity = 365 * 24 * time.Hour
	// RenewBefore is how long before expiry a server certificate is reissued.
	RenewBefore = 30 * 24 * time.Hour

	serialBits = 128
	pemKey     = "EC PRIVATE KEY"
	pemCert    = "CERTIFICATE"
	caName     = "StackPuddle local CA"
)

// ErrInvalidMaterial marks key or certificate bytes that are not what the caller said they are.
var ErrInvalidMaterial = errors.New("tls material is invalid")

// GenerateKey returns a new ECDSA P-256 private key as PEM.
func GenerateKey() ([]byte, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, fmt.Errorf("tls: generate key: %w", err)
	}
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, fmt.Errorf("tls: encode key: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemKey, Bytes: der}), nil
}

// ParseKey decodes a private key produced by GenerateKey.
func ParseKey(keyPEM []byte) (*ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(keyPEM)
	if block == nil || block.Type != pemKey {
		return nil, fmt.Errorf("%w: not an EC private key", ErrInvalidMaterial)
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMaterial, err)
	}
	return key, nil
}

// ParseCertificate decodes one certificate from PEM.
func ParseCertificate(certPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil || block.Type != pemCert {
		return nil, fmt.Errorf("%w: not a certificate", ErrInvalidMaterial)
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrInvalidMaterial, err)
	}
	return cert, nil
}

func serial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), serialBits)
	n, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("tls: serial number: %w", err)
	}
	return n, nil
}

// IssueCA returns the self-signed certificate authority certificate for key, valid from one hour
// before now for ten years.
func IssueCA(keyPEM []byte, now time.Time) ([]byte, error) {
	key, err := ParseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	number, err := serial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber:          number,
		Subject:               pkix.Name{CommonName: caName},
		NotBefore:             now.UTC().Add(-Backdate),
		NotAfter:              now.UTC().Add(CAValidity),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, fmt.Errorf("tls: sign the authority certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCert, Bytes: der}), nil
}

// Authority is a certificate authority: its key and its certificate, both PEM.
type Authority struct {
	KeyPEM  []byte
	CertPEM []byte
}

// IssueServer returns a server certificate for key signed by ca. names are the subject alternative
// names: an entry that parses as an IP address becomes an IP name, any other a DNS name. At least
// one name is required.
func IssueServer(ca Authority, keyPEM []byte, names []string, now time.Time) ([]byte, error) {
	if len(names) == 0 {
		return nil, fmt.Errorf("%w: a server certificate needs at least one name", ErrInvalidMaterial)
	}
	caKey, err := ParseKey(ca.KeyPEM)
	if err != nil {
		return nil, err
	}
	caCert, err := ParseCertificate(ca.CertPEM)
	if err != nil {
		return nil, err
	}
	key, err := ParseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	number, err := serial()
	if err != nil {
		return nil, err
	}
	template := &x509.Certificate{
		SerialNumber: number,
		Subject:      pkix.Name{CommonName: names[0]},
		NotBefore:    now.UTC().Add(-Backdate),
		NotAfter:     now.UTC().Add(ServerValidity),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	for _, name := range names {
		if ip := net.ParseIP(name); ip != nil {
			template.IPAddresses = append(template.IPAddresses, ip)
			continue
		}
		template.DNSNames = append(template.DNSNames, name)
	}
	der, err := x509.CreateCertificate(rand.Reader, template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, fmt.Errorf("tls: sign the server certificate: %w", err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: pemCert, Bytes: der}), nil
}
