package criserver

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// This file holds the CRI-L8-4 (#143) hermetic coverage for image lifecycle
// behavior on the apple/container ImageService path: pull error mapping (missing
// image, unsupported architecture), pull-by-digest resolution, and cache-reuse
// idempotency. The runtime-level pull/inspect mechanics are covered in
// pkg/runtime/container; here we assert the CRI gRPC contract kubelet relies on.

// TestPullImageMapsMissingImageToNotFound proves a registry "image not found"
// surfaces as codes.NotFound, so kubelet reports ErrImagePull/ImagePullBackOff
// for a typo'd tag rather than a generic Internal error.
func TestPullImageMapsMissingImageToNotFound(t *testing.T) {
	img := newFakeImageRuntime()
	img.pullErr = fmt.Errorf("pull %q: %w", "missing", runtime.ErrNotFound)
	s := imageServer(img)

	_, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "docker.io/library/does-not-exist:latest"},
	})
	if status.Code(err) != codes.NotFound {
		t.Fatalf("err code = %v, want NotFound for a missing image; err=%v", status.Code(err), err)
	}
}

// TestPullImageMapsIncompatibleArchToFailedPrecondition proves an image with no
// bootable arm64/amd64 variant is rejected at pull as FailedPrecondition, with
// the actionable diagnostic text preserved for the operator. macvz verifies the
// bootable variant at Pull (driver.go selectPlatform), so an unsupported-arch
// image must not reach CreateContainer.
func TestPullImageMapsIncompatibleArchToFailedPrecondition(t *testing.T) {
	img := newFakeImageRuntime()
	img.pullErr = fmt.Errorf("pull %q: %w: image has no linux/arm64 variant (found: linux/s390x); macvz boots arm64 micro-VMs on Apple Silicon",
		"ppc", runtime.ErrIncompatibleArch)
	s := imageServer(img)

	_, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: "docker.io/s390x/alpine:3.20"},
	})
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err code = %v, want FailedPrecondition for an unsupported arch; err=%v", status.Code(err), err)
	}
	if msg := status.Convert(err).Message(); !strings.Contains(msg, "linux/arm64") {
		t.Errorf("arch error should name the missing target arch so the operator can fix the image; got %q", msg)
	}
}

// TestPullImageByDigestResolvesDigestRef proves a pull-by-digest returns a
// runtime-usable image ref (the resolved ID), so kubelet records and later feeds
// back a stable reference for CreateContainer/RemoveImage.
func TestPullImageByDigestResolvesDigestRef(t *testing.T) {
	const digestRef = "docker.io/library/alpine@sha256:1234"
	img := newFakeImageRuntime()
	img.images[digestRef] = runtime.ImageInfo{
		ID:          "docker.io/library/alpine@sha256:1234",
		RepoDigests: []string{"docker.io/library/alpine@sha256:1234", "sha256:1234"},
	}
	s := imageServer(img)

	resp, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: digestRef},
	})
	if err != nil {
		t.Fatalf("PullImage(by digest): %v", err)
	}
	if resp.GetImageRef() != "docker.io/library/alpine@sha256:1234" {
		t.Errorf("ImageRef = %q, want the resolved digest ref", resp.GetImageRef())
	}
}

// TestPullImageCacheReuseIsIdempotent proves a second pull of an already-cached
// image stays valid and resolves to the same image ID. kubelet pulls per Pod
// admission with imagePullPolicy=IfNotPresent; a cache hit must not change the
// recorded ref or duplicate the ListImages entry.
func TestPullImageCacheReuseIsIdempotent(t *testing.T) {
	const ref = "docker.io/library/busybox:1.36"
	img := newFakeImageRuntime()
	s := imageServer(img)

	first, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: ref},
	})
	if err != nil {
		t.Fatalf("PullImage(first): %v", err)
	}
	second, err := s.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: ref},
	})
	if err != nil {
		t.Fatalf("PullImage(second, cache hit): %v", err)
	}
	if first.GetImageRef() != second.GetImageRef() {
		t.Errorf("cache-hit ref drift: first=%q second=%q", first.GetImageRef(), second.GetImageRef())
	}

	list, err := s.ListImages(context.Background(), &runtimeapi.ListImagesRequest{
		Filter: &runtimeapi.ImageFilter{Image: &runtimeapi.ImageSpec{Image: ref}},
	})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	if n := len(list.GetImages()); n != 1 {
		t.Errorf("ListImages after repeated pull = %d images, want 1 (no duplicate cache entry)", n)
	}
}
