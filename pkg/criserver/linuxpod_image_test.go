package criserver

import (
	"context"
	"testing"

	"github.com/chimerakang/macvz/pkg/runtime/linuxpod"
	runtimeapi "k8s.io/cri-api/pkg/apis/runtime/v1"
)

// CRI-L8-4 (#143) hermetic coverage for the LinuxPod-backed minimal ImageService.
//
// On the LinuxPod path the real image pull and arch verification happen inside
// the helper when it stages a rootfs; the adapter's ImageService is a thin record
// that satisfies the kubelet PullImage→CreateContainer→RemoveImage ordering. These
// tests pin that contract: pull-by-tag and pull-by-digest are both recorded and
// queryable, repeated pulls do not duplicate the list/cache entry, a missing image
// reports absent (not an error), and remove is idempotent and reflected in list.

func lpPull(t *testing.T, svc *LinuxPodService, ref string) {
	t.Helper()
	resp, err := svc.PullImage(context.Background(), &runtimeapi.PullImageRequest{
		Image: &runtimeapi.ImageSpec{Image: ref},
	})
	if err != nil {
		t.Fatalf("PullImage(%s): %v", ref, err)
	}
	if resp.GetImageRef() != ref {
		t.Fatalf("PullImage(%s) ref = %q, want the recorded reference", ref, resp.GetImageRef())
	}
}

func lpImagePresent(t *testing.T, svc *LinuxPodService, ref string) bool {
	t.Helper()
	resp, err := svc.ImageStatus(context.Background(), &runtimeapi.ImageStatusRequest{
		Image: &runtimeapi.ImageSpec{Image: ref},
	})
	if err != nil {
		t.Fatalf("ImageStatus(%s): %v", ref, err)
	}
	return resp.GetImage() != nil
}

// TestLinuxPodImagePullByTagAndDigest proves both a tag reference and a digest
// reference are recorded and independently queryable through ImageStatus, and that
// each ImageStatus carries the kubelet-required ID and non-zero size.
func TestLinuxPodImagePullByTagAndDigest(t *testing.T) {
	svc := newLinuxPodTestService(t, linuxpod.NewFakeBackend())

	const tagRef = "docker.io/library/busybox:1.36.1"
	const digestRef = "docker.io/library/busybox@sha256:abc123"
	lpPull(t, svc, tagRef)
	lpPull(t, svc, digestRef)

	for _, ref := range []string{tagRef, digestRef} {
		resp, err := svc.ImageStatus(context.Background(), &runtimeapi.ImageStatusRequest{
			Image: &runtimeapi.ImageSpec{Image: ref},
		})
		if err != nil {
			t.Fatalf("ImageStatus(%s): %v", ref, err)
		}
		img := resp.GetImage()
		if img == nil {
			t.Fatalf("ImageStatus(%s) reported absent after pull", ref)
		}
		if img.GetId() == "" {
			t.Errorf("ImageStatus(%s) ID must be populated for kubelet", ref)
		}
		if img.GetSize() == 0 {
			t.Errorf("ImageStatus(%s) size must be non-zero for kubelet", ref)
		}
	}
}

// TestLinuxPodImagePullReuseIsDeduplicated proves a repeated pull of the same
// reference does not create a duplicate ListImages entry — the cache-hit case
// kubelet drives on every Pod admission with imagePullPolicy=IfNotPresent.
func TestLinuxPodImagePullReuseIsDeduplicated(t *testing.T) {
	svc := newLinuxPodTestService(t, linuxpod.NewFakeBackend())

	const ref = "docker.io/library/busybox:1.36.1"
	lpPull(t, svc, ref)
	lpPull(t, svc, ref)
	lpPull(t, svc, ref)

	list, err := svc.ListImages(context.Background(), &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	count := 0
	for _, img := range list.GetImages() {
		if containsString(img.GetRepoTags(), ref) {
			count++
		}
	}
	if count != 1 {
		t.Errorf("ListImages has %d entries for %q after repeated pulls, want 1", count, ref)
	}
}

// TestLinuxPodImageStatusAbsentIsEmptyNotError proves an unpulled image reports an
// empty (nil image), non-error ImageStatus, so kubelet distinguishes "not present"
// from a runtime failure and proceeds to pull.
func TestLinuxPodImageStatusAbsentIsEmptyNotError(t *testing.T) {
	svc := newLinuxPodTestService(t, linuxpod.NewFakeBackend())

	if lpImagePresent(t, svc, "docker.io/library/never-pulled:latest") {
		t.Error("ImageStatus reported an image that was never pulled")
	}
}

// TestLinuxPodImageRemoveIsIdempotentAndListed proves RemoveImage drops the record
// (reflected in ImageStatus and ListImages) and that removing an absent image
// succeeds — the idempotent GC contract kubelet relies on.
func TestLinuxPodImageRemoveIsIdempotentAndListed(t *testing.T) {
	svc := newLinuxPodTestService(t, linuxpod.NewFakeBackend())
	ctx := context.Background()

	const ref = "docker.io/library/busybox:1.36.1"
	lpPull(t, svc, ref)
	if !lpImagePresent(t, svc, ref) {
		t.Fatal("image absent after pull")
	}

	if _, err := svc.RemoveImage(ctx, &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: ref},
	}); err != nil {
		t.Fatalf("RemoveImage: %v", err)
	}
	if lpImagePresent(t, svc, ref) {
		t.Error("image still present after RemoveImage")
	}

	// Idempotent: removing the now-absent image succeeds.
	if _, err := svc.RemoveImage(ctx, &runtimeapi.RemoveImageRequest{
		Image: &runtimeapi.ImageSpec{Image: ref},
	}); err != nil {
		t.Fatalf("RemoveImage(absent) should be a no-op success: %v", err)
	}

	list, err := svc.ListImages(ctx, &runtimeapi.ListImagesRequest{})
	if err != nil {
		t.Fatalf("ListImages: %v", err)
	}
	for _, img := range list.GetImages() {
		if containsString(img.GetRepoTags(), ref) {
			t.Errorf("ListImages still reports removed image %q", ref)
		}
	}
}
