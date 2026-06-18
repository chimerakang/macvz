package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadMissingPathReturnsDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err != nil {
		t.Fatalf("expected no error for missing file, got %v", err)
	}
	if cfg.RuntimeSocket == "" {
		t.Fatal("expected default RuntimeSocket to be set")
	}
}

func TestLoadOverridesDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	const body = "nodeName: mac-mini-01\nlogLevel: debug\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(p)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.NodeName != "mac-mini-01" {
		t.Errorf("NodeName = %q, want mac-mini-01", cfg.NodeName)
	}
	if cfg.LogLevel != "debug" {
		t.Errorf("LogLevel = %q, want debug", cfg.LogLevel)
	}
	// Unspecified field keeps its default.
	if cfg.RuntimeSocket == "" {
		t.Error("RuntimeSocket should retain its default when not overridden")
	}
}

func TestValidate(t *testing.T) {
	c := Default()
	if err := c.Validate(); err != nil {
		t.Fatalf("default config should validate: %v", err)
	}
	c.RuntimeSocket = ""
	if err := c.Validate(); err == nil {
		t.Error("expected error when RuntimeSocket is empty")
	}
}
