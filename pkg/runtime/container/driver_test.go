package container

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// fakeRunner records invocations and returns canned results keyed by the first
// argument (the subcommand), so tests assert on argument construction and on
// output/error mapping without a real container service.
type fakeRunner struct {
	calls    [][]string
	outputs  map[string][]byte // keyed by subcommand
	errs     map[string]error  // keyed by subcommand
	pipeRC   io.ReadCloser
	pipeErr  error
	runErr   error
	lastRunS streams
}

// key identifies a canned result. Two-level subcommands (image, system) key on
// their first two tokens so e.g. "image pull" and "image inspect" are distinct.
func key(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) >= 2 && (args[0] == "image" || args[0] == "system") {
		return args[0] + " " + args[1]
	}
	return args[0]
}

func (f *fakeRunner) output(_ context.Context, args ...string) ([]byte, error) {
	f.calls = append(f.calls, args)
	k := key(args)
	if err := f.errs[k]; err != nil {
		return f.outputs[k], err
	}
	return f.outputs[k], nil
}

func (f *fakeRunner) run(_ context.Context, s streams, args ...string) error {
	f.calls = append(f.calls, args)
	f.lastRunS = s
	return f.runErr
}

func (f *fakeRunner) pipe(_ context.Context, args ...string) (io.ReadCloser, error) {
	f.calls = append(f.calls, args)
	return f.pipeRC, f.pipeErr
}

func driverWith(f *fakeRunner) *Driver { return &Driver{run: f} }

func lastCall(f *fakeRunner) []string { return f.calls[len(f.calls)-1] }

func argsContain(args []string, want ...string) bool {
	joined := " " + strings.Join(args, " ") + " "
	for _, w := range want {
		if !strings.Contains(joined, " "+w+" ") {
			return false
		}
	}
	return true
}

func TestCreateBuildsArgs(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"create": []byte("pod-x\n")}}
	d := driverWith(f)

	spec := types.ContainerSpec{
		Name:        "pod-x",
		Image:       "docker.io/library/alpine:3.20",
		Command:     []string{"sleep"},
		Args:        []string{"300"},
		Env:         map[string]string{"B": "2", "A": "1"},
		CPUMillis:   1500,
		MemoryBytes: 512 * 1024 * 1024,
	}
	id, err := d.Create(context.Background(), spec)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if id != "pod-x" {
		t.Errorf("id = %q, want pod-x", id)
	}

	got := lastCall(f)
	if !argsContain(got, "create", "--name", "pod-x", "--cpus", "2", "--memory", "512M") {
		t.Errorf("missing expected args: %v", got)
	}
	// 1500 milli-cores rounds up to 2 vCPUs.
	// Env must be sorted (A before B) and image precede command/args.
	want := "--env A=1 --env B=2"
	if !strings.Contains(strings.Join(got, " "), want) {
		t.Errorf("env not sorted/expected; got %v", got)
	}
	joined := strings.Join(got, " ")
	if idx := strings.Index(joined, "alpine"); idx == -1 || strings.Index(joined, "sleep") < idx {
		t.Errorf("image must precede command; got %v", got)
	}
	if !strings.HasSuffix(joined, "sleep 300") {
		t.Errorf("command+args should trail; got %q", joined)
	}
}

func TestCreateBuildsSecurityContextArgs(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"create": []byte("pod-sc\n")}}
	d := driverWith(f)

	spec := types.ContainerSpec{
		Name:           "pod-sc",
		Image:          "docker.io/library/alpine:3.20",
		User:           "1000:2000",
		ReadOnlyRootFS: true,
		CapAdd:         []string{"CAP_NET_ADMIN"},
		CapDrop:        []string{"ALL"},
	}
	if _, err := d.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := lastCall(f)
	if !argsContain(got, "--user", "1000:2000") {
		t.Errorf("missing --user flag: %v", got)
	}
	if !argsContain(got, "--read-only") {
		t.Errorf("missing --read-only flag: %v", got)
	}
	if !argsContain(got, "--cap-add", "CAP_NET_ADMIN") {
		t.Errorf("missing --cap-add flag: %v", got)
	}
	if !argsContain(got, "--cap-drop", "ALL") {
		t.Errorf("missing --cap-drop flag: %v", got)
	}
}

func TestCreateOmitsSecurityContextArgsByDefault(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"create": []byte("pod-default\n")}}
	d := driverWith(f)
	if _, err := d.Create(context.Background(), types.ContainerSpec{Name: "pod-default", Image: "alpine"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	joined := strings.Join(lastCall(f), " ")
	for _, flag := range []string{"--user", "--read-only", "--cap-add", "--cap-drop"} {
		if strings.Contains(joined, flag) {
			t.Errorf("unset securityContext should not emit %s; got %q", flag, joined)
		}
	}
}

func TestCreateBuildsDNSArgs(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"create": []byte("pod-dns\n")}}
	d := driverWith(f)

	spec := types.ContainerSpec{
		Name:       "pod-dns",
		Image:      "docker.io/library/alpine:3.20",
		DNS:        []string{"10.96.0.10"},
		DNSSearch:  []string{"default.svc.cluster.local", "svc.cluster.local"},
		DNSOptions: []string{"ndots:5"},
	}
	if _, err := d.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := lastCall(f)
	if !argsContain(got, "--dns", "10.96.0.10") {
		t.Errorf("missing --dns flag: %v", got)
	}
	if !argsContain(got, "--dns-search", "default.svc.cluster.local") {
		t.Errorf("missing --dns-search flag: %v", got)
	}
	if !argsContain(got, "--dns-option", "ndots:5") {
		t.Errorf("missing --dns-option flag: %v", got)
	}
}

func TestCreateValidates(t *testing.T) {
	d := driverWith(&fakeRunner{})
	if _, err := d.Create(context.Background(), types.ContainerSpec{Image: "x"}); err == nil {
		t.Error("expected error when Name missing")
	}
	if _, err := d.Create(context.Background(), types.ContainerSpec{Name: "x"}); err == nil {
		t.Error("expected error when Image missing")
	}
}

const arm64Variants = `[{"variants":[
	{"config":{"os":"linux","architecture":"amd64"}},
	{"config":{"os":"unknown","architecture":"unknown"}},
	{"config":{"os":"linux","architecture":"arm64","variant":"v8"}}
]}]`

const amd64OnlyVariants = `[{"variants":[
	{"config":{"os":"linux","architecture":"amd64"}},
	{"config":{"os":"unknown","architecture":"unknown"}}
]}]`

func TestPullVerifiesArm64(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{
		"image pull":    []byte(""),
		"image inspect": []byte(arm64Variants),
	}}
	if err := driverWith(f).Pull(context.Background(), "alpine", nil); err != nil {
		t.Fatalf("Pull of arm64-capable image: %v", err)
	}
}

func TestPullRejectsNonArm64(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{
		"image pull":    []byte(""),
		"image inspect": []byte(amd64OnlyVariants),
	}}
	err := driverWith(f).Pull(context.Background(), "amd64/alpine", nil)
	if !errors.Is(err, runtime.ErrIncompatibleArch) {
		t.Fatalf("err = %v, want ErrIncompatibleArch", err)
	}
	// The message must be actionable: name the missing target and what was found.
	msg := err.Error()
	if !strings.Contains(msg, "linux/arm64") || !strings.Contains(msg, "linux/amd64") {
		t.Errorf("error not actionable enough: %q", msg)
	}
}

func TestPullWithAuthLogsInAndOut(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{
		"image pull":    []byte(""),
		"image inspect": []byte(arm64Variants),
	}}
	auth := &runtime.RegistryAuth{Server: "registry.example.com", Username: "alice", Password: "s3cret"}
	if err := driverWith(f).Pull(context.Background(), "registry.example.com/app:v1", auth); err != nil {
		t.Fatalf("authenticated Pull: %v", err)
	}

	// The full sequence must run: login, pull, inspect (arch check), logout.
	var sawLogin, sawPull, sawLogout bool
	for _, c := range f.calls {
		switch {
		case argsContain(c, "registry", "login"):
			sawLogin = true
			// The password must never appear in the command arguments.
			for _, a := range c {
				if a == "s3cret" {
					t.Error("password leaked into registry login arguments")
				}
			}
			if !argsContain(c, "--password-stdin", "registry.example.com") {
				t.Errorf("login args missing --password-stdin/server: %v", c)
			}
		case argsContain(c, "image", "pull"):
			sawPull = true
		case argsContain(c, "registry", "logout"):
			sawLogout = true
		}
	}
	if !sawLogin || !sawPull || !sawLogout {
		t.Fatalf("sequence incomplete: login=%v pull=%v logout=%v", sawLogin, sawPull, sawLogout)
	}

	// The password must be supplied over stdin.
	if f.lastRunS.Stdin == nil {
		t.Fatal("login did not wire a stdin for the password")
	}
	pw, _ := io.ReadAll(f.lastRunS.Stdin)
	if string(pw) != "s3cret" {
		t.Errorf("stdin password = %q, want %q", pw, "s3cret")
	}
}

func TestPullWithAuthLoginFailureAbortsPull(t *testing.T) {
	f := &fakeRunner{
		outputs: map[string][]byte{"image pull": []byte("")},
		runErr:  errors.New("unauthorized"),
	}
	auth := &runtime.RegistryAuth{Server: "registry.example.com", Username: "alice", Password: "p"}
	err := driverWith(f).Pull(context.Background(), "registry.example.com/app", auth)
	if err == nil {
		t.Fatal("expected Pull to fail when registry login fails")
	}
	for _, c := range f.calls {
		if argsContain(c, "image", "pull") {
			t.Error("image pull ran despite a failed login")
		}
	}
}

func TestCreateMapsIncompatibleArch(t *testing.T) {
	// apple/container auto-pulls on create; an arm64-less image fails with a
	// cryptic platform message that must map to ErrIncompatibleArch.
	f := &fakeRunner{errs: map[string]error{"create": &CommandError{
		Args: []string{"create"}, ExitCode: 1, Stderr: "Error: platform linux/arm64",
	}}}
	_, err := driverWith(f).Create(context.Background(),
		types.ContainerSpec{Name: "x", Image: "amd64/alpine"})
	if !errors.Is(err, runtime.ErrIncompatibleArch) {
		t.Errorf("err = %v, want ErrIncompatibleArch", err)
	}
}

func TestStopUsesTimeoutSeconds(t *testing.T) {
	f := &fakeRunner{}
	d := driverWith(f)
	if err := d.Stop(context.Background(), "pod-x", 3*time.Second); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !argsContain(lastCall(f), "stop", "--time", "3", "pod-x") {
		t.Errorf("unexpected stop args: %v", lastCall(f))
	}
}

func TestExecWiresFlagsAndStreams(t *testing.T) {
	f := &fakeRunner{}
	d := driverWith(f)
	in := strings.NewReader("hi")
	err := d.Exec(context.Background(), "pod-x", []string{"echo", "hello"},
		runtime.ExecIO{Stdin: in, TTY: true})
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	got := lastCall(f)
	if !argsContain(got, "exec", "--tty", "--interactive", "pod-x", "echo", "hello") {
		t.Errorf("unexpected exec args: %v", got)
	}
	if f.lastRunS.Stdin != in {
		t.Error("stdin not wired through")
	}
}

func TestExecEmptyCommand(t *testing.T) {
	d := driverWith(&fakeRunner{})
	if err := d.Exec(context.Background(), "pod-x", nil, runtime.ExecIO{}); err == nil {
		t.Error("expected error for empty command")
	}
}

func TestExecPropagatesExitCode(t *testing.T) {
	f := &fakeRunner{runErr: &CommandError{
		Args: []string{"exec"}, ExitCode: 7, Stderr: "",
	}}
	err := driverWith(f).Exec(context.Background(), "pod-x", []string{"sh", "-c", "exit 7"}, runtime.ExecIO{})
	var ee *runtime.ExitError
	if !errors.As(err, &ee) {
		t.Fatalf("err = %v, want *runtime.ExitError", err)
	}
	if ee.Code != 7 {
		t.Errorf("exit code = %d, want 7", ee.Code)
	}
}

func TestExecMapsStartFailures(t *testing.T) {
	cases := []struct {
		name   string
		stderr string
		want   error
	}{
		{"missing", "Error: get failed: container pod-x not found", runtime.ErrNotFound},
		{"stopped", "Error: container pod-x is not running", runtime.ErrNotRunning},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeRunner{runErr: &CommandError{
				Args: []string{"exec"}, ExitCode: 1, Stderr: tc.stderr,
			}}
			err := driverWith(f).Exec(context.Background(), "pod-x", []string{"echo", "hi"}, runtime.ExecIO{})
			if !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			// A start failure must NOT masquerade as a command ExitError.
			var ee *runtime.ExitError
			if errors.As(err, &ee) {
				t.Errorf("start failure wrongly mapped to ExitError: %v", err)
			}
		})
	}
}

func TestLogsBuildsArgs(t *testing.T) {
	f := &fakeRunner{pipeRC: io.NopCloser(strings.NewReader(""))}
	d := driverWith(f)
	rc, err := d.Logs(context.Background(), "pod-x", runtime.LogOptions{Follow: true, Tail: 10})
	if err != nil {
		t.Fatalf("Logs: %v", err)
	}
	_ = rc.Close()
	if !argsContain(lastCall(f), "logs", "--follow", "-n", "10", "pod-x") {
		t.Errorf("unexpected logs args: %v", lastCall(f))
	}
}

func TestStatusParsesRunning(t *testing.T) {
	const body = `[{"id":"pod-x","status":{"state":"running","startedDate":"2026-06-18T13:55:56Z","networks":[{"ipv4Address":"192.168.64.3/24"}]}}]`
	f := &fakeRunner{outputs: map[string][]byte{"inspect": []byte(body)}}
	d := driverWith(f)

	st, err := d.Status(context.Background(), "pod-x")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Phase != runtime.PhaseRunning {
		t.Errorf("Phase = %q, want Running", st.Phase)
	}
	if st.IP != "192.168.64.3" {
		t.Errorf("IP = %q, want 192.168.64.3 (CIDR stripped)", st.IP)
	}
	if st.StartedAt.IsZero() {
		t.Error("StartedAt should be parsed")
	}
}

func TestListBuildsArgsAndParsesStatuses(t *testing.T) {
	const body = `[
		{"id":"macvz-default-web-app","status":{"state":"running","startedDate":"2026-06-18T13:55:56Z"}},
		{"id":"macvz-default-done-app","status":{"state":"stopped","startedDate":"2026-06-18T13:56:56Z","exitStatus":3}}
	]`
	f := &fakeRunner{outputs: map[string][]byte{"list": []byte(body)}}
	statuses, err := driverWith(f).List(context.Background())
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if !argsContain(lastCall(f), "list", "--all", "--format", "json") {
		t.Fatalf("List args = %v, want list --all --format json", lastCall(f))
	}
	if len(statuses) != 2 {
		t.Fatalf("got %d statuses, want 2: %+v", len(statuses), statuses)
	}
	if statuses[0].ID != "macvz-default-web-app" || statuses[0].Phase != runtime.PhaseRunning {
		t.Fatalf("status[0] = %+v, want running web app", statuses[0])
	}
	if statuses[1].ID != "macvz-default-done-app" || statuses[1].Phase != runtime.PhaseFailed || statuses[1].ExitCode != 3 {
		t.Fatalf("status[1] = %+v, want failed done app exit 3", statuses[1])
	}
}

func TestStatusCreatedVsStopped(t *testing.T) {
	created := `[{"id":"x","status":{"state":"stopped","networks":[]}}]`
	stopped := `[{"id":"x","status":{"state":"stopped","startedDate":"2026-06-18T13:55:56Z","networks":[]}}]`

	if p := mustPhase(t, created); p != runtime.PhaseCreated {
		t.Errorf("never-started stopped should map to Created, got %q", p)
	}
	if p := mustPhase(t, stopped); p != runtime.PhaseStopped {
		t.Errorf("ran-then-stopped should map to Stopped, got %q", p)
	}
}

func TestStatusParsesNonZeroExitCode(t *testing.T) {
	body := `[{"id":"x","status":{"state":"stopped","startedDate":"2026-06-18T13:55:56Z","exitCode":7,"networks":[]}}]`
	f := &fakeRunner{outputs: map[string][]byte{"inspect": []byte(body)}}
	st, err := driverWith(f).Status(context.Background(), "x")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Phase != runtime.PhaseFailed {
		t.Errorf("Phase = %q, want Failed", st.Phase)
	}
	if st.ExitCode != 7 {
		t.Errorf("ExitCode = %d, want 7", st.ExitCode)
	}
}

func TestStatusParsesExitStatusAlias(t *testing.T) {
	body := `[{"id":"x","status":{"state":"stopped","startedDate":"2026-06-18T13:55:56Z","exitStatus":3,"networks":[]}}]`
	f := &fakeRunner{outputs: map[string][]byte{"inspect": []byte(body)}}
	st, err := driverWith(f).Status(context.Background(), "x")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	if st.Phase != runtime.PhaseFailed || st.ExitCode != 3 {
		t.Errorf("status = phase %q exit %d, want Failed/3", st.Phase, st.ExitCode)
	}
}

func mustPhase(t *testing.T, body string) runtime.Phase {
	t.Helper()
	f := &fakeRunner{outputs: map[string][]byte{"inspect": []byte(body)}}
	st, err := driverWith(f).Status(context.Background(), "x")
	if err != nil {
		t.Fatalf("Status: %v", err)
	}
	return st.Phase
}

func TestStatusNotFoundMapsTyped(t *testing.T) {
	f := &fakeRunner{
		errs: map[string]error{"inspect": &CommandError{
			Args: []string{"inspect", "x"}, ExitCode: 1, Stderr: "Error: container not found: x",
		}},
	}
	d := driverWith(f)
	_, err := d.Status(context.Background(), "x")
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Errorf("want ErrNotFound, got %v", err)
	}
}

func TestCreateIdempotentOnExisting(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"create": &CommandError{
		Args: []string{"create"}, ExitCode: 1,
		Stderr: `failed to create container (cause: "exists: "container already exists: pod-x"")`,
	}}}
	id, err := driverWith(f).Create(context.Background(),
		types.ContainerSpec{Name: "pod-x", Image: "alpine"})
	if err != nil {
		t.Fatalf("Create on existing should be idempotent, got %v", err)
	}
	if id != "pod-x" {
		t.Errorf("id = %q, want pod-x", id)
	}
}

func TestStopIdempotentOnMissing(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"stop": &CommandError{
		Args: []string{"stop"}, ExitCode: 1, Stderr: "container with ID pod-x not found",
	}}}
	if err := driverWith(f).Stop(context.Background(), "pod-x", time.Second); err != nil {
		t.Errorf("Stop on missing should be a no-op, got %v", err)
	}
}

func TestDestroyIdempotentOnMissing(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"delete": &CommandError{
		Args: []string{"delete"}, ExitCode: 1, Stderr: "container with ID pod-x not found",
	}}}
	if err := driverWith(f).Destroy(context.Background(), "pod-x"); err != nil {
		t.Errorf("Destroy on missing should be a no-op, got %v", err)
	}
}

func TestDestroyPropagatesRealError(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"delete": &CommandError{
		Args: []string{"delete"}, ExitCode: 1, Stderr: "internalError: disk busy",
	}}}
	if err := driverWith(f).Destroy(context.Background(), "pod-x"); err == nil {
		t.Error("Destroy should propagate non-not-found errors")
	}
}

func TestReadyMapsNotReady(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"system status": &CommandError{
		Args: []string{"system", "status"}, ExitCode: 1, Stderr: "service not running",
	}}}
	if err := driverWith(f).Ready(context.Background()); !errors.Is(err, runtime.ErrNotReady) {
		t.Errorf("want ErrNotReady, got %v", err)
	}

	ok := &fakeRunner{outputs: map[string][]byte{"system status": []byte("status running")}}
	if err := driverWith(ok).Ready(context.Background()); err != nil {
		t.Errorf("Ready should succeed, got %v", err)
	}
}
