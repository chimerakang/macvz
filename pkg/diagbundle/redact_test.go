package diagbundle

import (
	"strings"
	"testing"
)

func TestRedactSensitiveKeyValues(t *testing.T) {
	r := DefaultRedactor()
	cases := []struct {
		name string
		in   string
	}{
		{"yaml password", "password: hunter2"},
		{"yaml token quoted", `token: "abc.def.ghi"`},
		{"equals form", "API_KEY=supersecretvalue"},
		{"json token", `"token": "s3cr3t",`},
		{"kubeconfig client-key-data", "    client-key-data: LS0tLS1CRUdJTiالبصمة"},
		{"list item secret", "  - secret: my-value"},
		{"access_key", "access_key = AKIAEXAMPLE12345"},
		{"credentials", "credentials: dock3rcfgjson"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := r.Redact(tc.in)
			if !strings.Contains(got, Placeholder) {
				t.Fatalf("expected redaction, got %q", got)
			}
			// The secret value must be gone.
			for _, secret := range []string{"hunter2", "abc.def.ghi", "supersecretvalue", "s3cr3t", "AKIAEXAMPLE12345", "dock3rcfgjson"} {
				if strings.Contains(got, secret) {
					t.Fatalf("secret leaked: %q in %q", secret, got)
				}
			}
		})
	}
}

func TestRedactPreservesKeyName(t *testing.T) {
	r := DefaultRedactor()
	got := r.Redact("password: hunter2")
	if !strings.HasPrefix(got, "password: ") {
		t.Fatalf("key name should be preserved: %q", got)
	}
	if !strings.Contains(got, Placeholder) {
		t.Fatalf("value should be redacted: %q", got)
	}
}

func TestRedactPEMPrivateKeys(t *testing.T) {
	r := DefaultRedactor()
	for _, header := range []string{"RSA PRIVATE KEY", "PRIVATE KEY", "EC PRIVATE KEY", "OPENSSH PRIVATE KEY"} {
		pem := "-----BEGIN " + header + "-----\nMIIEvQIBADANBgkqhkiG9w0BAQEFAASC\nQUJDREVGT=\n-----END " + header + "-----"
		got := r.Redact("before\n" + pem + "\nafter")
		if strings.Contains(got, "MIIEvQIBADANBgkqhkiG9w0BAQEFAASC") {
			t.Fatalf("PEM body leaked for %s: %q", header, got)
		}
		if !strings.Contains(got, "before") || !strings.Contains(got, "after") {
			t.Fatalf("surrounding text dropped for %s: %q", header, got)
		}
	}
}

func TestRedactJWT(t *testing.T) {
	r := DefaultRedactor()
	jwt := "eyJhbGciOiJSUzI1NiIsImtpZCI6IngifQ.eyJzdWIiOiJzeXN0ZW0ifQ.c2lnbmF0dXJlX2hlcmU"
	got := r.Redact("token is " + jwt + " end")
	if strings.Contains(got, jwt) {
		t.Fatalf("JWT leaked: %q", got)
	}
	if !strings.Contains(got, "end") {
		t.Fatalf("trailing text dropped: %q", got)
	}
}

func TestRedactBearer(t *testing.T) {
	r := DefaultRedactor()
	// An inline bearer token (not on an "authorization:"-keyed line) keeps the
	// keyword and redacts only the credential.
	got := r.Redact("curl -H 'Bearer abc123XYZ.token-value' https://api")
	if strings.Contains(got, "abc123XYZ.token-value") {
		t.Fatalf("bearer token leaked: %q", got)
	}
	if !strings.Contains(strings.ToLower(got), "bearer") {
		t.Fatalf("bearer keyword should remain: %q", got)
	}

	// On an Authorization-keyed line, the whole value is stripped (more thorough).
	keyed := r.Redact("Authorization: Bearer abc123XYZ.token-value")
	if strings.Contains(keyed, "abc123XYZ.token-value") {
		t.Fatalf("keyed bearer leaked: %q", keyed)
	}
}

func TestRedactWireGuardKeys(t *testing.T) {
	r := DefaultRedactor()
	cfg := strings.Join([]string{
		"[Interface]",
		"PrivateKey = aP1vateK3yMaterialBase64EncodedValueXX=",
		"ListenPort = 51820",
		"[Peer]",
		"PublicKey = pUbl1cK3yMaterialBase64EncodedValueXXXX=",
		"PresharedKey = pr3shar3dK3yMaterialBase64EncodedValue=",
		"AllowedIPs = 10.99.0.2/32",
	}, "\n")
	got := r.Redact(cfg)

	if strings.Contains(got, "aP1vateK3yMaterialBase64EncodedValueXX=") {
		t.Fatalf("WireGuard private key leaked: %q", got)
	}
	if strings.Contains(got, "pr3shar3dK3yMaterialBase64EncodedValue=") {
		t.Fatalf("WireGuard preshared key leaked: %q", got)
	}
	// Public material and routing info must be preserved for debugging.
	if !strings.Contains(got, "pUbl1cK3yMaterialBase64EncodedValueXXXX=") {
		t.Fatalf("public key should NOT be redacted: %q", got)
	}
	if !strings.Contains(got, "AllowedIPs = 10.99.0.2/32") {
		t.Fatalf("AllowedIPs should be preserved: %q", got)
	}
	if !strings.Contains(got, "ListenPort = 51820") {
		t.Fatalf("ListenPort should be preserved: %q", got)
	}
}

func TestRedactDoesNotOverRedactPublicMaterial(t *testing.T) {
	r := DefaultRedactor()
	in := strings.Join([]string{
		"certificate-authority-data: LS0tLS1QVUJMSUM=",
		"client-certificate-data: LS0tLS1QVUJMSUM=",
		"PublicKey = pUbl1cK3yMaterialXXXX=",
		"server: https://10.0.0.1:6443",
		"internalIP: 10.0.0.5",
		"node-name: mac-a",
	}, "\n")
	got := r.Redact(in)
	if strings.Contains(got, Placeholder) {
		t.Fatalf("public/non-secret material was redacted: %q", got)
	}
}

func TestRedactIsIdempotent(t *testing.T) {
	r := DefaultRedactor()
	once := r.Redact("password: hunter2\nPrivateKey = abcdef==")
	twice := r.Redact(once)
	if once != twice {
		t.Fatalf("redaction not idempotent:\n once: %q\ntwice: %q", once, twice)
	}
}

func TestRedactBytes(t *testing.T) {
	r := DefaultRedactor()
	got := r.RedactBytes([]byte("token: abc"))
	if strings.Contains(string(got), "abc") {
		t.Fatalf("RedactBytes leaked: %q", got)
	}
}

func TestRedactCaseInsensitiveKeys(t *testing.T) {
	r := DefaultRedactor()
	for _, in := range []string{"PASSWORD: x1", "Token: y2", "Api_Key: z3"} {
		if got := r.Redact(in); strings.ContainsAny(got, "123") {
			t.Fatalf("case-insensitive redaction missed: %q -> %q", in, got)
		}
	}
}
