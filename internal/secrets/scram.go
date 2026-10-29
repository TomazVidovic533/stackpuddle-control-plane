package secrets

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
)

// scramIterations is the PBKDF2 iteration count the RFC 7677 exchange and common servers default to.
const scramIterations = 4096

// scramSaltSize is the length of the random salt in bytes.
const scramSaltSize = 16

// SCRAMSHA256 returns the SCRAM-SHA-256 verifier of password, in the form a server stores for a role,
// "SCRAM-SHA-256$<iterations>:<salt>$<StoredKey>:<ServerKey>" (RFC 5802, RFC 7677). The
// salt is read from random. The verifier is as secret as the password it comes from, so it is a Value.
func SCRAMSHA256(password Value, random io.Reader) (Value, error) {
	salt := make([]byte, scramSaltSize)
	if _, err := io.ReadFull(random, salt); err != nil {
		return Value{}, fmt.Errorf("secrets: scram salt: %w", err)
	}
	return scramVerifier(password.Reveal(), salt, scramIterations)
}

func scramVerifier(password, salt []byte, iterations int) (Value, error) {
	salted, err := pbkdf2.Key(sha256.New, string(password), salt, iterations, sha256.Size)
	if err != nil {
		return Value{}, fmt.Errorf("secrets: scram key: %w", err)
	}
	clientKey := hmacSHA256(salted, []byte("Client Key"))
	stored := sha256.Sum256(clientKey)
	server := hmacSHA256(salted, []byte("Server Key"))
	enc := base64.StdEncoding
	text := fmt.Sprintf("SCRAM-SHA-256$%d:%s$%s:%s", iterations, enc.EncodeToString(salt), enc.EncodeToString(stored[:]), enc.EncodeToString(server))
	return NewValue([]byte(text)), nil
}

func hmacSHA256(key, data []byte) []byte {
	mac := hmac.New(sha256.New, key)
	mac.Write(data)
	return mac.Sum(nil)
}
