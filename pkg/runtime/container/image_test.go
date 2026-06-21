package container

import (
	"context"
	"errors"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
)

// inspectWithDigest is a representative `container image inspect` payload that
// reports a top-level digest and size for a single-arch image.
const inspectWithDigest = `[
  {
    "reference": "docker.io/library/alpine:3.20",
    "name": "docker.io/library/alpine",
    "digest": "sha256:aaaa",
    "size": 5242880,
    "variants": [{"config": {"os": "linux", "architecture": "arm64"}}]
  }
]`

// inspectNoDigest reports no digest; the ID must degrade to the reference.
const inspectNoDigest = `[
  {
    "reference": "docker.io/library/busybox:1.36",
    "name": "docker.io/library/busybox",
    "size": 1048576
  }
]`

func TestImageStatusParsesDigestAndSize(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte(inspectWithDigest)}}
	info, err := driverWith(f).ImageStatus(context.Background(), "docker.io/library/alpine:3.20")
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if !argsContain(lastCall(f), "image", "inspect", "docker.io/library/alpine:3.20") {
		t.Errorf("unexpected args: %v", lastCall(f))
	}
	if info.ID != "docker.io/library/alpine@sha256:aaaa" {
		t.Errorf("ID = %q, want the runtime-usable repo digest", info.ID)
	}
	if info.Size != 5242880 {
		t.Errorf("Size = %d, want 5242880", info.Size)
	}
	if len(info.RepoTags) != 1 || info.RepoTags[0] != "docker.io/library/alpine:3.20" {
		t.Errorf("RepoTags = %v", info.RepoTags)
	}
	if len(info.RepoDigests) != 2 || info.RepoDigests[0] != "docker.io/library/alpine@sha256:aaaa" || info.RepoDigests[1] != "sha256:aaaa" {
		t.Errorf("RepoDigests = %v, want name@digest plus raw digest", info.RepoDigests)
	}
}

func TestImageStatusDegradesIDToReferenceWithoutDigest(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte(inspectNoDigest)}}
	info, err := driverWith(f).ImageStatus(context.Background(), "docker.io/library/busybox:1.36")
	if err != nil {
		t.Fatalf("ImageStatus: %v", err)
	}
	if info.ID != "docker.io/library/busybox:1.36" {
		t.Errorf("ID = %q, want the reference fallback", info.ID)
	}
	if len(info.RepoDigests) != 0 {
		t.Errorf("RepoDigests = %v, want none without a digest", info.RepoDigests)
	}
}

func TestImageStatusNotFound(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"image inspect": &CommandError{Stderr: "image not found", ExitCode: 1}}}
	_, err := driverWith(f).ImageStatus(context.Background(), "missing:latest")
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestImageStatusEmptyOutputIsNotFound(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image inspect": []byte("[]")}}
	_, err := driverWith(f).ImageStatus(context.Background(), "x:y")
	if !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound for an empty inspect array", err)
	}
}

func TestListImages(t *testing.T) {
	const out = `[
  {"reference": "alpine:3.20", "digest": "sha256:aaaa", "size": 100},
  {"reference": "busybox:1.36", "size": 200}
]`
	f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte(out)}}
	infos, err := driverWith(f).ListImages(context.Background())
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if !argsContain(lastCall(f), "image", "ls", "--format", "json") {
		t.Errorf("unexpected args: %v", lastCall(f))
	}
	if len(infos) != 2 {
		t.Fatalf("got %d images, want 2", len(infos))
	}
	if infos[0].ID != "alpine@sha256:aaaa" || infos[1].ID != "busybox:1.36" {
		t.Errorf("IDs = %q, %q", infos[0].ID, infos[1].ID)
	}
}

func TestListImagesEmptyStore(t *testing.T) {
	for _, out := range []string{"", "null", "[]"} {
		f := &fakeRunner{outputs: map[string][]byte{"image ls": []byte(out)}}
		infos, err := driverWith(f).ListImages(context.Background())
		if err != nil {
			t.Fatalf("ListImages(%q): %v", out, err)
		}
		if len(infos) != 0 {
			t.Errorf("ListImages(%q) = %v, want empty", out, infos)
		}
	}
}

func TestRemoveImage(t *testing.T) {
	f := &fakeRunner{outputs: map[string][]byte{"image delete": []byte("")}}
	if err := driverWith(f).RemoveImage(context.Background(), "alpine:3.20"); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if !argsContain(lastCall(f), "image", "delete", "alpine:3.20") {
		t.Errorf("unexpected args: %v", lastCall(f))
	}
}

func TestRemoveImageIdempotentOnNotFound(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"image delete": &CommandError{Stderr: "image not found", ExitCode: 1}}}
	if err := driverWith(f).RemoveImage(context.Background(), "ghost:latest"); err != nil {
		t.Fatalf("RemoveImage on a missing image should succeed, got %v", err)
	}
}

func TestRemoveImagePropagatesRealError(t *testing.T) {
	f := &fakeRunner{errs: map[string]error{"image delete": &CommandError{Stderr: "image is in use", ExitCode: 1}}}
	if err := driverWith(f).RemoveImage(context.Background(), "busy:latest"); err == nil {
		t.Fatal("expected an error when the runtime refuses the delete")
	}
}

func TestRepoNameStripsTagNotRegistryPort(t *testing.T) {
	cases := map[string]string{
		"alpine:3.20":                  "alpine",
		"docker.io/library/alpine:3.2": "docker.io/library/alpine",
		"localhost:5000/img:v1":        "localhost:5000/img",
		"localhost:5000/img":           "localhost:5000/img",
		"alpine":                       "alpine",
		"img@sha256:abc":               "img",
	}
	for in, want := range cases {
		if got := repoName(in); got != want {
			t.Errorf("repoName(%q) = %q, want %q", in, got, want)
		}
	}
}
