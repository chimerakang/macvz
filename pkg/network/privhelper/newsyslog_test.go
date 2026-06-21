package privhelper

import (
	"context"
	"os"
	"strings"
	"testing"
)

func TestRenderNewsyslog(t *testing.T) {
	cfg := testConfig(t.TempDir())
	out, err := cfg.RenderNewsyslog()
	if err != nil {
		t.Fatalf("RenderNewsyslog: %v", err)
	}
	// Both log files must be covered, each as a size-driven, compressed-archive
	// rotation line owned by root.
	for _, want := range []string{cfg.StdoutPath, cfg.StderrPath} {
		line := findLine(out, want)
		if line == "" {
			t.Fatalf("no rotation line for %q in:\n%s", want, out)
		}
		for _, field := range []string{"root:wheel", "644", "*", "J"} {
			if !strings.Contains(line, field) {
				t.Errorf("rotation line for %q missing %q: %q", want, field, line)
			}
		}
	}
}

func TestRenderNewsyslogHonorsOverrides(t *testing.T) {
	cfg := testConfig(t.TempDir())
	cfg.LogRotateCount = 3
	cfg.LogRotateSizeKB = 100
	out, err := cfg.RenderNewsyslog()
	if err != nil {
		t.Fatalf("RenderNewsyslog: %v", err)
	}
	line := findLine(out, cfg.StdoutPath)
	if !strings.Contains(line, " 3 ") || !strings.Contains(line, " 100 ") {
		t.Errorf("overrides count=3 size=100 not applied: %q", line)
	}
}

func TestRenderNewsyslogDefaults(t *testing.T) {
	cfg := testConfig(t.TempDir())
	out, err := cfg.RenderNewsyslog()
	if err != nil {
		t.Fatalf("RenderNewsyslog: %v", err)
	}
	line := findLine(out, cfg.StdoutPath)
	if !strings.Contains(line, " 7 ") || !strings.Contains(line, " 5000 ") {
		t.Errorf("default count=7 size=5000 not applied: %q", line)
	}
}

// An operator may disable rotation management by clearing NewsyslogPath; install
// must then write no drop-in (the field is simply skipped).
func TestInstallSkipsNewsyslogWhenDisabled(t *testing.T) {
	dir := t.TempDir()
	cfg := testConfig(dir)
	cfg.NewsyslogPath = ""

	src := dir + "/macvz-netd"
	if err := os.WriteFile(src, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	inst := &Installer{Cfg: cfg, run: (&recordingRunner{}).run}
	if err := inst.Install(context.Background(), src); err != nil {
		t.Fatalf("Install: %v", err)
	}
	// Nothing under the default newsyslog dir should have been created.
	if _, err := os.Stat(testConfig(dir).newsyslogDir()); !os.IsNotExist(err) {
		t.Errorf("newsyslog dir should not exist when rotation is disabled, stat err=%v", err)
	}
}

// findLine returns the first line of s containing sub, or "".
func findLine(s, sub string) string {
	for _, line := range strings.Split(s, "\n") {
		if strings.Contains(line, sub) && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			return line
		}
	}
	return ""
}
