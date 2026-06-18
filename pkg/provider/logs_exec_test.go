package provider

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
	"github.com/virtual-kubelet/virtual-kubelet/errdefs"
	vkapi "github.com/virtual-kubelet/virtual-kubelet/node/api"
	utilexec "k8s.io/utils/exec"
)

// --- log/exec hooks on the recording runtime ---

// nopWriteCloser adapts a Writer to io.WriteCloser for AttachIO streams.
type nopWriteCloser struct{ io.Writer }

func (nopWriteCloser) Close() error { return nil }

// fakeAttachIO implements vkapi.AttachIO for exec tests.
type fakeAttachIO struct {
	stdin  io.Reader
	stdout io.WriteCloser
	stderr io.WriteCloser
	tty    bool
	resize chan vkapi.TermSize
}

func newAttachIO(stdin string, tty bool) (*fakeAttachIO, *bytes.Buffer, *bytes.Buffer) {
	out, errb := &bytes.Buffer{}, &bytes.Buffer{}
	return &fakeAttachIO{
		stdin:  strings.NewReader(stdin),
		stdout: nopWriteCloser{out},
		stderr: nopWriteCloser{errb},
		tty:    tty,
		resize: make(chan vkapi.TermSize),
	}, out, errb
}

func (a *fakeAttachIO) Stdin() io.Reader              { return a.stdin }
func (a *fakeAttachIO) Stdout() io.WriteCloser        { return a.stdout }
func (a *fakeAttachIO) Stderr() io.WriteCloser        { return a.stderr }
func (a *fakeAttachIO) TTY() bool                     { return a.tty }
func (a *fakeAttachIO) Resize() <-chan vkapi.TermSize { return a.resize }

func runningProvider(t *testing.T) (*Provider, *recordingRuntime) {
	t.Helper()
	p, rt := newTestProvider()
	if err := p.CreatePod(context.Background(), testPod("web")); err != nil {
		t.Fatalf("CreatePod: %v", err)
	}
	return p, rt
}

func TestGetContainerLogsStreams(t *testing.T) {
	p, rt := runningProvider(t)
	rt.logData = "hello from alpine\n"

	rc, err := p.GetContainerLogs(context.Background(), "default", "p1", "web", vkapi.ContainerLogOpts{Tail: 10})
	if err != nil {
		t.Fatalf("GetContainerLogs: %v", err)
	}
	defer func() { _ = rc.Close() }()
	got, _ := io.ReadAll(rc)
	if string(got) != "hello from alpine\n" {
		t.Errorf("logs = %q, want greeting", string(got))
	}
	if rt.lastLogOpts.Tail != 10 {
		t.Errorf("tail not propagated: %+v", rt.lastLogOpts)
	}
}

func TestGetContainerLogsUnknownPod(t *testing.T) {
	p, _ := newTestProvider()
	_, err := p.GetContainerLogs(context.Background(), "default", "missing", "web", vkapi.ContainerLogOpts{})
	if !errdefs.IsNotFound(err) {
		t.Errorf("expected NotFound for unknown pod, got %v", err)
	}
}

func TestGetContainerLogsUnknownContainer(t *testing.T) {
	p, _ := runningProvider(t)
	_, err := p.GetContainerLogs(context.Background(), "default", "p1", "nope", vkapi.ContainerLogOpts{})
	if !errdefs.IsNotFound(err) {
		t.Errorf("expected NotFound for unknown container, got %v", err)
	}
}

func TestRunInContainerWiresStreams(t *testing.T) {
	p, rt := runningProvider(t)
	rt.execStdout = "stdout text"
	rt.execStderr = "stderr text"

	attach, out, errb := newAttachIO("input", false)
	err := p.RunInContainer(context.Background(), "default", "p1", "web", []string{"sh", "-c", "echo hi"}, attach)
	if err != nil {
		t.Fatalf("RunInContainer: %v", err)
	}
	if out.String() != "stdout text" {
		t.Errorf("stdout = %q, want %q", out.String(), "stdout text")
	}
	if errb.String() != "stderr text" {
		t.Errorf("stderr = %q, want %q", errb.String(), "stderr text")
	}
	if strings.Join(rt.lastExecCmd, " ") != "sh -c echo hi" {
		t.Errorf("cmd = %v, want [sh -c echo hi]", rt.lastExecCmd)
	}
}

func TestRunInContainerNonZeroExit(t *testing.T) {
	p, rt := runningProvider(t)
	rt.execErr = &runtime.ExitError{Code: 7}

	attach, _, _ := newAttachIO("", false)
	err := p.RunInContainer(context.Background(), "default", "p1", "web", []string{"false"}, attach)
	if err == nil {
		t.Fatal("expected an error for non-zero exit")
	}
	exitErr, ok := err.(utilexec.ExitError)
	if !ok {
		t.Fatalf("error %T does not implement utilexec.ExitError", err)
	}
	if !exitErr.Exited() || exitErr.ExitStatus() != 7 {
		t.Errorf("exit status = %d (exited=%v), want 7/true", exitErr.ExitStatus(), exitErr.Exited())
	}
}

func TestRunInContainerUnknownPod(t *testing.T) {
	p, _ := newTestProvider()
	attach, _, _ := newAttachIO("", false)
	err := p.RunInContainer(context.Background(), "default", "missing", "web", []string{"true"}, attach)
	if !errdefs.IsNotFound(err) {
		t.Errorf("expected NotFound for unknown pod, got %v", err)
	}
}
