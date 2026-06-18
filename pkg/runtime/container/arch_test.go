package container

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/internal/types"
	"github.com/chimerakang/macvz/pkg/runtime"
)

// rosettaDriver returns a driver with Rosetta translation enabled.
func rosettaDriver(f *fakeRunner) *Driver { return &Driver{run: f, rosetta: true} }

func TestSelectPlatformPrefersArm64(t *testing.T) {
	// arm64Variants advertises both amd64 and arm64; arm64 must win, no Rosetta.
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte(arm64Variants)}}
	choice, err := rosettaDriver(f).selectPlatform(context.Background(), "img")
	if err != nil {
		t.Fatalf("selectPlatform: %v", err)
	}
	if choice.platform != "linux/arm64" || choice.rosetta {
		t.Errorf("choice = %+v, want linux/arm64 without Rosetta", choice)
	}
}

func TestSelectPlatformAMD64ViaRosetta(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte(amd64OnlyVariants)}}
	choice, err := rosettaDriver(f).selectPlatform(context.Background(), "amd64/img")
	if err != nil {
		t.Fatalf("selectPlatform: %v", err)
	}
	if choice.platform != "linux/amd64" || !choice.rosetta {
		t.Errorf("choice = %+v, want linux/amd64 with Rosetta", choice)
	}
}

func TestSelectPlatformAMD64RejectedWithoutRosetta(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte(amd64OnlyVariants)}}
	_, err := driverWith(f).selectPlatform(context.Background(), "amd64/img")
	if !errors.Is(err, runtime.ErrIncompatibleArch) {
		t.Fatalf("err = %v, want ErrIncompatibleArch", err)
	}
	// The message must point the operator at the Rosetta opt-in.
	if msg := err.Error(); !strings.Contains(msg, "runtimeRosetta") || !strings.Contains(msg, "linux/amd64") {
		t.Errorf("error not actionable: %q", msg)
	}
}

func TestPullAcceptsAMD64WhenRosettaEnabled(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{
		"image pull":    []byte(""),
		"image inspect": []byte(amd64OnlyVariants),
	}}
	if err := rosettaDriver(f).Pull(context.Background(), "amd64/img"); err != nil {
		t.Fatalf("Pull of amd64 image with Rosetta enabled: %v", err)
	}
}

func TestCreateRosettaEmitsPlatformAndRosettaFlags(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{
		"image inspect": []byte(amd64OnlyVariants),
		"create":        []byte("pod-x\n"),
	}}
	_, err := rosettaDriver(f).Create(context.Background(), types.ContainerSpec{Name: "pod-x", Image: "amd64/img"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !argsContain(lastCall(f), "create", "--platform", "linux/amd64", "--rosetta") {
		t.Errorf("create args missing platform/rosetta flags: %v", lastCall(f))
	}
}

func TestCreateRosettaArm64SkipsRosettaFlag(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{
		"image inspect": []byte(arm64Variants),
		"create":        []byte("pod-x\n"),
	}}
	_, err := rosettaDriver(f).Create(context.Background(), types.ContainerSpec{Name: "pod-x", Image: "img"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got := strings.Join(lastCall(f), " ")
	if !strings.Contains(got, "--platform linux/arm64") {
		t.Errorf("expected explicit --platform linux/arm64; got %s", got)
	}
	if strings.Contains(got, "--rosetta") {
		t.Errorf("native arm64 must not request Rosetta; got %s", got)
	}
}

func TestCreateWithoutRosettaOmitsPlatform(t *testing.T) {
	// Default driver (Rosetta disabled) must not inspect or pin platform; it
	// relies on the runtime + Pull-time validation, preserving prior behavior.
	f := &fakeRunner{outputs: map[string][]byte{"create": []byte("pod-x\n")}}
	_, err := driverWith(f).Create(context.Background(), types.ContainerSpec{Name: "pod-x", Image: "img"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if strings.Contains(strings.Join(lastCall(f), " "), "--platform") {
		t.Errorf("Rosetta-disabled create should not pin --platform: %v", lastCall(f))
	}
	// And it must not have called image inspect.
	for _, c := range f.calls {
		if len(c) >= 2 && c[0] == "image" && c[1] == "inspect" {
			t.Errorf("Rosetta-disabled create should not inspect the image; calls=%v", f.calls)
		}
	}
}

func TestCreateRosettaRejectsIncompatibleArch(t *testing.T) {
	// An image with no arm64 and no amd64 variant is rejected even with Rosetta.
	noLinux := `[{"variants":[{"config":{"os":"linux","architecture":"arm","variant":"v7"}}]}]`
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte(noLinux)}}
	_, err := rosettaDriver(f).Create(context.Background(), types.ContainerSpec{Name: "pod-x", Image: "img"})
	if !errors.Is(err, runtime.ErrIncompatibleArch) {
		t.Errorf("err = %v, want ErrIncompatibleArch", err)
	}
}
