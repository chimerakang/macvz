package podnet

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// command is a single external command invocation.
type command struct {
	// Name is the binary to run, resolved through PATH (e.g. "pfctl", "sysctl").
	Name string
	// Args are the command arguments.
	Args []string
	// Stdin, when non-empty, is fed to the command's standard input. The router
	// uses it to load a pf anchor ruleset via `pfctl ... -f -`, avoiding temp
	// files on disk.
	Stdin string
}

func (c command) String() string {
	return strings.TrimSpace(c.Name + " " + strings.Join(c.Args, " "))
}

// runner executes external commands. It is an interface so the Router can be
// unit-tested against a fake without touching the host packet filter, mirroring
// the runner in pkg/network/wireguard and pkg/runtime/container.
type runner interface {
	run(ctx context.Context, c command) (string, error)
}

// CommandError describes a failed Pod-network command invocation.
type CommandError struct {
	Cmd      string
	ExitCode int
	Stderr   string
	Err      error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	if msg == "" {
		msg = "command failed"
	}
	return fmt.Sprintf("%s: %s", e.Cmd, msg)
}

func (e *CommandError) Unwrap() error { return e.Err }

// cliRunner is the production runner backed by the host's networking tools.
type cliRunner struct{}

func (cliRunner) run(ctx context.Context, c command) (string, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, c.Name, c.Args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if c.Stdin != "" {
		cmd.Stdin = strings.NewReader(c.Stdin)
	}
	if err := cmd.Run(); err != nil {
		ce := &CommandError{Cmd: c.String(), Stderr: stderr.String(), Err: err, ExitCode: -1}
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			ce.ExitCode = exitErr.ExitCode()
		}
		return stdout.String(), ce
	}
	return stdout.String(), nil
}
