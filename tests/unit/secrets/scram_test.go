package secrets_test

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"strings"
	"testing"

	"github.com/TomazVidovic533/stackpuddle-control-plane/internal/secrets"
)

// TestSCRAMSHA256_MatchesTheRFC7677Exchange checks the verifier against the published exchange for
// the password "pencil" with salt W22ZaJ0SNY7soEsUEjb6gQ== and 4096 iterations: the client proof must
// open to a client key whose hash is the verifier's StoredKey, and the server signature must be
// the HMAC of the verifier's ServerKey.
func TestSCRAMSHA256_MatchesTheRFC7677Exchange(t *testing.T) {
	t.Parallel()
	enc := base64.StdEncoding
	salt, err := enc.DecodeString("W22ZaJ0SNY7soEsUEjb6gQ==")
	if err != nil {
		t.Fatal(err)
	}
	verifier, err := secrets.SCRAMSHA256(secrets.NewValue([]byte("pencil")), bytes.NewReader(salt))
	if err != nil {
		t.Fatal(err)
	}
	prefix := "SCRAM-SHA-256$4096:W22ZaJ0SNY7soEsUEjb6gQ==$"
	text := string(verifier.Reveal())
	keys, ok := strings.CutPrefix(text, prefix)
	if !ok {
		t.Fatalf("verifier has the wrong shape: it does not start with %q", prefix)
	}
	storedText, serverText, _ := strings.Cut(keys, ":")
	stored, _ := enc.DecodeString(storedText)
	serverKey, _ := enc.DecodeString(serverText)

	authMessage := "n=user,r=rOprNGfwEbeRWgbNEkqO," +
		"r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0,s=W22ZaJ0SNY7soEsUEjb6gQ==,i=4096," +
		"c=biws,r=rOprNGfwEbeRWgbNEkqO%hvYDpWUa2RaTCAfuxFIlj)hNlF$k0"
	proof, _ := enc.DecodeString("dHzbZapWIk4jUhN+Ute9ytag9zjfMHgsqmmiz7AndVQ=")
	signature := mac(stored, authMessage)
	clientKey := make([]byte, len(proof))
	for i := range proof {
		clientKey[i] = proof[i] ^ signature[i]
	}
	if got := sha256.Sum256(clientKey); !bytes.Equal(got[:], stored) {
		t.Error("the client proof does not open to a key that hashes to the StoredKey")
	}
	wantServer, _ := enc.DecodeString("6rriTRBi23WpRR/wtup+mMhUZUn/dB5nLTJRsjl95G4=")
	if !bytes.Equal(mac(serverKey, authMessage), wantServer) {
		t.Error("the server signature differs from the RFC 7677 value")
	}
}

func mac(key []byte, message string) []byte {
	h := hmac.New(sha256.New, key)
	h.Write([]byte(message))
	return h.Sum(nil)
}

func TestSCRAMSHA256_APrintedVerifierIsRedactedAndTheSaltComesFromTheReader(t *testing.T) {
	t.Parallel()
	verifier, err := secrets.SCRAMSHA256(secrets.NewValue([]byte("p")), bytes.NewReader(make([]byte, 16)))
	if err != nil {
		t.Fatal(err)
	}
	if got := verifier.String(); got != "[redacted]" {
		t.Errorf("String() = %q", got)
	}
	if _, err := secrets.SCRAMSHA256(secrets.NewValue([]byte("p")), bytes.NewReader(nil)); err == nil {
		t.Error("a short random source did not fail")
	}
}
