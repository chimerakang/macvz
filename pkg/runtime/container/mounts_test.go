package container

import (
	"context"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/internal/types"
)

func TestCreateBuildsMountArgs(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"create": []byte("pod-x\n")}}
	d := driverWith(f)

	spec := types.ContainerSpec{
		Name:  "pod-x",
		Image: "docker.io/library/alpine:3.20",
		Mounts: []types.Mount{
			{Source: "/srv/data", Target: "/data", ReadOnly: true},
			{Source: "/var/lib/macvz/volumes/uid/scratch", Target: "/scratch"},
			{Target: "/cache", Tmpfs: true},
		},
	}
	if _, err := d.Create(context.Background(), spec); err != nil {
		t.Fatalf("Create: %v", err)
	}
	joined := strings.Join(lastCall(f), " ")

	for _, want := range []string{
		"--volume /srv/data:/data:ro",
		"--volume /var/lib/macvz/volumes/uid/scratch:/scratch",
		"--tmpfs /cache",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args missing %q; got %s", want, joined)
		}
	}
	// A read-write bind must not carry the :ro suffix.
	if strings.Contains(joined, "/scratch:ro") {
		t.Errorf("read-write mount should not be :ro; got %s", joined)
	}
	// Mounts precede the image.
	if mi, ii := strings.Index(joined, "--volume"), strings.Index(joined, "alpine"); mi == -1 || mi > ii {
		t.Errorf("mounts must precede image; got %s", joined)
	}
}
