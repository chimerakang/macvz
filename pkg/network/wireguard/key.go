package wireguard

import (
	"crypto/rand"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/crypto/curve25519"
)

// KeyLen is the length of a WireGuard key in bytes (Curve25519).
const KeyLen = 32

// Key is a WireGuard Curve25519 key (private or public). Its string form is the
// standard base64 encoding used by the `wg` tool and config files.
type Key [KeyLen]byte

// GenerateKey returns a new, clamped WireGuard private key.
func GenerateKey() (Key, error) {
	var k Key
	if _, err := rand.Read(k[:]); err != nil {
		return Key{}, fmt.Errorf("wireguard: read random bytes: %w", err)
	}
	clamp(&k)
	return k, nil
}

// clamp applies the Curve25519 private-key clamping WireGuard expects, so a key
// generated here behaves identically to one produced by `wg genkey`.
func clamp(k *Key) {
	k[0] &= 248
	k[31] &= 127
	k[31] |= 64
}

// PublicKey derives the public key for a private key.
func (k Key) PublicKey() Key {
	var pub, priv [KeyLen]byte
	copy(priv[:], k[:])
	// curve25519.ScalarBaseMult computes pub = priv * basepoint.
	curve25519.ScalarBaseMult(&pub, &priv)
	return Key(pub)
}

// String returns the base64 encoding of the key, as written to config and shown
// by `wg`.
func (k Key) String() string {
	return base64.StdEncoding.EncodeToString(k[:])
}

// IsZero reports whether the key is the all-zero value (i.e. unset).
func (k Key) IsZero() bool {
	return k == Key{}
}

// ParseKey decodes a base64-encoded WireGuard key.
func ParseKey(s string) (Key, error) {
	b, err := base64.StdEncoding.DecodeString(strings.TrimSpace(s))
	if err != nil {
		return Key{}, fmt.Errorf("wireguard: decode key: %w", err)
	}
	if len(b) != KeyLen {
		return Key{}, fmt.Errorf("wireguard: key is %d bytes, want %d", len(b), KeyLen)
	}
	var k Key
	copy(k[:], b)
	return k, nil
}

// LoadOrCreateKey reads a base64 private key from path, generating and
// persisting a new one (mode 0600) if the file does not exist. This gives each
// node a stable identity across restarts without hardcoding keys in config.
func LoadOrCreateKey(path string) (Key, error) {
	data, err := os.ReadFile(path)
	switch {
	case err == nil:
		k, perr := ParseKey(string(data))
		if perr != nil {
			return Key{}, fmt.Errorf("wireguard: parse key file %q: %w", path, perr)
		}
		return k, nil
	case !os.IsNotExist(err):
		return Key{}, fmt.Errorf("wireguard: read key file %q: %w", path, err)
	}

	// Not present: generate and persist a new private key.
	k, err := GenerateKey()
	if err != nil {
		return Key{}, err
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return Key{}, fmt.Errorf("wireguard: create key dir %q: %w", dir, err)
		}
	}
	if err := os.WriteFile(path, []byte(k.String()+"\n"), 0o600); err != nil {
		return Key{}, fmt.Errorf("wireguard: write key file %q: %w", path, err)
	}
	return k, nil
}
