package container

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// streams wires standard IO for a streaming command invocation.
type streams struct {
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

// runner executes apple/container CLI commands. It is an interface so the
// Driver can be unit-tested against a fake without a real container service.
type runner interface {
	// output runs the command to completion and returns its stdout. On a
	// non-zero exit it returns a *CommandError carrying stderr and the code.
	output(ctx context.Context, args ...string) ([]byte, error)
	// run executes the command wiring the given streams, blocking until exit.
	run(ctx context.Context, s streams, args ...string) error
	// pipe starts the command and returns a reader over its stdout that the
	// caller closes; closing terminates the process and waits for it.
	pipe(ctx context.Context, args ...string) (io.ReadCloser, error)
}

// CommandError describes a failed apple/container CLI invocation.
type CommandError struct {
	Args     []string
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
	return fmt.Sprintf("container %s: %s", strings.Join(e.Args, " "), msg)
}

func (e *CommandError) Unwrap() error { return e.Err }

// cliRunner is the production runner backed by the `container` binary.
type cliRunner struct {
	bin string
}

func (r *cliRunner) output(ctx context.Context, args ...string) ([]byte, error) {
	var stdout, stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.Bytes(), newCommandError(args, stderr.String(), err)
	}
	return stdout.Bytes(), nil
}

func (r *cliRunner) run(ctx context.Context, s streams, args ...string) error {
	var stderr bytes.Buffer
	cmd := exec.CommandContext(ctx, r.bin, args...)
	cmd.Stdin = s.Stdin
	cmd.Stdout = s.Stdout
	// Tee stderr to the caller (if any) while capturing it for error mapping.
	if s.Stderr != nil {
		cmd.Stderr = io.MultiWriter(s.Stderr, &stderr)
	} else {
		cmd.Stderr = &stderr
	}
	if err := cmd.Run(); err != nil {
		return newCommandError(args, stderr.String(), err)
	}
	return nil
}

func (r *cliRunner) pipe(ctx context.Context, args ...string) (io.ReadCloser, error) {
	cmd := exec.CommandContext(ctx, r.bin, args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("container %s: stdout pipe: %w", strings.Join(args, " "), err)
	}
	// The container's own stdout+stderr arrive multiplexed on the CLI's stdout;
	// the CLI's stderr carries only its own diagnostics, captured (bounded) so a
	// failed invocation surfaces a meaningful error on Close.
	stderr := &cappedBuffer{limit: 4096}
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return nil, newCommandError(args, "", err)
	}
	return &cmdReadCloser{ctx: ctx, cmd: cmd, stdout: stdout, stderr: stderr, args: args}, nil
}

// newCommandError wraps an exec failure, extracting the process exit code.
func newCommandError(args []string, stderr string, err error) *CommandError {
	ce := &CommandError{Args: args, Stderr: stderr, Err: err, ExitCode: -1}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		ce.ExitCode = exitErr.ExitCode()
	}
	return ce
}

// cmdReadCloser couples a streaming command's stdout with its process so that
// Close terminates the process and reaps it, avoiding zombies on early exit.
type cmdReadCloser struct {
	ctx    context.Context
	cmd    *exec.Cmd
	stdout io.ReadCloser
	stderr *cappedBuffer
	args   []string
}

func (c *cmdReadCloser) Read(p []byte) (int, error) { return c.stdout.Read(p) }

// Close stops following and reaps the process. It returns an error only when
// the command exited on its own with a failure (e.g. the workload does not
// exist); a caller-initiated stop or a context cancellation is not an error,
// since the stream simply ended as requested.
func (c *cmdReadCloser) Close() error {
	_ = c.stdout.Close()

	// Best-effort terminate in case we are stopping a still-running follow.
	if c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
	err := c.cmd.Wait()

	// Cancellation ends the stream by request, not in error.
	if c.ctx.Err() != nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) && exitErr.ExitCode() == -1 {
		// Terminated by a signal (our Kill or ctx) rather than its own non-zero
		// exit: the caller stopped following, which is not an error.
		return nil
	}
	if err != nil {
		return mapErr(newCommandError(c.args, c.stderr.String(), err))
	}
	return nil
}

// cappedBuffer accumulates up to limit bytes, dropping the overflow. It bounds
// memory for long-running follows whose CLI process may emit diagnostics.
type cappedBuffer struct {
	buf   []byte
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - len(b.buf); room > 0 {
		take := p
		if len(take) > room {
			take = take[:room]
		}
		b.buf = append(b.buf, take...)
	}
	return len(p), nil
}

func (b *cappedBuffer) String() string { return string(b.buf) }
