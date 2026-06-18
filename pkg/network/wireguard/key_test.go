package wireguard

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerateKeyIsClampedAndUnique(t *testing.T) {
	k1, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	k2, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	if k1 == k2 {
		t.Fatal("two generated keys are identical")
	}
	// Clamping invariants from the Curve25519 spec.
	if k1[0]&7 != 0 {
		t.Errorf("low 3 bits of byte 0 not cleared: %08b", k1[0])
	}
	if k1[31]&0x80 != 0 {
		t.Errorf("high bit of byte 31 not cleared: %08b", k1[31])
	}
	if k1[31]&0x40 == 0 {
		t.Errorf("bit 6 of byte 31 not set: %08b", k1[31])
	}
}

func TestKeyStringRoundTrip(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	parsed, err := ParseKey(k.String())
	if err != nil {
		t.Fatalf("ParseKey: %v", err)
	}
	if parsed != k {
		t.Errorf("round trip mismatch: %s != %s", parsed, k)
	}
}

func TestPublicKeyIsStableAndPublic(t *testing.T) {
	k, err := GenerateKey()
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	pub1 := k.PublicKey()
	pub2 := k.PublicKey()
	if pub1 != pub2 {
		t.Error("PublicKey is not deterministic")
	}
	if pub1 == k {
		t.Error("public key equals private key")
	}
	if pub1.IsZero() {
		t.Error("derived public key is zero")
	}
}

func TestParseKeyRejectsBadInput(t *testing.T) {
	for _, s := range []string{"", "not-base64!!", "c2hvcnQ="} { // last decodes to 5 bytes
		if _, err := ParseKey(s); err == nil {
			t.Errorf("ParseKey(%q) = nil error, want failure", s)
		}
	}
}

func TestLoadOrCreateKeyPersists(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "keys", "wg.key")

	created, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (create): %v", err)
	}
	// File must exist with 0600 perms.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file perm = %o, want 600", perm)
	}

	// Second call returns the same key (stable identity across restarts).
	loaded, err := LoadOrCreateKey(path)
	if err != nil {
		t.Fatalf("LoadOrCreateKey (load): %v", err)
	}
	if loaded != created {
		t.Errorf("reloaded key differs: %s != %s", loaded, created)
	}
}

func TestLoadOrCreateKeyRejectsCorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "wg.key")
	if err := os.WriteFile(path, []byte("garbage"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if _, err := LoadOrCreateKey(path); err == nil {
		t.Fatal("expected error for corrupt key file")
	}
}
