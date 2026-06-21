package privhelper

import (
	"bytes"
	"fmt"
	"path/filepath"
	"text/template"
)

// Log rotation for the privileged network helper (#69).
//
// The LaunchDaemon runs under KeepAlive and writes to StandardOutPath /
// StandardErrorPath forever; launchd itself never rotates those files, so on a
// long-running node they grow without bound. macOS's native rotation tool is
// newsyslog, driven by drop-in files under /etc/newsyslog.d. The helper installs
// one alongside its plist so its logs are capped from day one, with no separate
// operator step — and removes it on uninstall so nothing is left behind.
//
// newsyslog is invoked periodically by the system job com.apple.newsyslog, so
// rotation is size-checked on that cadence rather than instantly at the size
// boundary; the cap is a ceiling, not an exact trim point.

const (
	// DefaultNewsyslogPath is the drop-in rotation config for the helper's logs.
	DefaultNewsyslogPath = "/etc/newsyslog.d/macvz-netd.conf"
	// defaultLogRotateCount is how many compressed archives newsyslog keeps.
	defaultLogRotateCount = 7
	// defaultLogRotateSizeKB rotates a log once it grows past this many KB (~5 MB).
	defaultLogRotateSizeKB = 5000
)

// newsyslogTemplate renders the drop-in. Columns are the newsyslog.conf format:
// logfilename owner:group mode count size when flags. "when" is * (size-driven
// only) and the J flag bzip2-compresses rotated archives. The daemon runs as
// root, so its logs are root:wheel.
var newsyslogTemplate = template.Must(template.New("newsyslog").Parse(
	`# Managed by macvz-netd (#69). Rotates the privileged network helper's logs so
# a long-running node does not grow them without bound. Reinstall the helper to
# regenerate; macvz-netd uninstall removes it.
# logfilename                 owner:group  mode count size  when flags
{{.StdoutPath}} root:wheel 644 {{.Count}} {{.SizeKB}} * J
{{.StderrPath}} root:wheel 644 {{.Count}} {{.SizeKB}} * J
`))

// rotateCount returns the configured archive count, or the default when unset.
func (c LaunchdConfig) rotateCount() int {
	if c.LogRotateCount > 0 {
		return c.LogRotateCount
	}
	return defaultLogRotateCount
}

// rotateSizeKB returns the configured rotation threshold, or the default when unset.
func (c LaunchdConfig) rotateSizeKB() int {
	if c.LogRotateSizeKB > 0 {
		return c.LogRotateSizeKB
	}
	return defaultLogRotateSizeKB
}

// RenderNewsyslog produces the newsyslog drop-in for this config's log files. It
// is pure (no side effects) so it can be unit-tested and previewed without root.
// It returns an error if the config is not internally consistent.
func (c LaunchdConfig) RenderNewsyslog() (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	var buf bytes.Buffer
	if err := newsyslogTemplate.Execute(&buf, struct {
		StdoutPath, StderrPath string
		Count, SizeKB          int
	}{
		StdoutPath: c.StdoutPath,
		StderrPath: c.StderrPath,
		Count:      c.rotateCount(),
		SizeKB:     c.rotateSizeKB(),
	}); err != nil {
		return "", fmt.Errorf("render newsyslog config: %w", err)
	}
	return buf.String(), nil
}

// newsyslogDir is the directory the drop-in config lives in.
func (c LaunchdConfig) newsyslogDir() string {
	return filepath.Dir(c.NewsyslogPath)
}
