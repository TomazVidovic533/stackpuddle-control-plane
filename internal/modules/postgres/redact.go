package postgres

import "regexp"

// redacted replaces what must not leave the driver.
const redacted = "[redacted]"

func verifierPattern() *regexp.Regexp { return regexp.MustCompile(`SCRAM-SHA-256\$[^\s'"]*`) }

// redact removes every SCRAM verifier from probe output before it reaches an error, an outcome or
// evidence. Passwords never reach the driver; the verifier is the one secret form that can echo back
// from the server in an error's statement context.
func redact(text string) string {
	return verifierPattern().ReplaceAllString(text, redacted)
}
