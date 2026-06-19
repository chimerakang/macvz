// Package diagbundle produces a local diagnostic bundle for MacVz support and
// bug reports (#59): it collects a node's config summary, runtime/helper/mesh
// state, routes, pf anchor rules, logs, and recent events, redacts every secret
// it can recognise, and packages the result into a timestamped directory and
// tar.gz archive that is safe to attach to a GitHub issue.
//
// Redaction is the security boundary of this package. Every byte a source
// produces passes through the Redactor before it is written to disk, so a new
// source cannot accidentally leak a credential: the chokepoint is the Builder,
// not the individual collectors. The Redactor is conservative — it strips PEM
// private keys, WireGuard private/preshared keys, JWT/bearer tokens, and the
// values of a curated set of sensitive configuration keys — and is exercised by
// dedicated unit tests, since a redaction miss is a credential leak.
package diagbundle

import (
	"regexp"
	"strings"
)

// Placeholder replaces any redacted value, so a reviewer can see that a secret
// was present (and where) without seeing the secret itself.
const Placeholder = "[REDACTED]"

// DefaultSensitiveKeys is the curated set of configuration/JSON/env key names
// whose values are always redacted, case-insensitively. The list names the
// secret-bearing keys that appear in MacVz config, kubeconfig, apple/container
// inspect output, and Kubernetes objects. Public material (certificates,
// public keys, CA data) is deliberately absent so the bundle keeps the context
// needed to debug TLS/identity issues.
var DefaultSensitiveKeys = []string{
	"password",
	"passwd",
	"passphrase",
	"token",
	"secret",
	"secretkey",
	"secret_key",
	"secret-key",
	"accesskey",
	"access_key",
	"access-key",
	"apikey",
	"api_key",
	"api-key",
	"privatekey",
	"private_key",
	"private-key",
	"client-key-data",
	"presharedkey",
	"preshared_key",
	"authorization",
	"bearer",
	"sessionkey",
	"session_key",
	"credentials",
}

// Redactor removes secrets from collected text. The zero value is not usable;
// build one with NewRedactor or DefaultRedactor.
type Redactor struct {
	// keyValue matches a "sensitive-key: value" or "sensitive-key=value" line
	// and captures the key+separator so only the value is replaced.
	keyValue *regexp.Regexp
	// pemBlock matches a full PEM private-key block (any "* PRIVATE KEY" type,
	// including OPENSSH), across multiple lines.
	pemBlock *regexp.Regexp
	// jwt matches a three-segment JSON Web Token (e.g. a ServiceAccount token).
	jwt *regexp.Regexp
	// bearer matches an "Authorization: Bearer <token>" / "Bearer <token>" value.
	bearer *regexp.Regexp
	// wgKey matches WireGuard PrivateKey/PresharedKey assignment lines.
	wgKey *regexp.Regexp
}

// DefaultRedactor returns a Redactor configured with DefaultSensitiveKeys.
func DefaultRedactor() *Redactor {
	return NewRedactor(DefaultSensitiveKeys)
}

// NewRedactor builds a Redactor that redacts the values of the given sensitive
// key names (case-insensitive) in addition to the structural secret patterns
// (PEM private keys, JWT/bearer tokens, WireGuard keys) that are always
// stripped regardless of the key list.
func NewRedactor(sensitiveKeys []string) *Redactor {
	alternation := make([]string, 0, len(sensitiveKeys))
	for _, k := range sensitiveKeys {
		if k == "" {
			continue
		}
		alternation = append(alternation, regexp.QuoteMeta(k))
	}
	keys := strings.Join(alternation, "|")

	// A sensitive key at a word boundary (so "publickey" is not matched by "key"),
	// an optional closing quote, a ':' or '=' separator, then the value to the end
	// of the line. The key and separator are captured (group 1) so only the value
	// is replaced. Not anchored to line start, so inline forms like {"token": "x"}
	// are caught too; the value is redacted to end-of-line, which over-redacts a
	// trailing inline field but never under-redacts.
	keyValue := regexp.MustCompile(`(?im)(\b(?:` + keys + `)["']?\s*[:=]\s*)\S.*$`)

	return &Redactor{
		keyValue: keyValue,
		pemBlock: regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`),
		jwt:      regexp.MustCompile(`eyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}`),
		bearer:   regexp.MustCompile(`(?i)(bearer\s+)[A-Za-z0-9._\-+/=]+`),
		wgKey:    regexp.MustCompile(`(?im)^(\s*(?:PrivateKey|PresharedKey)\s*=\s*).+$`),
	}
}

// Redact returns s with every recognised secret replaced by Placeholder. It is
// idempotent: redacting already-redacted text is a no-op, so it is safe to run
// over output that may already contain placeholders.
func (r *Redactor) Redact(s string) string {
	// Structural multi-line secrets first, before the line-based passes.
	s = r.pemBlock.ReplaceAllString(s, Placeholder)

	// WireGuard private/preshared keys (keep the assignment, drop the value).
	s = r.wgKey.ReplaceAllString(s, "${1}"+Placeholder)

	// Sensitive key/value pairs (config, kubeconfig, JSON, env).
	s = r.keyValue.ReplaceAllString(s, "${1}"+Placeholder)

	// Inline tokens that can appear anywhere, not just as a keyed value.
	s = r.bearer.ReplaceAllString(s, "${1}"+Placeholder)
	s = r.jwt.ReplaceAllString(s, Placeholder)

	return s
}

// RedactBytes is the []byte convenience form of Redact.
func (r *Redactor) RedactBytes(b []byte) []byte {
	return []byte(r.Redact(string(b)))
}
